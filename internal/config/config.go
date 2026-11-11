package config

import (
	"errors"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	DatabaseURL          string
	ListenAddr           string
	SessionSecret        []byte
	EncryptionKey        []byte
	DStackURL            string
	DStackAdminToken     string
	DStackTokenMode      string
	ProviderBackendsPath string
	MidtransServerKey    string
	MidtransClientKey    string
	MidtransBaseURL      string
	MidtransStatusURL    string
	MidtransEnvironment  string
	PublicBaseURL        string
	TrustedProxyCIDRs    []string
	SMTPHost             string
	SMTPPort             int
	SMTPUser             string
	SMTPPassword         string
	SMTPFrom             string
	VerifyTLS            bool
}

func Load() (Config, error) {
	cfg := Config{
		DatabaseURL:          os.Getenv("DATABASE_URL"),
		ListenAddr:           os.Getenv("LISTEN_ADDR"),
		SessionSecret:        []byte(os.Getenv("SESSION_SECRET")),
		EncryptionKey:        []byte(os.Getenv("DSTACK_ENCRYPTION_KEY")),
		DStackURL:            os.Getenv("DSTACK_URL"),
		DStackAdminToken:     os.Getenv("DSTACK_ADMIN_TOKEN"),
		DStackTokenMode:      os.Getenv("DSTACK_CUSTOMER_TOKEN_MODE"),
		ProviderBackendsPath: os.Getenv("PROVIDER_BACKENDS_FILE"),
		MidtransServerKey:    os.Getenv("MIDTRANS_SERVER_KEY"),
		MidtransClientKey:    os.Getenv("MIDTRANS_CLIENT_KEY"),
		MidtransBaseURL:      os.Getenv("MIDTRANS_BASE_URL"),
		MidtransStatusURL:    os.Getenv("MIDTRANS_STATUS_URL"),
		MidtransEnvironment:  os.Getenv("MIDTRANS_ENVIRONMENT"),
		PublicBaseURL:        os.Getenv("PUBLIC_BASE_URL"),
		SMTPHost:             os.Getenv("SMTP_HOST"),
		SMTPUser:             os.Getenv("SMTP_USER"),
		SMTPPassword:         os.Getenv("SMTP_PASSWORD"),
		SMTPFrom:             os.Getenv("SMTP_FROM"),
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}
	if cfg.MidtransEnvironment == "" {
		cfg.MidtransEnvironment = "sandbox"
	}
	if cfg.DStackTokenMode != "admin_refresh" {
		return Config{}, errors.New("DSTACK_CUSTOMER_TOKEN_MODE must be admin_refresh; the installed dstack must support /api/users/refresh_token")
	}
	for _, raw := range strings.Split(os.Getenv("TRUSTED_PROXY_CIDRS"), ",") {
		cidr := strings.TrimSpace(raw)
		if cidr == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return Config{}, errors.New("TRUSTED_PROXY_CIDRS must contain only CIDR ranges")
		}
		cfg.TrustedProxyCIDRs = append(cfg.TrustedProxyCIDRs, cidr)
	}
	if cfg.VerifyTLS, _ = strconv.ParseBool(os.Getenv("SMTP_VERIFY_TLS")); !cfg.VerifyTLS {
		return Config{}, errors.New("SMTP_VERIFY_TLS must be true")
	}
	port := os.Getenv("SMTP_PORT")
	if port == "" {
		return Config{}, errors.New("SMTP_PORT is required")
	}
	var err error
	cfg.SMTPPort, err = strconv.Atoi(port)
	if err != nil || cfg.SMTPPort < 1 || cfg.SMTPPort > 65535 {
		return Config{}, errors.New("SMTP_PORT must be a valid port")
	}
	checks := map[string]string{
		"DATABASE_URL":          cfg.DatabaseURL,
		"SESSION_SECRET":        string(cfg.SessionSecret),
		"DSTACK_ENCRYPTION_KEY": string(cfg.EncryptionKey),
		"DSTACK_URL":            cfg.DStackURL,
		"DSTACK_ADMIN_TOKEN":    cfg.DStackAdminToken,
		"MIDTRANS_SERVER_KEY":   cfg.MidtransServerKey,
		"MIDTRANS_CLIENT_KEY":   cfg.MidtransClientKey,
		"PUBLIC_BASE_URL":       cfg.PublicBaseURL,
		"SMTP_HOST":             cfg.SMTPHost,
		"SMTP_USER":             cfg.SMTPUser,
		"SMTP_PASSWORD":         cfg.SMTPPassword,
		"SMTP_FROM":             cfg.SMTPFrom,
	}
	for name, value := range checks {
		if value == "" {
			return Config{}, errors.New(name + " is required")
		}
	}
	if cfg.MidtransBaseURL == "" {
		if cfg.MidtransEnvironment == "production" {
			cfg.MidtransBaseURL = "https://app.midtrans.com"
		} else {
			cfg.MidtransBaseURL = "https://app.sandbox.midtrans.com"
		}
	}
	if cfg.MidtransStatusURL == "" {
		if cfg.MidtransEnvironment == "production" {
			cfg.MidtransStatusURL = "https://api.midtrans.com"
		} else {
			cfg.MidtransStatusURL = "https://api.sandbox.midtrans.com"
		}
	}
	if len(cfg.SessionSecret) < 32 {
		return Config{}, errors.New("SESSION_SECRET must contain at least 32 bytes")
	}
	if !strings.HasPrefix(cfg.PublicBaseURL, "https://") {
		return Config{}, errors.New("PUBLIC_BASE_URL must use HTTPS")
	}
	publicURL, err := url.Parse(cfg.PublicBaseURL)
	if err != nil || publicURL.Host == "" {
		return Config{}, errors.New("PUBLIC_BASE_URL must be an absolute HTTPS URL")
	}
	for name, value := range map[string]string{"DSTACK_URL": cfg.DStackURL} {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
			return Config{}, errors.New(name + " must be an HTTPS URL")
		}
	}
	if cfg.SMTPPort != 465 {
		return Config{}, errors.New("SMTP_PORT must use implicit TLS on port 465")
	}
	if len(cfg.EncryptionKey) != 32 {
		return Config{}, errors.New("DSTACK_ENCRYPTION_KEY must contain exactly 32 bytes")
	}
	if !strings.HasPrefix(cfg.DatabaseURL, "postgres://") && !strings.HasPrefix(cfg.DatabaseURL, "postgresql://") {
		return Config{}, errors.New("DATABASE_URL must use PostgreSQL")
	}
	if cfg.MidtransEnvironment != "sandbox" && cfg.MidtransEnvironment != "production" {
		return Config{}, errors.New("MIDTRANS_ENVIRONMENT must be sandbox or production")
	}
	for name, value := range map[string]string{"MIDTRANS_BASE_URL": cfg.MidtransBaseURL, "MIDTRANS_STATUS_URL": cfg.MidtransStatusURL} {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return Config{}, errors.New(name + " must be an HTTPS URL")
		}
	}
	return cfg, nil
}
