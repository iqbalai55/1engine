package config

import (
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
)

type Config struct {
	DatabaseURL          string
	ListenAddr           string
	SessionSecret        []byte
	AdminUsername        string
	AdminPassword        string
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
	GoogleClientID       string
	GoogleClientSecret   string
	GoogleRedirectURL    string
}

func Load() (Config, error) {
	cfg := Config{
		DatabaseURL:          os.Getenv("DATABASE_URL"),
		ListenAddr:           os.Getenv("LISTEN_ADDR"),
		SessionSecret:        []byte(os.Getenv("SESSION_SECRET")),
		AdminUsername:        os.Getenv("ADMIN_USERNAME"),
		AdminPassword:        os.Getenv("ADMIN_PASSWORD"),
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
		GoogleClientID:       os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret:   os.Getenv("GOOGLE_CLIENT_SECRET"),
		GoogleRedirectURL:    os.Getenv("GOOGLE_REDIRECT_URL"),
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
	checks := map[string]string{
		"DATABASE_URL":          cfg.DatabaseURL,
		"SESSION_SECRET":        string(cfg.SessionSecret),
		"DSTACK_ENCRYPTION_KEY": string(cfg.EncryptionKey),
		"DSTACK_URL":            cfg.DStackURL,
		"DSTACK_ADMIN_TOKEN":    cfg.DStackAdminToken,
		"MIDTRANS_SERVER_KEY":   cfg.MidtransServerKey,
		"MIDTRANS_CLIENT_KEY":   cfg.MidtransClientKey,
		"PUBLIC_BASE_URL":       cfg.PublicBaseURL,
		"GOOGLE_CLIENT_ID":      cfg.GoogleClientID,
		"GOOGLE_CLIENT_SECRET":  cfg.GoogleClientSecret,
		"GOOGLE_REDIRECT_URL":   cfg.GoogleRedirectURL,
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
	if (cfg.AdminUsername == "") != (cfg.AdminPassword == "") {
		return Config{}, errors.New("ADMIN_USERNAME and ADMIN_PASSWORD must be set together")
	}
	if cfg.AdminPassword != "" && len(cfg.AdminPassword) < 12 {
		return Config{}, errors.New("ADMIN_PASSWORD must contain at least 12 bytes")
	}
	if !strings.HasPrefix(cfg.PublicBaseURL, "https://") {
		return Config{}, errors.New("PUBLIC_BASE_URL must use HTTPS")
	}
	publicURL, err := url.Parse(cfg.PublicBaseURL)
	if err != nil || publicURL.Host == "" {
		return Config{}, errors.New("PUBLIC_BASE_URL must be an absolute HTTPS URL")
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
	for name, value := range map[string]string{
		"DSTACK_URL":          cfg.DStackURL,
		"MIDTRANS_BASE_URL":   cfg.MidtransBaseURL,
		"MIDTRANS_STATUS_URL": cfg.MidtransStatusURL,
		"GOOGLE_REDIRECT_URL": cfg.GoogleRedirectURL,
	} {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
			return Config{}, errors.New(name + " must be an HTTPS URL")
		}
	}
	return cfg, nil
}
