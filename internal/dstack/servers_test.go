package dstack

import (
	"strings"
	"testing"
)

func TestServersKeepConfigurationOrderAndValidateEachServer(t *testing.T) {
	backends, err := ParseProviderBackends([]byte("backends:\n  - type: runpod\n"))
	if err != nil {
		t.Fatal(err)
	}
	servers, err := NewServers(
		Server{Name: "self-hosted", URL: "https://dstack.internal", Token: "admin-token", Mode: ModeAdminRefresh, Backends: backends},
		Server{Name: "sky", URL: "https://sky.dstack.ai", Token: "user-token", Mode: ModeSharedProject},
	)
	if err != nil {
		t.Fatal(err)
	}
	names := servers.Names()
	if len(names) != 2 || names[0] != "self-hosted" || names[1] != "sky" {
		t.Fatalf("server names = %v", names)
	}
	first, ok := servers.Default()
	if !ok || first.Name != "self-hosted" {
		t.Fatalf("default server = %+v", first)
	}
	sky, ok := servers.Get("sky")
	if !ok || sky.Mode != ModeSharedProject {
		t.Fatalf("sky server = %+v", sky)
	}
	if _, ok := servers.Get("missing"); ok {
		t.Fatal("unknown server was returned")
	}
}

func TestServersRejectInvalidDefinitions(t *testing.T) {
	backends, err := ParseProviderBackends([]byte("backends:\n  - type: aws\n"))
	if err != nil {
		t.Fatal(err)
	}
	valid := Server{Name: "primary", URL: "https://dstack.internal", Token: "admin-token", Mode: ModeAdminRefresh}
	cases := []struct {
		name string
		defs []Server
		want string
	}{
		{"duplicate name", []Server{valid, valid}, "configured more than once"},
		{"missing name", []Server{{URL: valid.URL, Token: valid.Token, Mode: valid.Mode}}, "name is required"},
		{"plain http", []Server{{Name: "a", URL: "http://dstack.internal", Token: valid.Token, Mode: valid.Mode}}, "HTTPS URL"},
		{"credentials in url", []Server{{Name: "a", URL: "https://user:pass@dstack.internal", Token: valid.Token, Mode: valid.Mode}}, "HTTPS URL"},
		{"missing token", []Server{{Name: "a", URL: valid.URL, Mode: valid.Mode}}, "token is required"},
		{"unknown mode", []Server{{Name: "a", URL: valid.URL, Token: valid.Token, Mode: "cloud"}}, "mode must be"},
		{"shared with backends", []Server{{Name: "a", URL: valid.URL, Token: valid.Token, Mode: ModeSharedProject, Backends: backends}}, "must not configure provider backends"},
	}
	for _, test := range cases {
		if _, err := NewServers(test.defs...); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error = %v, want %q", test.name, err, test.want)
		}
	}
}
