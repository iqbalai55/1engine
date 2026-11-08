package dstack

import (
	"context"
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
		if request.path != wantPaths[i] || request.body != wantBodies[i] || request.auth != "Bearer admin-secret" {
			t.Errorf("request %d = %s %s auth=%q, want %s %s auth=admin", i, request.path, request.body, request.auth, wantPaths[i], wantBodies[i])
		}
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
