package dstack

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"billing-backend/internal/domain"
)

func TestAdminClientUsesDocumentedProvisioningPayloadsAndRefreshesToken(t *testing.T) {
	var requests []struct {
		path string
		body string
		auth string
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		requests = append(requests, struct {
			path string
			body string
			auth string
		}{r.URL.Path, string(body), r.Header.Get("Authorization")})
		if r.URL.Path == "/api/users/refresh_token" {
			_, _ = w.Write([]byte(`{"creds":{"token":"tenant-token"}}`))
		}
	}))
	defer server.Close()
	client := AdminClient{BaseURL: server.URL, Token: "admin-secret", HTTP: server.Client()}
	if err := client.post(context.Background(), "/api/projects/create", projectCreateRequest{ProjectName: "tenant-project"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.post(context.Background(), "/api/projects/tenant-project/add_members", addMembersRequest{Members: []memberSetting{{Username: "tenant-user", ProjectRole: "admin"}}}, nil); err != nil {
		t.Fatal(err)
	}
	token, err := client.RefreshToken(context.Background(), "tenant-user")
	if err != nil || token != "tenant-token" {
		t.Fatalf("refreshed token = %q, %v", token, err)
	}
	wantPaths := []string{"/api/projects/create", "/api/projects/tenant-project/add_members", "/api/users/refresh_token"}
	wantBodies := []string{
		`{"project_name":"tenant-project","is_public":false,"templates_repo":null}`,
		`{"members":[{"username":"tenant-user","project_role":"admin"}]}`,
		`{"username":"tenant-user"}`,
	}
	for i, request := range requests {
		if i >= len(wantPaths) || request.path != wantPaths[i] || request.body != wantBodies[i] || request.auth != "Bearer admin-secret" {
			t.Errorf("request %d = %s %s auth=%q", i, request.path, request.body, request.auth)
		}
	}
	if len(requests) != len(wantPaths) {
		t.Fatalf("request count = %d, want %d", len(requests), len(wantPaths))
	}
}

func TestAdminClientClassifiesDStackResourceErrors(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		if r.URL.Path == "/missing" {
			_, _ = w.Write([]byte(`{"detail":[{"code":"resource_not_exists"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"detail":[{"code":"resource_exists"}]}`))
	}))
	defer server.Close()
	client := AdminClient{BaseURL: server.URL, Token: "admin-secret", HTTP: server.Client()}
	if err := client.post(context.Background(), "/missing", map[string]any{}, nil); !errors.Is(err, errResourceNotFound) {
		t.Fatalf("missing-resource error = %v", err)
	}
	if err := client.post(context.Background(), "/exists", map[string]any{}, nil); !errors.Is(err, errResourceExists) {
		t.Fatalf("existing-resource error = %v", err)
	}
}

func TestProvisionerPostsUserRoleAndDoesNotReturnAdminCredential(t *testing.T) {
	var paths []string
	var bodies []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		paths = append(paths, r.URL.Path)
		bodies = append(bodies, string(body))
		if r.URL.Path == "/api/users/refresh_token" {
			_, _ = w.Write([]byte(`{"creds":{"token":"customer-only-token"}}`))
		}
	}))
	defer server.Close()
	client := AdminClient{BaseURL: server.URL, Token: "admin-secret", HTTP: server.Client()}
	if err := client.post(context.Background(), "/api/users/create", userCreateRequest{Username: "tenant-user", GlobalRole: "user", Email: "tenant@example.test", Active: true}, nil); err != nil {
		t.Fatal(err)
	}
	if token, err := client.RefreshToken(context.Background(), "tenant-user"); err != nil || token != "customer-only-token" {
		t.Fatalf("customer credential = %q, %v", token, err)
	}
	if len(paths) != 2 || paths[0] != "/api/users/create" || paths[1] != "/api/users/refresh_token" || bodies[0] != `{"username":"tenant-user","global_role":"user","email":"tenant@example.test","active":true}` {
		t.Fatalf("admin calls = %v %v", paths, bodies)
	}
	for _, body := range bodies {
		if strings.Contains(body, "admin-secret") || strings.Contains(body, "customer-only-token") {
			t.Fatalf("credential leaked in request body %q", body)
		}
	}
}

func TestAdminTokenRefreshFailsClosedWhenEndpointUnavailable(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	client := AdminClient{BaseURL: server.URL, Token: "admin-secret", HTTP: server.Client()}
	if token, err := client.RefreshToken(context.Background(), "tenant-user"); err == nil || token != "" || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing endpoint returned token=%q err=%v", token, err)
	}
}

func TestSharedAccountFailsClosedWithoutProjectPermission(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{"user", `{"username":"provider-user","permissions":{"can_create_projects":false}}`, "cannot create projects"},
		{"empty username", `{"username":"","permissions":{"can_create_projects":true}}`, "no username"},
	} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(test.body))
		}))
		admin := AdminClient{BaseURL: server.URL, Token: "provider-token", HTTP: server.Client()}
		if _, err := sharedAccount(context.Background(), admin); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error = %v, want %q", test.name, err, test.want)
		}
		server.Close()
	}
}

func TestSharedAccountRequiresProjectCreation(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"username":"provider-user","global_role":"user","permissions":{"can_create_projects":true}}`))
	}))
	defer server.Close()
	admin := AdminClient{BaseURL: server.URL, Token: "provider-token", HTTP: server.Client()}
	me, err := sharedAccount(context.Background(), admin)
	if err != nil || me.Username != "provider-user" {
		t.Fatalf("shared account = %+v, %v", me, err)
	}
}

func TestEnsureProjectCreatesOrReusesTheTenantProject(t *testing.T) {
	var paths []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/projects/create":
			_, _ = w.Write([]byte(`{"project_id":"2f6f2f1c-0000-4000-8000-000000000001","project_name":"tenant-project","owner":{"username":"provider-user"}}`))
		case "/api/projects/tenant-project/get":
			_, _ = w.Write([]byte(`{"project_id":"2f6f2f1c-0000-4000-8000-000000000001","project_name":"tenant-project","owner":{"username":"provider-user"}}`))
		}
	}))
	defer server.Close()
	provisioner := &Provisioner{}
	project, err := provisioner.ensureProject(context.Background(), AdminClient{BaseURL: server.URL, Token: "provider-token", HTTP: server.Client()}, "tenant-project")
	if err != nil || project.ProjectID != "2f6f2f1c-0000-4000-8000-000000000001" || project.Owner.Username != "provider-user" {
		t.Fatalf("created project = %+v, %v", project, err)
	}
	if len(paths) != 1 || paths[0] != "/api/projects/create" {
		t.Fatalf("paths = %v", paths)
	}
}

func TestEnsureProjectRejectsAnExistingProjectWithAnotherName(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/projects/create" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"detail":[{"code":"resource_exists"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"project_id":"2f6f2f1c-0000-4000-8000-000000000001","project_name":"other-project"}`))
	}))
	defer server.Close()
	provisioner := &Provisioner{}
	if _, err := provisioner.ensureProject(context.Background(), AdminClient{BaseURL: server.URL, Token: "provider-token", HTTP: server.Client()}, "tenant-project"); err == nil {
		t.Fatal("existing project with another name was accepted")
	}
}

func TestProvisionIdentityCannotChangeEndpointPath(t *testing.T) {
	for _, test := range []struct {
		username string
		project  string
		valid    bool
	}{
		{"tenant-user", "tenant-project", true},
		{"tenant/user", "tenant-project", false},
		{"tenant-user", "tenant/project", false},
	} {
		err := validateProvisionIdentity(domain.DStackBinding{Username: test.username, Project: domain.ProjectName(test.project)})
		if test.valid != (err == nil) {
			t.Errorf("identity %q/%q validation error = %v", test.username, test.project, err)
		}
	}
}
