package dstack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"billing-backend/internal/domain"
)

func TestPolicyAllowsDocumentedOperationsAndRejectsOthers(t *testing.T) {
	p := NewProxy("https://dstack.invalid", nil, nil, nil)
	allowed := []struct {
		method string
		path   string
		kind   OperationKind
	}{
		{http.MethodPost, "/api/runs/list", OperationRead},
		{http.MethodPost, "/api/instances/list", OperationRead},
		{http.MethodPost, "/api/project/acme/runs/get", OperationRead},
		{http.MethodPost, "/api/project/acme/runs/apply", OperationStart},
		{http.MethodPost, "/api/project/acme/runs/stop", OperationStop},
	}
	for _, test := range allowed {
		if kind, err := p.classify(test.method, test.path, "acme"); err != nil || kind != test.kind {
			t.Errorf("%s %s = %q, %v, want %q", test.method, test.path, kind, err, test.kind)
		}
	}
	denied := []struct{ method, path string }{
		{http.MethodGet, "/api/runs/list"},
		{http.MethodGet, "/api/project/acme/runs/list"},
		{http.MethodPost, "/api/projects/create"},
		{http.MethodPost, "/api/projects/list"},
		{http.MethodPost, "/api/fleets/list"},
		{http.MethodPost, "/api/volumes/list"},
		{http.MethodPost, "/api/server/info"},
		{http.MethodPost, "/api/users/create"},
		{http.MethodPost, "/api/users/refresh_token"},
		{http.MethodPost, "/api/users/get_my_user"},
		{http.MethodPost, "/api/auth/list_providers"},
		{http.MethodPost, "/api/projects/acme/add_members"},
		{http.MethodPost, "/api/projects/acme/update"},
		{http.MethodPost, "/api/project/other/runs/get"},
		{http.MethodPost, "/api/project/acme/runs/apply/extra"},
		{http.MethodPost, "/api/project/acme/runs/import"},
		{http.MethodPost, "/api/project/acme/runs/apply/future"},
		{http.MethodPost, "/api/projects/acme/delete"},
		{http.MethodDelete, "/api/project/acme/runs/run-1"},
	}
	for _, test := range denied {
		if _, err := p.classify(test.method, test.path, "acme"); err == nil {
			t.Errorf("%s %s was allowed", test.method, test.path)
		}
	}
}

func TestProxyBindsTenantProjectAndUserOnlyAtDocumentedFields(t *testing.T) {
	got, err := bindProject([]byte(`{"project_name":"","username":"","description":"project other"}`), "/api/runs/list", "tenant-project", "tenant-project-id", "tenant-user")
	if err != nil {
		t.Fatal(err)
	}
	var actual map[string]any
	if err := json.Unmarshal(got, &actual); err != nil {
		t.Fatal(err)
	}
	if actual["project_name"] != "tenant-project" || actual["username"] != "tenant-user" || actual["description"] != "project other" {
		t.Fatalf("bound JSON = %s", got)
	}
	if _, err := bindProject([]byte(`{"project_name":"other"}`), "/api/runs/list", "tenant-project", "tenant-project-id", "tenant-user"); err == nil {
		t.Fatal("arbitrary project selection was accepted")
	}
	instances, err := bindProject([]byte(`{"project_names":["tenant-project"]}`), "/api/instances/list", "tenant-project", "tenant-project-id", "tenant-user")
	if err != nil || !strings.Contains(string(instances), `"project_names":["tenant-project"]`) {
		t.Fatalf("instance filter = %s, %v", instances, err)
	}
	if _, err := bindProject([]byte(`{"project_names":["other"]}`), "/api/instances/list", "tenant-project", "tenant-project-id", "tenant-user"); err == nil {
		t.Fatal("cross-project instance filter was accepted")
	}
	events, err := bindProject([]byte(`{"within_projects":["tenant-project-id"]}`), "/api/events/list", "tenant-project", "tenant-project-id", "tenant-user")
	if err != nil || !strings.Contains(string(events), `"within_projects":["tenant-project-id"]`) {
		t.Fatalf("event project UUID filter = %s, %v", events, err)
	}
	if _, err := bindProject([]byte(`{"within_projects":["other-project-id"]}`), "/api/events/list", "tenant-project", "tenant-project-id", "tenant-user"); err == nil {
		t.Fatal("cross-project event filter was accepted")
	}
}

type proxyCredential string

func (c proxyCredential) CustomerCredential(context.Context, domain.TenantID, []byte) (string, error) {
	return string(c), nil
}

func TestProxyForwardsOnlyCustomerCredentialAndTenantScopedRequest(t *testing.T) {
	adminReached := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		adminReached = strings.Contains(r.URL.Path, "/api/users/")
		if r.Header.Get("Authorization") != "Bearer customer-secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/runs/list" {
			t.Errorf("forwarded endpoint = %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"only_active":true,"project_name":"tenant-project","username":"tenant-user"}` {
			t.Errorf("forwarded body = %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	proxy := NewProxy(server.URL, proxyCredential("customer-secret"), nil, nil)
	proxy.HTTP = server.Client()
	if _, err := proxy.Execute(context.Background(), "tenant-id", domain.DStackBinding{Project: "tenant-project", Username: "tenant-user", Provision: domain.ProvisionReady}, Operation{Method: http.MethodPost, Path: "/api/runs/list", Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if adminReached {
		t.Fatal("customer request reached an admin endpoint")
	}
}

func TestProxyRejectsBodyRewriteForUnknownPath(t *testing.T) {
	proxy := NewProxy("https://dstack.invalid", proxyCredential("customer-secret"), nil, nil)
	if _, err := proxy.Execute(context.Background(), "tenant-id", domain.DStackBinding{Project: "tenant-project", Username: "tenant-user", Provision: domain.ProvisionReady}, Operation{Method: http.MethodPost, Path: "/api/project/tenant-project/runs/apply/extra", Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"project_name":"other"}`)}); err == nil {
		t.Fatal("unknown path was rewritten and accepted")
	}
}

func TestProxyClassifiesDocumentedCustomerOperationsByBillingImpact(t *testing.T) {
	proxy := NewProxy("https://dstack.invalid", nil, nil, nil)
	for _, test := range []struct {
		path string
		kind OperationKind
	}{
		{"/api/project/acme/fleets/apply", OperationStart},
		{"/api/project/acme/volumes/create", OperationMutation},
		{"/api/project/acme/secrets/create_or_update", OperationMutation},
		{"/api/users/public_keys/add", OperationMutation},
		{"/api/events/list", OperationRead},
	} {
		if kind, err := proxy.classify(http.MethodPost, test.path, "acme"); err != nil || kind != test.kind {
			t.Errorf("customer route %s classified %q, %v, want %q", test.path, kind, err, test.kind)
		}
	}
}

func TestProjectPathRewriteEscapesValidatedName(t *testing.T) {
	proxy := NewProxy("https://dstack.invalid", proxyCredential("customer-secret"), nil, nil)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/project/project%20with%20space/runs/get" {
			t.Errorf("escaped path = %q", r.URL.EscapedPath())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	proxy.BaseURL = server.URL
	proxy.HTTP = server.Client()
	_, err := proxy.Execute(context.Background(), "tenant-id", domain.DStackBinding{Project: "project with space", Username: "tenant-user", Provision: domain.ProvisionReady}, Operation{Method: http.MethodPost, Path: "/api/project/project with space/runs/get", Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
}

func TestComputeStartRequiresBoundedIdempotencyKey(t *testing.T) {
	proxy := NewProxy("https://dstack.invalid", proxyCredential("customer-secret"), nil, nil)
	_, err := proxy.Execute(context.Background(), "tenant-id", domain.DStackBinding{Project: "tenant-project", Username: "tenant-user", Provision: domain.ProvisionReady}, Operation{Method: http.MethodPost, Path: "/api/project/tenant-project/runs/apply", Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"plan":{},"force":false}`)})
	if err == nil || !strings.Contains(err.Error(), "Idempotency-Key") {
		t.Fatalf("compute start without identity returned %v", err)
	}
	_, err = proxy.Execute(context.Background(), "tenant-id", domain.DStackBinding{Project: "tenant-project", Username: "tenant-user", Provision: domain.ProvisionReady}, Operation{Method: http.MethodPost, Path: "/api/project/tenant-project/runs/apply", Headers: http.Header{"Content-Type": []string{"application/json"}}, IdempotencyKey: "safe-key", Body: []byte(`not-json`)})
	if err == nil || !strings.Contains(err.Error(), "valid JSON") {
		t.Fatalf("invalid compute request body returned %v", err)
	}
}

func TestRunApplyRequiresSafeDocumentedPayload(t *testing.T) {
	got, err := bindProject([]byte(`{"plan":{"run_spec":{"project_name":"","user":""}},"force":false}`), "/api/project/tenant-project/runs/apply", "tenant-project", "tenant-project-id", "tenant-user")
	if err != nil || !strings.Contains(string(got), `"project_name":"tenant-project"`) || !strings.Contains(string(got), `"user":"tenant-user"`) {
		t.Fatalf("bound run apply = %s, %v", got, err)
	}
	if _, err := bindProject([]byte(`{"plan":{"run_spec":{"project_name":"other"}},"force":false}`), "/api/project/tenant-project/runs/apply", "tenant-project", "tenant-project-id", "tenant-user"); err == nil {
		t.Fatal("cross-project run plan was accepted")
	}
	if _, err := bindProject([]byte(`{"plan":{"run_spec":{}},"force":true}`), "/api/project/tenant-project/runs/apply", "tenant-project", "tenant-project-id", "tenant-user"); err == nil {
		t.Fatal("forced run plan was accepted")
	}
}

func TestProxyClassifiesDocumentedInstanceRead(t *testing.T) {
	proxy := NewProxy("https://dstack.invalid", nil, nil, nil)
	kind, err := proxy.classify(http.MethodPost, "/api/project/acme/instances/get", "acme")
	if err != nil || kind != OperationRead {
		t.Fatalf("documented instance read = %q, %v", kind, err)
	}
}

func TestProxyRejectsPathVariantsAndQueryRewrites(t *testing.T) {
	proxy := NewProxy("https://dstack.invalid", nil, nil, nil)
	for _, path := range []string{"/api/project/acme/runs/get/..", "/api/project/acme%2fruns/get", "//evil.example/api/runs/list"} {
		if _, err := proxy.Execute(context.Background(), "tenant-id", domain.DStackBinding{Project: "acme", Username: "tenant-user", Provision: domain.ProvisionReady}, Operation{Method: http.MethodPost, Path: path}); err == nil {
			t.Errorf("path variant %q was accepted", path)
		}
	}
	if _, err := proxy.Execute(context.Background(), "tenant-id", domain.DStackBinding{Project: "acme", Username: "tenant-user", Provision: domain.ProvisionReady}, Operation{Method: http.MethodPost, Path: "/api/runs/list", Query: map[string][]string{"project_name": {"other"}}}); err == nil {
		t.Fatal("query was allowed to select another project")
	}
}

func TestProxyScopesModelAndServicePathsToTenantProject(t *testing.T) {
	proxy := NewProxy("https://dstack.invalid", nil, nil, nil)
	for _, path := range []string{
		"/proxy/models/other-project/models",
		"/proxy/models/other-project/chat/completions",
		"/proxy/services/other-project/run-1",
	} {
		if _, err := proxy.classify(http.MethodGet, path, "tenant-project"); err == nil {
			t.Fatalf("cross-project proxy path %q was accepted", path)
		}
	}
	for _, test := range []struct {
		method string
		path   string
		kind   OperationKind
	}{
		{http.MethodGet, "/proxy/models/tenant-project/models", OperationRead},
		{http.MethodPost, "/proxy/models/tenant-project/chat/completions", OperationStart},
		{http.MethodGet, "/proxy/services/tenant-project/run-1", OperationRead},
	} {
		if kind, err := proxy.classify(test.method, test.path, "tenant-project"); err != nil || kind != test.kind {
			t.Errorf("tenant proxy route %s %s classified %q, %v", test.method, test.path, kind, err)
		}
	}
}

func TestProxyResponseRedactsCredentials(t *testing.T) {
	got, err := redactJSON([]byte(`{"token":"secret","nested":{"password":"secret","value":1},"items":[{"api_secret":"secret"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "secret") || strings.Contains(string(got), "password") {
		t.Fatalf("credential leaked in response %s", got)
	}
}
