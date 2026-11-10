package dstack

import (
	"strings"
	"testing"
)

func TestParseProviderBackendsAcceptsEachSupportedType(t *testing.T) {
	cases := []struct {
		backendType BackendType
		config      string
	}{
		{BackendAWS, "type: aws\ncreds:\n  type: access_key\n  access_key: aws-test-access\n  secret_key: aws-test-secret\n"},
		{BackendGCP, "type: gcp\nproject_id: inference-project\ncreds:\n  type: service_account\n  data: '{\"private_key\":\"gcp-test-secret\"}'\n"},
		{BackendNebius, "type: nebius\ncreds:\n  type: service_account\n  service_account_id: service-account-id\n  public_key_id: public-key-id\n  private_key_content: nebius-test-secret\n"},
		{BackendRunPod, "type: runpod\ncreds:\n  type: api_key\n  api_key: runpod-test-secret\n"},
	}
	configs := make([]string, 0, len(cases))
	for _, test := range cases {
		t.Run(string(test.backendType), func(t *testing.T) {
			got, err := ParseProviderBackends([]byte("backends:\n  - " + strings.ReplaceAll(test.config, "\n", "\n    ")))
			if err != nil {
				t.Fatal(err)
			}
			types := got.Types()
			if len(types) != 1 || types[0] != test.backendType {
				t.Fatalf("Types() = %v, want [%q]", types, test.backendType)
			}
			backend, ok := got.Get(test.backendType)
			if !ok || backend.Type != test.backendType || !strings.Contains(backend.YAML, "type: "+string(test.backendType)) {
				t.Errorf("Get(%q) = %#v, %v", test.backendType, backend, ok)
			}
			if got.Allows("ssh") || !got.Allows(string(test.backendType)) {
				t.Fatal("provider allowlist did not match the configured backend type")
			}
		})
		configs = append(configs, "  - "+strings.ReplaceAll(strings.TrimSuffix(test.config, "\n"), "\n", "\n    ")+"\n")
	}
	all, err := ParseProviderBackends([]byte("backends:\n" + strings.Join(configs, "")))
	if err != nil || len(all.Types()) != len(cases) {
		t.Fatalf("all provider types config = %v, %v", all.Types(), err)
	}
}

func TestParseProviderBackendsAcceptsEmptyConfiguration(t *testing.T) {
	for _, input := range []string{"", "{}", "backends: []\n"} {
		got, err := ParseProviderBackends([]byte(input))
		if err != nil || len(got.Types()) != 0 {
			t.Errorf("ParseProviderBackends(%q) = %v, %v", input, got.Types(), err)
		}
	}
	if got, err := ParseProviderBackends([]byte("backends:\n  - type: aws\n")); err != nil || len(got.Types()) != 1 {
		t.Fatalf("one-provider config = %v, %v", got.Types(), err)
	}
}

func TestParseProviderBackendsRejectsInvalidConfigurationWithoutLeakingSecrets(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{"invalid yaml", "backends: [secret: unterminated"},
		{"unsupported type", "backends:\n  - type: azure\n    api_key: not-a-supported-provider"},
		{"missing type", "backends:\n  - creds:\n      api_key: missing-type-secret"},
		{"duplicate type", "backends:\n  - type: aws\n    secret_key: first-secret\n  - type: aws\n    secret_key: second-secret"},
		{"multiple documents", "backends: []\n---\nbackends: []\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseProviderBackends([]byte(test.body))
			if err == nil {
				t.Fatal("invalid provider config was accepted")
			}
			for _, secret := range []string{"not-a-supported-provider", "missing-type-secret", "first-secret", "second-secret", "unterminated"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error exposed configuration input %q", err)
				}
			}
		})
	}
}
