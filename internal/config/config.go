package config

import (
	"encoding/json"
	"errors"
	"fmt"
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
	dstackServers        []DStackServerConfig
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

// DStackServerConfig is one dstack server the backend can provision and place workloads on.
type DStackServerConfig struct {
	Name                 string `json:"name"`
	URL                  string `json:"url"`
	Token                string `json:"token"`
	Mode                 string `json:"mode"`
	ProviderBackendsFile string `json:"provider_backends_file"`
}

// DStackServers returns the configured dstack servers in placement order.
func (c Config) DStackServers() []DStackServerConfig {
	return c.dstackServers
}

// DefaultDStackServer is the server assigned to new tenants.
func (c Config) DefaultDStackServer() string {
	if len(c.dstackServers) == 0 {
		return ""
	}
	return c.dstackServers[0].Name
}

func loadDStackServers(cfg *Config) error {
	raw := strings.TrimSpace(os.Getenv("DSTACK_SERVERS"))
	if raw == "" {
		return loadSingleDStackServer(cfg)
	}
	for name, value := range map[string]string{
		"DSTACK_URL":                 cfg.DStackURL,
		"DSTACK_ADMIN_TOKEN":         cfg.DStackAdminToken,
		"DSTACK_CUSTOMER_TOKEN_MODE": cfg.DStackTokenMode,
		"PROVIDER_BACKENDS_FILE":     cfg.ProviderBackendsPath,
	} {
		if strings.TrimSpace(value) != "" {
			return fmt.Errorf("%s must be empty when DSTACK_SERVERS is set", name)
		}
	}
	var servers []DStackServerConfig
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&servers); err != nil {
		return errors.New("DSTACK_SERVERS must be a JSON list of dstack servers")
	}
	if len(servers) == 0 {
		return errors.New("DSTACK_SERVERS must list at least one dstack server")
	}
	seen := make(map[string]struct{}, len(servers))
	for _, server := range servers {
		if err := validateDStackServer(server); err != nil {
			return err
		}
		if _, exists := seen[server.Name]; exists {
			return errors.New("DSTACK_SERVERS names a dstack server more than once")
		}
		seen[server.Name] = struct{}{}
	}
	cfg.dstackServers = servers
	return nil
}

func loadSingleDStackServer(cfg *Config) error {
	if strings.TrimSpace(cfg.DStackURL) == "" {
		return errors.New("DSTACK_URL is required")
	}
	parsed, err := url.Parse(cfg.DStackURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return errors.New("DSTACK_URL must be an HTTPS URL")
	}
	if strings.TrimSpace(cfg.DStackAdminToken) == "" {
		return errors.New("DSTACK_ADMIN_TOKEN is required")
	}
	switch cfg.DStackTokenMode {
	case "admin_refresh":
	case "shared_project":
		if strings.TrimSpace(cfg.ProviderBackendsPath) != "" {
			return errors.New("PROVIDER_BACKENDS_FILE must be empty when DSTACK_CUSTOMER_TOKEN_MODE is shared_project")
		}
	default:
		return errors.New("DSTACK_CUSTOMER_TOKEN_MODE must be admin_refresh or shared_project")
	}
	server := DStackServerConfig{
		Name:                 "default",
		URL:                  cfg.DStackURL,
		Token:                cfg.DStackAdminToken,
		Mode:                 cfg.DStackTokenMode,
		ProviderBackendsFile: cfg.ProviderBackendsPath,
	}
	if err := validateDStackServer(server); err != nil {
		return err
	}
	cfg.dstackServers = []DStackServerConfig{server}
	return nil
}

func validateDStackServer(server DStackServerConfig) error {
	if strings.TrimSpace(server.Name) == "" {
		return errors.New("each dstack server needs a name")
	}
	if strings.TrimSpace(server.URL) == "" || strings.TrimSpace(server.Token) == "" {
		return fmt.Errorf("dstack server %q needs a url and a token", server.Name)
	}
	parsed, err := url.Parse(server.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("dstack server %q must use an absolute HTTPS URL", server.Name)
	}
	switch server.Mode {
	case "admin_refresh":
	case "shared_project":
		if strings.TrimSpace(server.ProviderBackendsFile) != "" {
			return fmt.Errorf("dstack server %q must not set provider_backends_file in shared_project mode", server.Name)
		}
	default:
		return fmt.Errorf("dstack server %q mode must be admin_refresh or shared_project", server.Name)
	}
	return nil
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
	if err := loadDStackServers(&cfg); err != nil {
		return Config{}, err
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
