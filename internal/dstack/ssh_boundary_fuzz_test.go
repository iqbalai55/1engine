package dstack

import (
	"encoding/json"
	"testing"
)

// The secure SSH boundary has two halves:
//
//  1. A run plan forwarded to dstack never contains a customer-supplied
//     ssh_key_pub. dstack fills the key from the tenant user's server-managed
//     key, so the customer never holds a key that opens a host.
//  2. A JSON response returned to a customer never contains the direct host
//     address, the direct host SSH port, or an attach command for the host.
//     The proxied SSH command is the only connection detail that survives.
//
// Go has no verifier for this code, so these targets fuzz the real functions
// with generated bodies. They check every input the fuzzer reaches within the
// time bound, not every input.

func FuzzRunPlanStripsCustomerSSHKey(f *testing.F) {
	backends, err := ParseProviderBackends([]byte("backends:\n  - type: aws\n"))
	if err != nil {
		f.Fatal(err)
	}
	proxy := NewProxy("https://dstack.invalid", nil, nil, nil)
	proxy.Backends = backends
	for _, seed := range []string{
		`{"plan":{"run_spec":{"ssh_key_pub":"ssh-ed25519 AAAA customer","configuration":{"type":"task"}}},"force":false}`,
		`{"run_spec":{"ssh_key_pub":"ssh-ed25519 AAAA customer","configuration":{"type":"task"}}}`,
		`{"plan":{"run_spec":{"configuration":{"type":"task"}}},"force":false}`,
		`{"run_spec":{"configuration":{"type":"task"}}}`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, body string) {
		for _, path := range []string{
			"/api/project/acme/runs/apply",
			"/api/project/acme/runs/get_plan",
		} {
			out, err := proxy.bindProviderBackends([]byte(body), path)
			if err != nil {
				continue
			}
			if hasJSONKey(out, "ssh_key_pub") {
				t.Fatalf("%s forwarded a customer SSH key: %s", path, out)
			}
		}
	})
}

func FuzzResponseScrubsDirectHostAccess(f *testing.F) {
	for _, seed := range []string{
		`{"jobs":[{"job_connection_info":{"attached_ssh_command":["ssh","host"],"attached_ide_url":"vscode://ssh-remote+host/path","proxied_ssh_command":["ssh","-p","2222","job@proxy"],"sshproxy_hostname":"proxy"}}]}`,
		`{"job_provisioning_data":{"hostname":"10.0.0.1","ssh_port":22}}`,
		`{"instance_type":{"name":"A100"},"hostname":"10.0.0.1"}`,
		`{"jobs":[{"job_submissions":[{"job_provisioning_data":{"hostname":"10.0.0.1"}}]}]}`,
		`{"jobs":[{"job_connection_info":{"proxied_ssh_command":["ssh","-p","2222","job@proxy"]}}]}`,
		`[]`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		out, err := redactJSON([]byte(raw))
		if err != nil {
			return
		}
		var value any
		if err := json.Unmarshal(out, &value); err != nil {
			t.Fatalf("redacted response is not JSON: %v: %s", err, out)
		}
		if problem := directHostAccess(value); problem != "" {
			t.Fatalf("response kept direct host access %q: %s", problem, out)
		}
	})
}

func hasJSONKey(raw []byte, key string) bool {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	return containsJSONKey(value, key)
}

func containsJSONKey(node any, key string) bool {
	switch v := node.(type) {
	case map[string]any:
		if _, ok := v[key]; ok {
			return true
		}
		for _, child := range v {
			if containsJSONKey(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if containsJSONKey(child, key) {
				return true
			}
		}
	}
	return false
}

func directHostAccess(node any) string {
	switch v := node.(type) {
	case map[string]any:
		if _, ok := v["job_provisioning_data"]; ok {
			return "job_provisioning_data"
		}
		if info, ok := v["job_connection_info"].(map[string]any); ok {
			if _, ok := info["attached_ssh_command"]; ok {
				return "attached_ssh_command"
			}
			if _, ok := info["attached_ide_url"]; ok {
				return "attached_ide_url"
			}
		}
		if _, isInstance := v["instance_type"]; isInstance {
			if _, ok := v["hostname"]; ok {
				return "instance hostname"
			}
		}
		for _, child := range v {
			if problem := directHostAccess(child); problem != "" {
				return problem
			}
		}
	case []any:
		for _, child := range v {
			if problem := directHostAccess(child); problem != "" {
				return problem
			}
		}
	}
	return ""
}
