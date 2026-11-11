package dstack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"

	"billing-backend/internal/domain"
)

// Offer is one rentable GPU option that dstack can see for a tenant.
type Offer struct {
	Provider     string  `json:"provider"`
	Region       string  `json:"region"`
	InstanceType string  `json:"instance_type"`
	GPUName      string  `json:"gpu_name"`
	GPUCount     int     `json:"gpu_count"`
	GPUMemoryGB  float64 `json:"gpu_memory_gb"`
	PricePerHour float64 `json:"price_per_hour"`
	Availability string  `json:"availability"`
}

type offerPlanResponse struct {
	JobPlans []struct {
		Offers []struct {
			Backend      string  `json:"backend"`
			Region       string  `json:"region"`
			Price        float64 `json:"price"`
			Availability string  `json:"availability"`
			Instance     struct {
				Name      string `json:"name"`
				Resources struct {
					GPUs []struct {
						Name      string  `json:"name"`
						MemoryMiB float64 `json:"memory_mib"`
					} `json:"gpus"`
				} `json:"resources"`
			} `json:"instance"`
		} `json:"offers"`
	} `json:"job_plans"`
}

// Offers lists rentable capacity for a tenant without reserving anything or creating a
// fleet. It asks dstack for offers only, so no instance is provisioned and no credits move.
func (p *Proxy) Offers(ctx context.Context, tenant domain.TenantID, binding domain.DStackBinding, gpuCount int, minMemoryGB float64) ([]Offer, error) {
	if gpuCount < 1 || gpuCount > 8 {
		return nil, errors.New("gpu_count must be between 1 and 8")
	}
	if minMemoryGB < 0 || minMemoryGB > 1024 {
		return nil, errors.New("min_memory_gb must be between 0 and 1024")
	}
	if binding.Project == "" {
		return nil, domain.ErrTenantUnavailable
	}
	gpu := map[string]any{"count": gpuCount}
	if minMemoryGB > 0 {
		gpu["memory"] = fmt.Sprintf("%gGB", minMemoryGB)
	}
	plan := map[string]any{
		"run_spec": map[string]any{
			"run_name": "offers",
			"configuration": map[string]any{
				"type":     "task",
				"image":    "python:3.12",
				"commands": []string{"true"},
				"resources": map[string]any{
					"gpu": gpu,
				},
			},
		},
		"for_offers_only": true,
	}
	body, err := json.Marshal(plan)
	if err != nil {
		return nil, errors.New("offer request could not be encoded")
	}
	op := Operation{
		Method:  http.MethodPost,
		Path:    "/api/project/" + url.PathEscape(string(binding.Project)) + "/runs/get_plan",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    body,
	}
	response, err := p.Execute(ctx, tenant, binding, op)
	if err != nil {
		return nil, err
	}
	if response.Status < 200 || response.Status >= 300 {
		return nil, fmt.Errorf("offer lookup returned status %d", response.Status)
	}
	return parseOffers(response.Body)
}

func parseOffers(body []byte) ([]Offer, error) {
	var plan offerPlanResponse
	if err := json.Unmarshal(body, &plan); err != nil {
		return nil, errors.New("offer lookup returned invalid JSON")
	}
	offers := make([]Offer, 0, 8)
	for _, jobPlan := range plan.JobPlans {
		for _, offer := range jobPlan.Offers {
			entry := Offer{
				Provider:     offer.Backend,
				Region:       offer.Region,
				InstanceType: offer.Instance.Name,
				PricePerHour: offer.Price,
				Availability: offer.Availability,
				GPUCount:     len(offer.Instance.Resources.GPUs),
			}
			if len(offer.Instance.Resources.GPUs) > 0 {
				entry.GPUName = offer.Instance.Resources.GPUs[0].Name
				entry.GPUMemoryGB = offer.Instance.Resources.GPUs[0].MemoryMiB / 1024
			}
			offers = append(offers, entry)
		}
	}
	sort.SliceStable(offers, func(i, j int) bool { return offers[i].PricePerHour < offers[j].PricePerHour })
	return offers, nil
}
