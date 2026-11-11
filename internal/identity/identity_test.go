package identity

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestGoogleStateRejectsTamperingAndExpiry(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	service := &Service{SessionSecret: secret}
	state, err := service.signState()
	if err != nil {
		t.Fatal(err)
	}
	if err := service.verifyState(state); err != nil {
		t.Fatalf("fresh state was rejected: %v", err)
	}
	tampered := state[:len(state)-1] + "A"
	if tampered == state {
		tampered = state[:len(state)-1] + "B"
	}
	if err := service.verifyState(tampered); err == nil {
		t.Fatal("tampered state was accepted")
	}
	other := &Service{SessionSecret: []byte("ffffffffffffffffffffffffffffffff")}
	if err := other.verifyState(state); err == nil {
		t.Fatal("state signed with another secret was accepted")
	}
	expired := signedState(t, secret, time.Now().Add(-googleStateTTL-time.Minute))
	if err := service.verifyState(expired); err == nil {
		t.Fatal("expired state was accepted")
	}
	if err := service.verifyState("not-a-state"); err == nil {
		t.Fatal("malformed state was accepted")
	}
}

func TestGoogleAuthURLBindsClientAndState(t *testing.T) {
	client := NewGoogleOAuthClient("client-id", "client-secret", "https://portal.example.com/api/auth/google/callback")
	authorizationURL := client.AuthURL("state-value")
	for _, want := range []string{
		"https://accounts.google.com/o/oauth2/v2/auth?",
		"client_id=client-id",
		"redirect_uri=https%3A%2F%2Fportal.example.com%2Fapi%2Fauth%2Fgoogle%2Fcallback",
		"state=state-value",
		"scope=openid+email+profile",
	} {
		if !strings.Contains(authorizationURL, want) {
			t.Errorf("authorization URL %q does not contain %q", authorizationURL, want)
		}
	}
}

func TestGoogleExchangeVerifiesIdentityToken(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	claims := validClaims()
	claims["email"] = "User@Example.test"
	client := googleTestClient(t, privateKey, privateKey, claims)
	identity, err := client.Exchange(context.Background(), "authorization-code")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Subject != "google-subject" {
		t.Errorf("subject = %q, want google-subject", identity.Subject)
	}
	if identity.Email != "user@example.test" {
		t.Errorf("email = %q, want a normalized verified address", identity.Email)
	}
}

func TestGoogleExchangeAcceptsAudienceList(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	claims := validClaims()
	claims["aud"] = []any{"another-client", "client-id"}
	client := googleTestClient(t, privateKey, privateKey, claims)
	if _, err := client.Exchange(context.Background(), "authorization-code"); err != nil {
		t.Fatalf("audience list was rejected: %v", err)
	}
}

func TestGoogleExchangeRejectsInvalidIdentityTokens(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		claims     map[string]any
		signKey    *rsa.PrivateKey
		publishKey *rsa.PrivateKey
	}{
		"wrong issuer":        {claims: withClaim(validClaims(), "iss", "https://evil.example.com"), signKey: privateKey, publishKey: privateKey},
		"wrong audience":      {claims: withClaim(validClaims(), "aud", "another-client"), signKey: privateKey, publishKey: privateKey},
		"expired token":       {claims: withClaim(validClaims(), "exp", time.Now().Add(-time.Minute).Unix()), signKey: privateKey, publishKey: privateKey},
		"unverified email":    {claims: withClaim(validClaims(), "email_verified", false), signKey: privateKey, publishKey: privateKey},
		"missing subject":     {claims: withClaim(validClaims(), "sub", ""), signKey: privateKey, publishKey: privateKey},
		"missing email":       {claims: withClaim(validClaims(), "email", ""), signKey: privateKey, publishKey: privateKey},
		"foreign signing key": {claims: validClaims(), signKey: otherKey, publishKey: privateKey},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			client := googleTestClient(t, test.publishKey, test.signKey, test.claims)
			if _, err := client.Exchange(context.Background(), "authorization-code"); err == nil {
				t.Fatal("invalid identity token was accepted")
			}
		})
	}
}

func TestGoogleExchangeRejectsRejectedTokenEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer server.Close()
	client := NewGoogleOAuthClient("client-id", "client-secret", "https://portal.example.com/api/auth/google/callback")
	client.TokenURL = server.URL
	if _, err := client.Exchange(context.Background(), "authorization-code"); err == nil {
		t.Fatal("rejected authorization code was accepted")
	}
}

func TestGoogleStartFailsWithoutConfiguration(t *testing.T) {
	service := &Service{SessionSecret: []byte("0123456789abcdef0123456789abcdef")}
	if _, _, err := service.StartGoogle(); err == nil {
		t.Fatal("sign-in started without a Google client")
	}
}

func validClaims() map[string]any {
	return map[string]any{
		"iss":            "https://accounts.google.com",
		"aud":            "client-id",
		"sub":            "google-subject",
		"email":          "user@example.test",
		"email_verified": true,
		"exp":            time.Now().Add(time.Minute).Unix(),
	}
}

func withClaim(claims map[string]any, key string, value any) map[string]any {
	copied := make(map[string]any, len(claims))
	for name, current := range claims {
		copied[name] = current
	}
	copied[key] = value
	return copied
}

func googleTestClient(t *testing.T, publishKey, signKey *rsa.PrivateKey, claims map[string]any) *GoogleOAuthClient {
	t.Helper()
	keysServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		exponent := big.NewInt(int64(publishKey.PublicKey.E)).Bytes()
		payload := map[string]any{"keys": []map[string]string{{
			"kid": "test-key",
			"n":   base64.RawURLEncoding.EncodeToString(publishKey.PublicKey.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(exponent),
		}}}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(keysServer.Close)
	token := signIdentityToken(t, signKey, "test-key", claims)
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": token})
	}))
	t.Cleanup(tokenServer.Close)
	client := NewGoogleOAuthClient("client-id", "client-secret", "https://portal.example.com/api/auth/google/callback")
	client.TokenURL = tokenServer.URL
	client.CertsURL = keysServer.URL
	client.HTTPClient = keysServer.Client()
	return client
}

func signIdentityToken(t *testing.T, key *rsa.PrivateKey, keyID string, claims map[string]any) string {
	t.Helper()
	headerJSON, err := json.Marshal(map[string]string{"alg": "RS256", "kid": keyID, "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func signedState(t *testing.T, secret []byte, issued time.Time) string {
	t.Helper()
	nonce := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef"))
	payload := nonce + "." + strconv.FormatInt(issued.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
