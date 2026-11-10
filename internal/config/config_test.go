package config

import (
	"strings"
	"testing"
)

// loadEnvNames lists every environment variable Load reads. Each case
// publishes all of them, so a case never inherits a value from the ambient
// process environment.
var loadEnvNames = []string{
	"DATABASE_URL",
	"DSTACK_ADMIN_TOKEN",
	"DSTACK_CUSTOMER_TOKEN_MODE",
	"DSTACK_ENCRYPTION_KEY",
	"DSTACK_URL",
	"LISTEN_ADDR",
	"MIDTRANS_BASE_URL",
	"MIDTRANS_CLIENT_KEY",
	"MIDTRANS_ENVIRONMENT",
	"MIDTRANS_SERVER_KEY",
	"MIDTRANS_STATUS_URL",
	"PUBLIC_BASE_URL",
	"SESSION_SECRET",
	"SMTP_FROM",
	"SMTP_HOST",
	"SMTP_PASSWORD",
	"SMTP_PORT",
	"SMTP_USER",
	"SMTP_VERIFY_TLS",
}

// baseEnv is a complete and valid environment for Load. Empty values mean the
// setting is left unset so Load applies its own default.
func baseEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":               "postgres://billing:billing@localhost:5432/billing",
		"DSTACK_ADMIN_TOKEN":         "dstack-admin-token",
		"DSTACK_CUSTOMER_TOKEN_MODE": "admin_refresh",
		"DSTACK_ENCRYPTION_KEY":      strings.Repeat("k", 32),
		"DSTACK_URL":                 "https://dstack.internal",
		"LISTEN_ADDR":                "",
		"MIDTRANS_BASE_URL":          "",
		"MIDTRANS_CLIENT_KEY":        "midtrans-client-key",
		"MIDTRANS_ENVIRONMENT":       "",
		"MIDTRANS_SERVER_KEY":        "midtrans-server-key",
		"MIDTRANS_STATUS_URL":        "",
		"PUBLIC_BASE_URL":            "https://app.example.com",
		"SESSION_SECRET":             strings.Repeat("s", 32),
		"SMTP_FROM":                  "billing@example.com",
		"SMTP_HOST":                  "smtp.example.com",
		"SMTP_PASSWORD":              "smtp-password",
		"SMTP_PORT":                  "465",
		"SMTP_USER":                  "smtp-user",
		"SMTP_VERIFY_TLS":            "true",
	}
}

// applyEnv publishes the base environment with the overridden settings
// replaced. t.Setenv restores the previous process environment when the test
// ends.
func applyEnv(t *testing.T, overrides map[string]string) {
	t.Helper()
	base := baseEnv()
	for _, name := range loadEnvNames {
		value := base[name]
		if override, ok := overrides[name]; ok {
			value = override
		}
		t.Setenv(name, value)
	}
}

func wantString(t *testing.T, got, want, field string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", field, got, want)
	}
}

func TestLoadAcceptsCompleteEnvironment(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]string
		check     func(t *testing.T, cfg Config)
	}{
		{
			name: "defaults the listen address and the sandbox midtrans endpoints",
			check: func(t *testing.T, cfg Config) {
				wantString(t, cfg.ListenAddr, ":8080", "ListenAddr")
				wantString(t, cfg.MidtransEnvironment, "sandbox", "MidtransEnvironment")
				wantString(t, cfg.MidtransBaseURL, "https://app.sandbox.midtrans.com", "MidtransBaseURL")
				wantString(t, cfg.MidtransStatusURL, "https://api.sandbox.midtrans.com", "MidtransStatusURL")
			},
		},
		{
			name: "carries every configured value into the loaded config",
			check: func(t *testing.T, cfg Config) {
				wantString(t, cfg.DatabaseURL, "postgres://billing:billing@localhost:5432/billing", "DatabaseURL")
				wantString(t, cfg.DStackURL, "https://dstack.internal", "DStackURL")
				wantString(t, cfg.DStackAdminToken, "dstack-admin-token", "DStackAdminToken")
				wantString(t, cfg.DStackTokenMode, "admin_refresh", "DStackTokenMode")
				wantString(t, cfg.MidtransServerKey, "midtrans-server-key", "MidtransServerKey")
				wantString(t, cfg.MidtransClientKey, "midtrans-client-key", "MidtransClientKey")
				wantString(t, cfg.PublicBaseURL, "https://app.example.com", "PublicBaseURL")
				wantString(t, cfg.SMTPHost, "smtp.example.com", "SMTPHost")
				wantString(t, cfg.SMTPUser, "smtp-user", "SMTPUser")
				wantString(t, cfg.SMTPPassword, "smtp-password", "SMTPPassword")
				wantString(t, cfg.SMTPFrom, "billing@example.com", "SMTPFrom")
				if cfg.SMTPPort != 465 {
					t.Errorf("SMTPPort = %d, want 465", cfg.SMTPPort)
				}
				if !cfg.VerifyTLS {
					t.Error("VerifyTLS = false, want true")
				}
				if len(cfg.SessionSecret) != 32 {
					t.Errorf("len(SessionSecret) = %d, want 32", len(cfg.SessionSecret))
				}
				if len(cfg.EncryptionKey) != 32 {
					t.Errorf("len(EncryptionKey) = %d, want 32", len(cfg.EncryptionKey))
				}
			},
		},
		{
			name:      "keeps an explicit listen address",
			overrides: map[string]string{"LISTEN_ADDR": ":9090"},
			check: func(t *testing.T, cfg Config) {
				wantString(t, cfg.ListenAddr, ":9090", "ListenAddr")
			},
		},
		{
			name:      "uses the production midtrans endpoints",
			overrides: map[string]string{"MIDTRANS_ENVIRONMENT": "production"},
			check: func(t *testing.T, cfg Config) {
				wantString(t, cfg.MidtransEnvironment, "production", "MidtransEnvironment")
				wantString(t, cfg.MidtransBaseURL, "https://app.midtrans.com", "MidtransBaseURL")
				wantString(t, cfg.MidtransStatusURL, "https://api.midtrans.com", "MidtransStatusURL")
			},
		},
		{
			name: "keeps explicit midtrans endpoints",
			overrides: map[string]string{
				"MIDTRANS_BASE_URL":   "https://payments.example.com",
				"MIDTRANS_STATUS_URL": "https://status.example.com",
			},
			check: func(t *testing.T, cfg Config) {
				wantString(t, cfg.MidtransBaseURL, "https://payments.example.com", "MidtransBaseURL")
				wantString(t, cfg.MidtransStatusURL, "https://status.example.com", "MidtransStatusURL")
				wantString(t, cfg.MidtransEnvironment, "sandbox", "MidtransEnvironment")
			},
		},
		{
			name:      "accepts the postgresql database scheme",
			overrides: map[string]string{"DATABASE_URL": "postgresql://billing@localhost:5432/billing"},
			check: func(t *testing.T, cfg Config) {
				wantString(t, cfg.DatabaseURL, "postgresql://billing@localhost:5432/billing", "DatabaseURL")
			},
		},
		{
			name:      "accepts a numeric true for smtp tls verification",
			overrides: map[string]string{"SMTP_VERIFY_TLS": "1"},
			check: func(t *testing.T, cfg Config) {
				if !cfg.VerifyTLS {
					t.Error("VerifyTLS = false, want true")
				}
			},
		},
		{
			name:      "accepts a session secret longer than the minimum",
			overrides: map[string]string{"SESSION_SECRET": strings.Repeat("s", 64)},
			check: func(t *testing.T, cfg Config) {
				if len(cfg.SessionSecret) != 64 {
					t.Errorf("len(SessionSecret) = %d, want 64", len(cfg.SessionSecret))
				}
			},
		},
		{
			name:      "accepts a dstack url with a path",
			overrides: map[string]string{"DSTACK_URL": "https://dstack.internal/api"},
			check: func(t *testing.T, cfg Config) {
				wantString(t, cfg.DStackURL, "https://dstack.internal/api", "DStackURL")
			},
		},
		{
			name:      "accepts a public base url with a path",
			overrides: map[string]string{"PUBLIC_BASE_URL": "https://app.example.com/billing"},
			check: func(t *testing.T, cfg Config) {
				wantString(t, cfg.PublicBaseURL, "https://app.example.com/billing", "PublicBaseURL")
			},
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			applyEnv(t, test.overrides)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() = %v, want a valid configuration", err)
			}
			test.check(t, cfg)
		})
	}
}

func TestLoadRejectsInvalidEnvironment(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]string
		wantError string
	}{
		{
			name:      "dstack customer token mode must be admin_refresh",
			overrides: map[string]string{"DSTACK_CUSTOMER_TOKEN_MODE": "customer"},
			wantError: "DSTACK_CUSTOMER_TOKEN_MODE must be admin_refresh",
		},
		{
			name:      "dstack customer token mode must be set",
			overrides: map[string]string{"DSTACK_CUSTOMER_TOKEN_MODE": ""},
			wantError: "DSTACK_CUSTOMER_TOKEN_MODE must be admin_refresh",
		},
		{
			name:      "smtp tls verification must be enabled",
			overrides: map[string]string{"SMTP_VERIFY_TLS": "false"},
			wantError: "SMTP_VERIFY_TLS must be true",
		},
		{
			name:      "smtp tls verification must be set",
			overrides: map[string]string{"SMTP_VERIFY_TLS": ""},
			wantError: "SMTP_VERIFY_TLS must be true",
		},
		{
			name:      "smtp port is required",
			overrides: map[string]string{"SMTP_PORT": ""},
			wantError: "SMTP_PORT is required",
		},
		{
			name:      "smtp port must be a number",
			overrides: map[string]string{"SMTP_PORT": "not-a-port"},
			wantError: "SMTP_PORT must be a valid port",
		},
		{
			name:      "smtp port must not be zero",
			overrides: map[string]string{"SMTP_PORT": "0"},
			wantError: "SMTP_PORT must be a valid port",
		},
		{
			name:      "smtp port must not exceed the port range",
			overrides: map[string]string{"SMTP_PORT": "65536"},
			wantError: "SMTP_PORT must be a valid port",
		},
		{
			name:      "smtp port must use implicit tls",
			overrides: map[string]string{"SMTP_PORT": "587"},
			wantError: "SMTP_PORT must use implicit TLS on port 465",
		},
		{
			name:      "database url is required",
			overrides: map[string]string{"DATABASE_URL": ""},
			wantError: "DATABASE_URL is required",
		},
		{
			name:      "session secret is required",
			overrides: map[string]string{"SESSION_SECRET": ""},
			wantError: "SESSION_SECRET is required",
		},
		{
			name:      "dstack encryption key is required",
			overrides: map[string]string{"DSTACK_ENCRYPTION_KEY": ""},
			wantError: "DSTACK_ENCRYPTION_KEY is required",
		},
		{
			name:      "dstack url is required",
			overrides: map[string]string{"DSTACK_URL": ""},
			wantError: "DSTACK_URL is required",
		},
		{
			name:      "dstack admin token is required",
			overrides: map[string]string{"DSTACK_ADMIN_TOKEN": ""},
			wantError: "DSTACK_ADMIN_TOKEN is required",
		},
		{
			name:      "midtrans server key is required",
			overrides: map[string]string{"MIDTRANS_SERVER_KEY": ""},
			wantError: "MIDTRANS_SERVER_KEY is required",
		},
		{
			name:      "midtrans client key is required",
			overrides: map[string]string{"MIDTRANS_CLIENT_KEY": ""},
			wantError: "MIDTRANS_CLIENT_KEY is required",
		},
		{
			name:      "public base url is required",
			overrides: map[string]string{"PUBLIC_BASE_URL": ""},
			wantError: "PUBLIC_BASE_URL is required",
		},
		{
			name:      "smtp host is required",
			overrides: map[string]string{"SMTP_HOST": ""},
			wantError: "SMTP_HOST is required",
		},
		{
			name:      "smtp user is required",
			overrides: map[string]string{"SMTP_USER": ""},
			wantError: "SMTP_USER is required",
		},
		{
			name:      "smtp password is required",
			overrides: map[string]string{"SMTP_PASSWORD": ""},
			wantError: "SMTP_PASSWORD is required",
		},
		{
			name:      "smtp from is required",
			overrides: map[string]string{"SMTP_FROM": ""},
			wantError: "SMTP_FROM is required",
		},
		{
			name:      "session secret must reach the minimum length",
			overrides: map[string]string{"SESSION_SECRET": strings.Repeat("s", 31)},
			wantError: "SESSION_SECRET must contain at least 32 bytes",
		},
		{
			name:      "public base url must use https",
			overrides: map[string]string{"PUBLIC_BASE_URL": "http://app.example.com"},
			wantError: "PUBLIC_BASE_URL must use HTTPS",
		},
		{
			name:      "public base url must be absolute",
			overrides: map[string]string{"PUBLIC_BASE_URL": "https://"},
			wantError: "PUBLIC_BASE_URL must be an absolute HTTPS URL",
		},
		{
			name:      "dstack url must use https",
			overrides: map[string]string{"DSTACK_URL": "http://dstack.internal"},
			wantError: "DSTACK_URL must be an HTTPS URL",
		},
		{
			name:      "dstack url must not carry credentials",
			overrides: map[string]string{"DSTACK_URL": "https://user:pass@dstack.internal"},
			wantError: "DSTACK_URL must be an HTTPS URL",
		},
		{
			name:      "dstack url must be absolute",
			overrides: map[string]string{"DSTACK_URL": "dstack.internal"},
			wantError: "DSTACK_URL must be an HTTPS URL",
		},
		{
			name:      "encryption key must not be shorter than 32 bytes",
			overrides: map[string]string{"DSTACK_ENCRYPTION_KEY": strings.Repeat("k", 31)},
			wantError: "DSTACK_ENCRYPTION_KEY must contain exactly 32 bytes",
		},
		{
			name:      "encryption key must not be longer than 32 bytes",
			overrides: map[string]string{"DSTACK_ENCRYPTION_KEY": strings.Repeat("k", 33)},
			wantError: "DSTACK_ENCRYPTION_KEY must contain exactly 32 bytes",
		},
		{
			name:      "database url must use postgresql",
			overrides: map[string]string{"DATABASE_URL": "mysql://billing@localhost:3306/billing"},
			wantError: "DATABASE_URL must use PostgreSQL",
		},
		{
			name:      "midtrans environment must be sandbox or production",
			overrides: map[string]string{"MIDTRANS_ENVIRONMENT": "staging"},
			wantError: "MIDTRANS_ENVIRONMENT must be sandbox or production",
		},
		{
			name:      "midtrans base url must use https",
			overrides: map[string]string{"MIDTRANS_BASE_URL": "http://app.sandbox.midtrans.com"},
			wantError: "MIDTRANS_BASE_URL must be an HTTPS URL",
		},
		{
			name:      "midtrans status url must use https",
			overrides: map[string]string{"MIDTRANS_STATUS_URL": "http://api.sandbox.midtrans.com"},
			wantError: "MIDTRANS_STATUS_URL must be an HTTPS URL",
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			applyEnv(t, test.overrides)
			if _, err := Load(); err == nil {
				t.Fatal("Load() accepted an invalid environment")
			} else if !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Load() error = %q, want it to mention %q", err, test.wantError)
			}
		})
	}
}
