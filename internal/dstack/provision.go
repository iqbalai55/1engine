package dstack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

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
	Servers       Servers
	Tenant        *tenant.Service
	EncryptionKey []byte
	KeyVersion    string
	HTTP          *http.Client
}

func (p *Provisioner) adminClient(server Server) AdminClient {
	return AdminClient{BaseURL: server.URL, Token: server.Token, HTTP: p.HTTP}
}

func (p *Provisioner) serverFor(t domain.Tenant) (Server, error) {
	server, ok := p.Servers.Get(t.DStack.Server)
	if !ok {
		return Server{}, errors.New("tenant dstack server is not configured")
	}
	return server, nil
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
	Owner       struct {
		Username string `json:"username"`
	} `json:"owner"`
	Members []struct {
		User struct {
			Username string `json:"username"`
		} `json:"user"`
		ProjectRole string `json:"project_role"`
	} `json:"members"`
}

type myUserResponse struct {
	Username    string `json:"username"`
	GlobalRole  string `json:"global_role"`
	Permissions struct {
		CanCreateProjects bool `json:"can_create_projects"`
	} `json:"permissions"`
}

var (
	errResourceExists   = errors.New("dstack resource already exists")
	errResourceNotFound = errors.New("dstack resource does not exist")
)

type dstackError struct {
	Detail []struct {
		Code string `json:"code"`
	} `json:"detail"`
}

func (e dstackError) code() string {
	for _, detail := range e.Detail {
		if detail.Code != "" {
			return detail.Code
		}
	}
	return ""
}

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
		if resp.StatusCode == http.StatusNotFound {
			return errResourceNotFound
		}
		if resp.StatusCode == http.StatusBadRequest {
			var response dstackError
			if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&response); err == nil {
				code := response.code()
				switch code {
				case "resource_exists":
					return errResourceExists
				case "resource_not_exists":
					return errResourceNotFound
				case "invalid_credentials", "backend_not_available":
					return fmt.Errorf("dstack admin operation failed: %s", code)
				}
				if code != "" {
					return fmt.Errorf("dstack admin operation returned status %d (%s)", resp.StatusCode, code)
				}
			}
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

func (c AdminClient) ApplyBackendConfig(ctx context.Context, project string, backend ProviderBackend) error {
	readPath, err := backendConfigReadPath(project, backend.Type)
	if err != nil {
		return err
	}
	var current struct {
		ConfigYAML string `json:"config_yaml"`
	}
	readErr := c.post(ctx, readPath, map[string]any{}, &current)
	writePath, err := backendConfigWritePath(project, backend.Type, false)
	if err != nil {
		return err
	}
	if readErr == nil {
		equal, err := sameEffectiveBackendConfig(current.ConfigYAML, backend.YAML, backend.Type)
		if err != nil {
			return errors.New("dstack returned an invalid provider backend config")
		}
		if equal {
			return nil
		}
		writePath, err = backendConfigWritePath(project, backend.Type, true)
		if err != nil {
			return err
		}
	} else if !errors.Is(readErr, errResourceNotFound) {
		return fmt.Errorf("read existing dstack provider backend: %w", readErr)
	}
	return c.post(ctx, writePath, map[string]string{"config_yaml": backend.YAML}, nil)
}

func backendConfigReadPath(project string, backendType BackendType) (string, error) {
	if _, ok := supportedBackendTypes[backendType]; !ok {
		return "", errors.New("unsupported provider backend type")
	}
	if strings.TrimSpace(project) == "" || strings.ContainsAny(project, "/\\") {
		return "", errors.New("dstack project name is invalid")
	}
	return "/api/project/" + url.PathEscape(project) + "/backends/" + string(backendType) + "/get_yaml", nil
}

func backendConfigWritePath(project string, backendType BackendType, update bool) (string, error) {
	if _, ok := supportedBackendTypes[backendType]; !ok {
		return "", errors.New("unsupported provider backend type")
	}
	if strings.TrimSpace(project) == "" || strings.ContainsAny(project, "/\\") {
		return "", errors.New("dstack project name is invalid")
	}
	action := "create_yaml"
	if update {
		action = "update_yaml"
	}
	return "/api/project/" + url.PathEscape(project) + "/backends/" + action, nil
}

func sameEffectiveBackendConfig(actualYAML, desiredYAML string, backendType BackendType) (bool, error) {
	var actual any
	if err := yaml.Unmarshal([]byte(actualYAML), &actual); err != nil {
		return false, err
	}
	var desired any
	if err := yaml.Unmarshal([]byte(desiredYAML), &desired); err != nil {
		return false, err
	}
	actualMap, actualOK := actual.(map[string]any)
	desiredMap, desiredOK := desired.(map[string]any)
	if !actualOK || !desiredOK {
		return false, errors.New("backend config must be a YAML mapping")
	}
	if _, regionsSpecified := desiredMap["regions"]; !regionsSpecified {
		delete(actualMap, "regions")
	}
	if backendType == BackendGCP {
		if credentials, ok := actualMap["creds"].(map[string]any); ok && credentials["filename"] == "" {
			delete(credentials, "filename")
		}
	}
	return reflect.DeepEqual(actualMap, desiredMap), nil
}

func (p *Provisioner) Provision(ctx context.Context, id domain.TenantID) error {
	t, err := p.Tenant.Get(ctx, id)
	if err != nil {
		return err
	}
	server, err := p.serverFor(t)
	if err != nil {
		return p.failed(ctx, id, err)
	}
	hasCredential, err := p.Tenant.HasCredential(ctx, id, server.Name)
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
	if server.Mode == ModeSharedProject {
		return p.provisionShared(ctx, id, t, server)
	}
	return p.provisionOwnedAccount(ctx, id, t, server)
}

func (p *Provisioner) provisionOwnedAccount(ctx context.Context, id domain.TenantID, t domain.Tenant, server Server) error {
	admin := p.adminClient(server)
	email, err := p.Identity.Email(ctx, string(t.Owner))
	if err != nil {
		return err
	}
	if err = admin.post(ctx, "/api/users/create", userCreateRequest{Username: t.DStack.Username, GlobalRole: "user", Email: email, Active: true}, nil); err != nil && !errors.Is(err, errResourceExists) {
		return p.failed(ctx, id, err)
	}
	if errors.Is(err, errResourceExists) {
		var existing struct {
			Username string `json:"username"`
		}
		if verifyErr := admin.post(ctx, "/api/users/get_user", map[string]string{"username": t.DStack.Username}, &existing); verifyErr != nil || existing.Username != t.DStack.Username {
			return p.failed(ctx, id, errors.New("existing dstack user does not match tenant identity"))
		}
	}
	project, err := p.ensureProject(ctx, admin, string(t.DStack.Project))
	if err != nil {
		return p.failed(ctx, id, err)
	}
	if err := p.recordProject(ctx, id, t, project); err != nil {
		return p.failed(ctx, id, err)
	}
	if err := p.addAdminMember(ctx, admin, string(t.DStack.Project), t.DStack.Username); err != nil {
		return p.failed(ctx, id, err)
	}
	if err := p.applyBackends(ctx, admin, string(t.DStack.Project), server.Backends); err != nil {
		return p.failed(ctx, id, err)
	}
	if p.EncryptionKey == nil || p.KeyVersion == "" {
		return p.failed(ctx, id, errors.New("dstack credential encryption key configuration is unavailable"))
	}
	secret, err := admin.RefreshToken(ctx, t.DStack.Username)
	if err != nil || strings.TrimSpace(secret) == "" {
		if err == nil {
			err = errors.New("dstack admin token refresh returned an empty token")
		}
		return p.failed(ctx, id, err)
	}
	if err = p.Tenant.SaveCustomerCredential(ctx, id, server.Name, secret, p.EncryptionKey, p.KeyVersion); err != nil {
		return p.failed(ctx, id, err)
	}
	return nil
}

func (p *Provisioner) provisionShared(ctx context.Context, id domain.TenantID, t domain.Tenant, server Server) error {
	admin := p.adminClient(server)
	if p.EncryptionKey == nil || p.KeyVersion == "" {
		return p.failed(ctx, id, errors.New("dstack credential encryption key configuration is unavailable"))
	}
	if server.Token == "" || strings.TrimSpace(server.URL) == "" {
		return p.failed(ctx, id, errors.New("dstack provider account token is not configured"))
	}
	me, err := sharedAccount(ctx, admin)
	if err != nil {
		return p.failed(ctx, id, err)
	}
	project, err := p.ensureProject(ctx, admin, string(t.DStack.Project))
	if err != nil {
		return p.failed(ctx, id, err)
	}
	if project.Owner.Username != "" && project.Owner.Username != me.Username {
		return p.failed(ctx, id, errors.New("existing dstack project belongs to another account"))
	}
	if err := p.recordProject(ctx, id, t, project); err != nil {
		return p.failed(ctx, id, err)
	}
	if err := p.Tenant.SetAccountUsername(ctx, id, me.Username); err != nil {
		return p.failed(ctx, id, err)
	}
	if err := p.Tenant.SaveCustomerCredential(ctx, id, server.Name, server.Token, p.EncryptionKey, p.KeyVersion); err != nil {
		return p.failed(ctx, id, err)
	}
	return nil
}

func sharedAccount(ctx context.Context, admin AdminClient) (myUserResponse, error) {
	var me myUserResponse
	if err := admin.post(ctx, "/api/users/get_my_user", map[string]any{}, &me); err != nil {
		return me, fmt.Errorf("dstack provider account is unavailable: %w", err)
	}
	if strings.TrimSpace(me.Username) == "" {
		return me, errors.New("dstack provider account returned no username")
	}
	if !me.Permissions.CanCreateProjects {
		return me, errors.New("dstack provider account cannot create projects")
	}
	return me, nil
}

func (p *Provisioner) ensureProject(ctx context.Context, admin AdminClient, name string) (projectResponse, error) {
	var created projectResponse
	err := admin.post(ctx, "/api/projects/create", projectCreateRequest{ProjectName: name}, &created)
	if err == nil {
		return created, nil
	}
	if !errors.Is(err, errResourceExists) {
		return projectResponse{}, err
	}
	var existing projectResponse
	if verifyErr := admin.post(ctx, "/api/projects/"+url.PathEscape(name)+"/get", map[string]any{}, &existing); verifyErr != nil || existing.ProjectName != name {
		return projectResponse{}, errors.New("existing dstack project does not match tenant identity")
	}
	return existing, nil
}

func (p *Provisioner) recordProject(ctx context.Context, id domain.TenantID, t domain.Tenant, project projectResponse) error {
	if project.ProjectName != "" && project.ProjectName != string(t.DStack.Project) {
		return errors.New("dstack returned a project that does not match tenant identity")
	}
	if project.ProjectID == "" {
		return nil
	}
	return p.Tenant.SetProjectID(ctx, id, project.ProjectID)
}

func (p *Provisioner) addAdminMember(ctx context.Context, admin AdminClient, project, username string) error {
	memberPath := "/api/projects/" + url.PathEscape(project) + "/add_members"
	err := admin.post(ctx, memberPath, addMembersRequest{Members: []memberSetting{{Username: username, ProjectRole: "admin"}}}, nil)
	if err == nil {
		return nil
	}
	if !errors.Is(err, errResourceExists) {
		return err
	}
	var existing projectResponse
	if verifyErr := admin.post(ctx, "/api/projects/"+url.PathEscape(project)+"/get", map[string]any{}, &existing); verifyErr != nil || !hasAdminMember(existing, username) {
		return errors.New("existing dstack project membership does not match tenant identity")
	}
	return nil
}

func (p *Provisioner) SyncConfiguredBackends(ctx context.Context) error {
	tenants, err := p.Tenant.List(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, current := range tenants {
		if current.DStack.Provision != domain.ProvisionReady || current.State == domain.TenantSuspended {
			continue
		}
		server, ok := p.Servers.Get(current.DStack.Server)
		if !ok || len(server.Backends.Types()) == 0 {
			continue
		}
		if err := p.applyBackends(ctx, p.adminClient(server), string(current.DStack.Project), server.Backends); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (p *Provisioner) applyBackends(ctx context.Context, admin AdminClient, project string, backends ProviderBackends) error {
	for _, backendType := range backends.Types() {
		backend, _ := backends.Get(backendType)
		if err := admin.ApplyBackendConfig(ctx, project, backend); err != nil {
			return err
		}
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
