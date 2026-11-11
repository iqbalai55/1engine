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
		{http.MethodPost, "/api/runs/get", OperationRead},
		{http.MethodPost, "/api/runs/get_plan", OperationRead},
		{http.MethodPost, "/api/logs/poll", OperationRead},
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
		{http.MethodPost, "/api/project/acme/backends/aws/config_info"},
		{http.MethodPost, "/api/project/acme/backends/aws/get_yaml"},
		{http.MethodPost, "/api/project/acme/backends/create_yaml"},
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

func TestConfiguredBackendPlansStayWithinEnabledProviderSet(t *testing.T) {
	backends, err := ParseProviderBackends([]byte("backends:\n  - type: aws\n  - type: runpod\n"))
	if err != nil {
		t.Fatal(err)
	}
	proxy := NewProxy("https://dstack.invalid", nil, nil, nil)
	proxy.Backends = backends
	cases := []struct {
		name string
		path string
		body string
		want string
		fail bool
	}{
		{"fleet defaults to configured backends", "/api/project/acme/fleets/apply", `{"plan":{"spec":{"configuration":{"type":"fleet"},"profile":{}}},"force":false}`, `"backends":["aws","runpod"]`, false},
		{"fleet accepts configured selection", "/api/project/acme/fleets/get_plan", `{"spec":{"configuration":{"type":"fleet","backends":["aws"]},"profile":{}}}`, `"backends":["aws"]`, false},
		{"run checks configuration backend", "/api/project/acme/runs/apply", `{"plan":{"run_spec":{"configuration":{"type":"task","backends":["vastai"]},"profile":{"backends":["aws"]}}},"force":false}`, "", true},
		{"run resolves profile precedence", "/api/project/acme/runs/apply", `{"plan":{"run_spec":{"configuration":{"type":"task","backends":["runpod"]},"profile":{"backends":["aws"]}}},"force":false}`, `"backends":["runpod"]`, false},
		{"run default provider selection", "/api/project/acme/runs/get_plan", `{"run_spec":{"configuration":{"type":"task"}}}`, `"backends":["aws","runpod"]`, false},
		{"empty provider selection is rejected", "/api/project/acme/fleets/apply", `{"plan":{"spec":{"configuration":{"type":"fleet","backends":[]},"profile":{}}},"force":false}`, "", true},
		{"ssh fleets are rejected", "/api/project/acme/fleets/apply", `{"plan":{"spec":{"configuration":{"type":"fleet","ssh_config":{"hosts":["host"]}},"profile":{}}},"force":false}`, "", true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := proxy.bindProviderBackends([]byte(test.body), test.path)
			if test.fail {
				if err == nil {
					t.Fatal("invalid provider selection was accepted")
				}
				return
			}
			if err != nil || !strings.Contains(string(got), test.want) {
				t.Fatalf("bound request = %s, %v, want %q", got, err, test.want)
			}
		})
	}
}

func TestProxyExecutesRunPlanForTheTenantProject(t *testing.T) {
	backends, err := ParseProviderBackends([]byte("backends:\n  - type: aws\n  - type: runpod\n"))
	if err != nil {
		t.Fatal(err)
	}
	var forwarded string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		forwarded = string(body)
		if r.URL.Path != "/api/project/tenant-project/runs/get_plan" {
			t.Errorf("forwarded path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tenant-token" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"project_name":"tenant-project","user":"tenant-user","run_spec":{},"job_plans":[{"job_spec":{},"offers":[{"backend":"aws","price":0.75}],"total_offers":1}],"action":"create"}`))
	}))
	defer server.Close()
	proxy := NewProxy(server.URL, proxyCredential("tenant-token"), nil, nil)
	proxy.Backends = backends
	proxy.HTTP = server.Client()
	op := Operation{
		Method:  http.MethodPost,
		Path:    "/api/project/tenant-project/runs/get_plan",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"run_spec":{"run_name":"hello","configuration":{"type":"task"}}}`),
	}
	resp, err := proxy.Execute(context.Background(), "tenant-id", domain.DStackBinding{Project: "tenant-project", Username: "tenant-user", Provision: domain.ProvisionReady}, op)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusOK || !strings.Contains(string(resp.Body), `"job_plans"`) || !strings.Contains(string(resp.Body), `"price":0.75`) {
		t.Fatalf("plan response = %d %s", resp.Status, resp.Body)
	}
	if !strings.Contains(forwarded, `"backends":["aws","runpod"]`) {
		t.Fatalf("forwarded plan did not select the configured backends: %s", forwarded)
	}
	if strings.Contains(forwarded, "project_name") || strings.Contains(forwarded, "tenant_id") {
		t.Fatalf("plan request leaked a project field the proxy should own: %s", forwarded)
	}
}

func TestProxyScopesAliasedReadsToTheTenantProject(t *testing.T) {
	var paths []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	backends, err := ParseProviderBackends([]byte("backends:\n  - type: aws\n"))
	if err != nil {
		t.Fatal(err)
	}
	proxy := NewProxy(server.URL, proxyCredential("tenant-token"), nil, nil)
	proxy.Backends = backends
	proxy.HTTP = server.Client()
	binding := domain.DStackBinding{Project: "tenant-project", Username: "tenant-user", Provision: domain.ProvisionReady}
	for _, test := range []struct {
		path string
		body string
		want string
	}{
		{"/api/runs/get_plan", `{"run_spec":{"configuration":{"type":"task"}}}`, "/api/project/tenant-project/runs/get_plan"},
		{"/api/runs/get", `{"run_name":"hello"}`, "/api/project/tenant-project/runs/get"},
		{"/api/logs/poll", `{"run_name":"hello"}`, "/api/project/tenant-project/logs/poll"},
	} {
		op := Operation{Method: http.MethodPost, Path: test.path, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(test.body)}
		if _, err := proxy.Execute(context.Background(), "tenant-id", binding, op); err != nil {
			t.Fatalf("%s: %v", test.path, err)
		}
		if got := paths[len(paths)-1]; got != test.want {
			t.Errorf("%s forwarded to %s, want %s", test.path, got, test.want)
		}
	}
	if len(paths) != 3 {
		t.Fatalf("forwarded %d requests, want 3", len(paths))
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
