package identity

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	googleAuthorizeURL = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL     = "https://oauth2.googleapis.com/token"
	googleCertsURL     = "https://www.googleapis.com/oauth2/v3/certs"
)

// GoogleIdentity is the stable part of a Google account the portal stores.
type GoogleIdentity struct {
	Subject string
	Email   string
}

type GoogleOAuth interface {
	AuthURL(state string) string
	Exchange(ctx context.Context, code string) (GoogleIdentity, error)
}

type GoogleOAuthClient struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	HTTPClient   *http.Client
	Now          func() time.Time
	TokenURL     string
	CertsURL     string

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	keysExp time.Time
}

func NewGoogleOAuthClient(clientID, clientSecret, redirectURL string) *GoogleOAuthClient {
	return &GoogleOAuthClient{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		HTTPClient:   &http.Client{Timeout: 10 * time.Second},
		Now:          time.Now,
	}
}

func (c *GoogleOAuthClient) AuthURL(state string) string {
	query := url.Values{
		"client_id":     {c.ClientID},
		"redirect_uri":  {c.RedirectURL},
		"response_type": {"code"},
		"scope":         {"openid email profile"},
		"state":         {state},
		"prompt":        {"select_account"},
	}
	return googleAuthorizeURL + "?" + query.Encode()
}

func (c *GoogleOAuthClient) Exchange(ctx context.Context, code string) (GoogleIdentity, error) {
	if strings.TrimSpace(code) == "" {
		return GoogleIdentity{}, errors.New("google authorization code is required")
	}
	form := url.Values{
		"code":          {code},
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
		"redirect_uri":  {c.RedirectURL},
		"grant_type":    {"authorization_code"},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return GoogleIdentity{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.client().Do(request)
	if err != nil {
		return GoogleIdentity{}, fmt.Errorf("google token exchange failed: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return GoogleIdentity{}, err
	}
	if response.StatusCode != http.StatusOK {
		return GoogleIdentity{}, errors.New("google token exchange was rejected")
	}
	var payload struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return GoogleIdentity{}, errors.New("google token response was unreadable")
	}
	return c.verifyIDToken(ctx, payload.IDToken)
}

func (c *GoogleOAuthClient) verifyIDToken(ctx context.Context, raw string) (GoogleIdentity, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return GoogleIdentity{}, errors.New("google identity token is malformed")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return GoogleIdentity{}, errors.New("google identity token header is malformed")
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil || header.Algorithm != "RS256" {
		return GoogleIdentity{}, errors.New("google identity token algorithm is not supported")
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return GoogleIdentity{}, errors.New("google identity token claims are malformed")
	}
	var claims struct {
		Issuer        string `json:"iss"`
		Audience      any    `json:"aud"`
		Subject       string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Expires       int64  `json:"exp"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return GoogleIdentity{}, errors.New("google identity token claims are malformed")
	}
	if claims.Issuer != "accounts.google.com" && claims.Issuer != "https://accounts.google.com" {
		return GoogleIdentity{}, errors.New("google identity token issuer is not Google")
	}
	if !audienceMatches(claims.Audience, c.ClientID) {
		return GoogleIdentity{}, errors.New("google identity token audience does not match this client")
	}
	if claims.Expires <= c.now().Unix() {
		return GoogleIdentity{}, errors.New("google identity token has expired")
	}
	if !claims.EmailVerified || claims.Subject == "" || claims.Email == "" {
		return GoogleIdentity{}, errors.New("google account has no verified email")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return GoogleIdentity{}, errors.New("google identity token signature is malformed")
	}
	key, err := c.signingKey(ctx, header.KeyID)
	if err != nil {
		return GoogleIdentity{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return GoogleIdentity{}, errors.New("google identity token signature is invalid")
	}
	return GoogleIdentity{Subject: claims.Subject, Email: strings.ToLower(strings.TrimSpace(claims.Email))}, nil
}

func audienceMatches(audience any, clientID string) bool {
	switch value := audience.(type) {
	case string:
		return value == clientID
	case []any:
		for _, entry := range value {
			if token, ok := entry.(string); ok && token == clientID {
				return true
			}
		}
	}
	return false
}

func (c *GoogleOAuthClient) signingKey(ctx context.Context, keyID string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if key, ok := c.keys[keyID]; ok && c.now().Before(c.keysExp) {
		return key, nil
	}
	if err := c.refreshKeys(ctx); err != nil {
		return nil, err
	}
	key, ok := c.keys[keyID]
	if !ok {
		return nil, errors.New("google signing key is not published")
	}
	return key, nil
}

func (c *GoogleOAuthClient) refreshKeys(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.certsURL(), nil)
	if err != nil {
		return err
	}
	response, err := c.client().Do(request)
	if err != nil {
		return fmt.Errorf("google signing keys are unavailable: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return errors.New("google signing keys were rejected")
	}
	var payload struct {
		Keys []struct {
			KeyID string `json:"kid"`
			N     string `json:"n"`
			E     string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return errors.New("google signing keys were unreadable")
	}
	keys := make(map[string]*rsa.PublicKey, len(payload.Keys))
	for _, entry := range payload.Keys {
		modulus, err := base64.RawURLEncoding.DecodeString(entry.N)
		if err != nil || len(modulus) == 0 {
			continue
		}
		exponent, err := base64.RawURLEncoding.DecodeString(entry.E)
		if err != nil {
			continue
		}
		value := new(big.Int).SetBytes(exponent)
		if !value.IsInt64() || value.Int64() < 3 || value.Int64() > math.MaxInt32 {
			continue
		}
		keys[entry.KeyID] = &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: int(value.Int64())}
	}
	if len(keys) == 0 {
		return errors.New("google published no usable signing keys")
	}
	c.keys = keys
	c.keysExp = c.now().Add(time.Hour)
	return nil
}

func (c *GoogleOAuthClient) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *GoogleOAuthClient) tokenURL() string {
	if c.TokenURL != "" {
		return c.TokenURL
	}
	return googleTokenURL
}

func (c *GoogleOAuthClient) certsURL() string {
	if c.CertsURL != "" {
		return c.CertsURL
	}
	return googleCertsURL
}

func (c *GoogleOAuthClient) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
