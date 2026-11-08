package dstack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"billing-backend/internal/domain"
	"billing-backend/internal/identity"
	"billing-backend/internal/tenant"
)

type AdminClient struct {
	BaseURL, Token string
	HTTP           *http.Client
}
type Provisioner struct {
	Identity      *identity.Service
	Admin         AdminClient
	Tenant        *tenant.Service
	EncryptionKey []byte
	KeyVersion    string
}
type userCreateRequest struct {
	Username   string `json:"username"`
	GlobalRole string `json:"global_role"`
	Email      string `json:"email"`
	Active     bool   `json:"active"`
}
type projectCreateRequest struct {
	ProjectName   string  `json:"project_name"`
	IsPublic      bool    `json:"is_public"`
	TemplatesRepo *string `json:"templates_repo"`
}
type memberSetting struct {
	Username    string `json:"username"`
	ProjectRole string `json:"project_role"`
}
type addMembersRequest struct {
	Members []memberSetting `json:"members"`
}
type refreshTokenResponse struct {
	Creds struct {
		Token string `json:"token"`
	} `json:"creds"`
}

type projectResponse struct {
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
	Members     []struct {
		User struct {
			Username string `json:"username"`
		} `json:"user"`
		ProjectRole string `json:"project_role"`
	} `json:"members"`
}

var errResourceExists = errors.New("dstack resource already exists")

func (c AdminClient) post(ctx context.Context, path string, input, output any) error {
	if c.BaseURL == "" || c.Token == "" {
		return errors.New("dstack admin configuration is unavailable")
	}
	base, err := url.Parse(c.BaseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil {
		return errors.New("dstack admin endpoint must be an absolute HTTPS URL")
	}
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	base.Path = strings.TrimRight(base.Path, "/") + path
	base.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusConflict {
			return errResourceExists
		}
		return fmt.Errorf("dstack admin operation returned status %d", resp.StatusCode)
	}
	if output != nil {
		if err := json.NewDecoder(resp.Body).Decode(output); err != nil {
			return err
		}
	}
	return nil
}

func (c AdminClient) RefreshToken(ctx context.Context, username string) (string, error) {
	if strings.TrimSpace(username) == "" {
		return "", errors.New("dstack username is required for token refresh")
	}
	var response refreshTokenResponse
	if err := c.post(ctx, "/api/users/refresh_token", map[string]string{"username": username}, &response); err != nil {
		return "", fmt.Errorf("dstack customer token refresh is unavailable: %w", err)
	}
	if strings.TrimSpace(response.Creds.Token) == "" {
		return "", errors.New("dstack customer token refresh returned no token")
	}
	return response.Creds.Token, nil
}

func (p *Provisioner) Provision(ctx context.Context, id domain.TenantID) error {
	t, err := p.Tenant.Get(ctx, id)
	if err != nil {
		return err
	}
	hasCredential, err := p.Tenant.HasCredential(ctx, id)
	if err != nil {
		return err
	}
	if t.DStack.Provision == domain.ProvisionReady && hasCredential {
		return nil
	}
	if t.State == domain.TenantSuspended {
		return domain.ErrTenantUnavailable
	}
	if err := validateProvisionIdentity(t.DStack); err != nil {
		return p.failed(ctx, id, err)
	}
	email, err := p.Identity.Email(ctx, string(t.Owner))
	if err != nil {
		return err
	}
	if err = p.Admin.post(ctx, "/api/users/create", userCreateRequest{Username: t.DStack.Username, GlobalRole: "user", Email: email, Active: true}, nil); err != nil && !errors.Is(err, errResourceExists) {
		return p.failed(ctx, id, err)
	}
	if errors.Is(err, errResourceExists) {
		var existing struct {
			Username string `json:"username"`
		}
		if verifyErr := p.Admin.post(ctx, "/api/users/get_user", map[string]string{"username": t.DStack.Username}, &existing); verifyErr != nil || existing.Username != t.DStack.Username {
			return p.failed(ctx, id, errors.New("existing dstack user does not match tenant identity"))
		}
	}
	var createdProject projectResponse
	if err = p.Admin.post(ctx, "/api/projects/create", projectCreateRequest{ProjectName: string(t.DStack.Project)}, &createdProject); err != nil && !errors.Is(err, errResourceExists) {
		return p.failed(ctx, id, err)
	}
	if errors.Is(err, errResourceExists) {
		if verifyErr := p.Admin.post(ctx, "/api/projects/"+url.PathEscape(string(t.DStack.Project))+"/get", map[string]any{}, &createdProject); verifyErr != nil || createdProject.ProjectName != string(t.DStack.Project) {
			return p.failed(ctx, id, errors.New("existing dstack project does not match tenant identity"))
		}
	}
	if createdProject.ProjectID != "" {
		if err = p.Tenant.SetProjectID(ctx, id, createdProject.ProjectID); err != nil {
			return p.failed(ctx, id, err)
		}
	}
	memberPath := "/api/projects/" + url.PathEscape(string(t.DStack.Project)) + "/add_members"
	if err = p.Admin.post(ctx, memberPath, addMembersRequest{Members: []memberSetting{{Username: t.DStack.Username, ProjectRole: "admin"}}}, nil); err != nil && !errors.Is(err, errResourceExists) {
		return p.failed(ctx, id, err)
	}
	if errors.Is(err, errResourceExists) {
		var existing projectResponse
		if verifyErr := p.Admin.post(ctx, "/api/projects/"+url.PathEscape(string(t.DStack.Project))+"/get", map[string]any{}, &existing); verifyErr != nil || !hasAdminMember(existing, t.DStack.Username) {
			return p.failed(ctx, id, errors.New("existing dstack project membership does not match tenant identity"))
		}
	}
	if p.Admin.Token == "" || strings.TrimSpace(p.Admin.BaseURL) == "" {
		return p.failed(ctx, id, errors.New("dstack admin refresh-token compatibility is not configured"))
	}
	if p.EncryptionKey == nil || p.KeyVersion == "" {
		return p.failed(ctx, id, errors.New("dstack credential encryption key configuration is unavailable"))
	}
	secret, err := p.Admin.RefreshToken(ctx, t.DStack.Username)
	if err != nil || strings.TrimSpace(secret) == "" {
		if err == nil {
			err = errors.New("dstack admin token refresh returned an empty token")
		}
		return p.failed(ctx, id, err)
	}
	if err = p.Tenant.SaveCustomerCredential(ctx, id, secret, p.EncryptionKey, p.KeyVersion); err != nil {
		return p.failed(ctx, id, err)
	}
	return nil
}

func validateProvisionIdentity(binding domain.DStackBinding) error {
	if binding.Username == "" || binding.Project == "" || strings.ContainsAny(binding.Username, "/\\") || strings.ContainsAny(string(binding.Project), "/\\") {
		return errors.New("dstack tenant identity is invalid")
	}
	return nil
}

func hasAdminMember(project projectResponse, username string) bool {
	for _, member := range project.Members {
		if member.User.Username == username && member.ProjectRole == "admin" {
			return true
		}
	}
	return false
}

func (p *Provisioner) failed(ctx context.Context, id domain.TenantID, cause error) error {
	_ = p.Tenant.MarkProvisionFailed(ctx, id)
	return cause
}

func (p *Provisioner) ProvisionPending(ctx context.Context, limit int) error {
	ids, err := p.Tenant.ListAwaitingProvisioning(ctx, limit)
	if err != nil {
		return err
	}
	var firstErr error
	for _, id := range ids {
		if err := p.Provision(ctx, id); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (p *Provisioner) PendingTenants(ctx context.Context, limit int) ([]domain.TenantID, error) {
	return p.Tenant.ListAwaitingProvisioning(ctx, limit)
}
