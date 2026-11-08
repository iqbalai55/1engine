package dstack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"billing-backend/internal/compute"
	"billing-backend/internal/domain"
)

type ObservationCredentials interface {
	CustomerCredential(context.Context, domain.TenantID, []byte) (string, error)
}

type ObservationTenantSource interface {
	Get(context.Context, domain.TenantID) (domain.Tenant, error)
}

type ObservationClient struct {
	BaseURL       string
	Credentials   ObservationCredentials
	Tenants       ObservationTenantSource
	EncryptionKey []byte
	HTTP          *http.Client
}

func (c *ObservationClient) ListTenantRuns(ctx context.Context, tenant domain.TenantID, previousAt *time.Time, previousID string, now time.Time) (compute.ObservationPage, error) {
	return c.listTenantObservations(ctx, tenant, "runs", previousAt, previousID, now)
}

func (c *ObservationClient) ListTenantInstances(ctx context.Context, tenant domain.TenantID, previousAt *time.Time, previousID string, now time.Time) (compute.ObservationPage, error) {
	return c.listTenantObservations(ctx, tenant, "instances", previousAt, previousID, now)
}

func (c *ObservationClient) ListTenantEvents(ctx context.Context, tenant domain.TenantID, previousAt *time.Time, previousID string, now time.Time) (compute.ObservationPage, error) {
	return c.listTenantObservations(ctx, tenant, "events", previousAt, previousID, now)
}

func (c *ObservationClient) listTenantObservations(ctx context.Context, tenant domain.TenantID, stream string, previousAt *time.Time, previousID string, _ time.Time) (compute.ObservationPage, error) {
	binding, err := c.Tenants.Get(ctx, tenant)
	if err != nil || binding.DStack.Provision != domain.ProvisionReady {
		return compute.ObservationPage{}, domain.ErrTenantUnavailable
	}
	credential, err := c.Credentials.CustomerCredential(ctx, tenant, c.EncryptionKey)
	if err != nil || credential == "" {
		return compute.ObservationPage{}, domain.ErrTenantUnavailable
	}
	path := ""
	body := map[string]any{"limit": 100, "ascending": true}
	timeField, idField := "", ""
	switch stream {
	case "runs":
		path, timeField, idField = "/api/runs/list", "prev_submitted_at", "prev_run_id"
		body["project_name"] = string(binding.DStack.Project)
		body["username"] = binding.DStack.Username
	case "instances":
		path, timeField, idField = "/api/instances/list", "prev_created_at", "prev_id"
		body["project_names"] = []string{string(binding.DStack.Project)}
	case "events":
		path, timeField, idField = "/api/events/list", "prev_recorded_at", "prev_id"
		body["include_target_types"] = []string{"run", "instance"}
		if binding.DStack.ProjectID == "" {
			return compute.ObservationPage{}, errors.New("dstack project ID is unavailable for scoped event polling")
		}
		body["within_projects"] = []string{binding.DStack.ProjectID}
	default:
		return compute.ObservationPage{}, errors.New("unsupported dstack observation stream")
	}
	if previousAt != nil {
		body[timeField] = previousAt.UTC().Format(time.RFC3339Nano)
		if previousID != "" {
			body[idField] = previousID
		} else if stream == "events" {
			body[idField] = "00000000-0000-0000-0000-000000000000"
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return compute.ObservationPage{}, err
	}
	base, err := url.Parse(c.BaseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil {
		return compute.ObservationPage{}, errors.New("dstack endpoint must be an absolute HTTPS URL")
	}
	base.Path = strings.TrimRight(base.Path, "/") + path
	base.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String(), bytes.NewReader(encoded))
	if err != nil {
		return compute.ObservationPage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return compute.ObservationPage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return compute.ObservationPage{}, fmt.Errorf("dstack %s listing returned status %d", stream, resp.StatusCode)
	}
	var items []map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return compute.ObservationPage{}, err
	}
	page := compute.ObservationPage{Done: len(items) < 100}
	atKey := map[string]string{"runs": "submitted_at", "instances": "created", "events": "recorded_at"}[stream]
	for _, item := range items {
		var id string
		if err := json.Unmarshal(item["id"], &id); err != nil || id == "" {
			return compute.ObservationPage{}, errors.New("dstack returned an observation without an ID")
		}
		var timestamp time.Time
		if err := json.Unmarshal(item[atKey], &timestamp); err != nil {
			return compute.ObservationPage{}, errors.New("dstack returned an observation without a valid timestamp")
		}
		payload, err := json.Marshal(item)
		if err != nil {
			return compute.ObservationPage{}, err
		}
		if stream == "events" {
			var targets []struct {
				ProjectName *string `json:"project_name"`
			}
			if err := json.Unmarshal(item["targets"], &targets); err != nil {
				return compute.ObservationPage{}, errors.New("dstack returned an event with invalid targets")
			}
			belongs := false
			for _, target := range targets {
				if target.ProjectName != nil && *target.ProjectName == string(binding.DStack.Project) {
					belongs = true
					break
				}
			}
			if !belongs {
				continue
			}
		}
		sum := sha256.Sum256(payload)
		page.Observations = append(page.Observations, compute.Observation{Tenant: tenant, Stream: stream, ID: id, Version: hex.EncodeToString(sum[:]), ObservedAt: timestamp, Payload: payload})
	}
	if len(items) > 0 {
		item := items[len(items)-1]
		var timestamp time.Time
		if err := json.Unmarshal(item[atKey], &timestamp); err != nil {
			return compute.ObservationPage{}, errors.New("dstack returned an observation without a valid timestamp")
		}
		var id string
		if err := json.Unmarshal(item["id"], &id); err != nil || id == "" {
			return compute.ObservationPage{}, errors.New("dstack returned an observation without an ID")
		}
		page.NextAt, page.NextID = &timestamp, id
	}
	return page, nil
}
