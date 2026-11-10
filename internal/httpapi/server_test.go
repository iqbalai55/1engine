package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"billing-backend/internal/identity"
	"github.com/gin-gonic/gin"
)

type identityStub struct {
	signUpError error
	email       string
	password    string
	signUpCalls int
	loginCalls  int
	loginError  error
}

type nextRecorder struct{ calls int }

func (n *nextRecorder) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	n.calls++
	w.WriteHeader(http.StatusNoContent)
}

func (s *identityStub) SignUp(_ context.Context, email, password string) error {
	s.signUpCalls++
	s.email = email
	s.password = password
	return s.signUpError
}

func (*identityStub) VerifyEmail(context.Context, string) (string, error) {
	return "", errors.New("unexpected VerifyEmail call")
}

func (s *identityStub) Login(context.Context, string, string) (identity.Session, error) {
	s.loginCalls++
	return identity.Session{Token: "session-token"}, s.loginError
}

func (*identityStub) Authenticate(context.Context, string) (string, error) {
	return "", errors.New("unexpected Authenticate call")
}

func (*identityStub) IsProvider(context.Context, string) (bool, error) {
	return false, errors.New("unexpected IsProvider call")
}

func (*identityStub) Email(context.Context, string) (string, error) {
	return "", errors.New("unexpected Email call")
}

func (*identityStub) RevokeSession(context.Context, string) error {
	return errors.New("unexpected RevokeSession call")
}

func TestRouterHealthAndProtectedRoutes(t *testing.T) {
	router := (&Server{}).Router()

	health := httptest.NewRecorder()
	router.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	if health.Code != http.StatusOK || health.Body.String() != `{"status":"ok"}` {
		t.Fatalf("health response = %d %q, want 200 and healthy status", health.Code, health.Body.String())
	}

	protected := httptest.NewRecorder()
	router.ServeHTTP(protected, httptest.NewRequest(http.MethodGet, "/v1/me", nil))
	if protected.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /v1/me status = %d, want 401", protected.Code)
	}
}

func TestSignupRejectsMalformedOrUnknownRequestFields(t *testing.T) {
	identity := &identityStub{}
	router := (&Server{Identity: identity}).Router()
	for _, body := range []string{`{`, `{"email":"user@example.test","password":"long-enough-password","extra":true}`} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/signup", strings.NewReader(body))
		request.RemoteAddr = "192.0.2.10:1234"
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("signup body %q status = %d, want 400", body, recorder.Code)
		}
	}
	if identity.signUpCalls != 0 {
		t.Fatalf("malformed signup reached identity service %d times", identity.signUpCalls)
	}
}

func TestSignupReturnsAcceptedAfterIdentityServiceAcceptsRequest(t *testing.T) {
	identity := &identityStub{}
	router := (&Server{Identity: identity}).Router()
	request := httptest.NewRequest(http.MethodPost, "/v1/signup", strings.NewReader(`{"email":"user@example.test","password":"long-enough-password"}`))
	request.RemoteAddr = "192.0.2.11:1234"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("signup status = %d, want 202", recorder.Code)
	}
	if identity.email != "user@example.test" || identity.password != "long-enough-password" {
		t.Fatalf("signup received (%q, %q), want submitted credentials", identity.email, identity.password)
	}
}

func TestSignupReturnsBadRequestWhenIdentityServiceRejectsRequest(t *testing.T) {
	identity := &identityStub{signUpError: errors.New("signup rejected")}
	router := (&Server{Identity: identity}).Router()
	request := httptest.NewRequest(http.MethodPost, "/v1/signup", strings.NewReader(`{"email":"user@example.test","password":"long-enough-password"}`))
	request.RemoteAddr = "192.0.2.12:1234"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("rejected signup status = %d, want 400", recorder.Code)
	}
}

func TestLoginRateLimitAdvancesAllowedRequests(t *testing.T) {
	identity := &identityStub{}
	router := (&Server{Identity: identity}).Router()
	request := httptest.NewRequest(http.MethodPost, "/v1/login", strings.NewReader(`{"email":"user@example.test","password":"long-enough-password"}`))
	request.RemoteAddr = "192.0.2.21:1234"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || identity.loginCalls != 1 {
		t.Fatalf("allowed login response = %d with %d identity calls, want 200 and one call", recorder.Code, identity.loginCalls)
	}
}

func TestRequestLimiterDoesNotEvictAtExactThreshold(t *testing.T) {
	limiter := newRequestLimiter()
	start := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	for i := 0; i < 9_999; i++ {
		limiter.windows[fmt.Sprintf("old:%d", i)] = rateWindow{started: start, count: 1}
	}
	limiter.windows["active"] = rateWindow{started: start.Add(3 * time.Minute), count: 1}
	if !limiter.Allow("new", 1, time.Minute, start.Add(2*time.Minute)) {
		t.Fatal("new request was rejected")
	}
	if len(limiter.windows) != 10_001 {
		t.Fatalf("window count at exact threshold = %d, want 10001", len(limiter.windows))
	}
}

func TestRequestLimiterEvictsLastExpiredBucket(t *testing.T) {
	limiter := newRequestLimiter()
	start := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	for i := 0; i < 10_001; i++ {
		limiter.windows[fmt.Sprintf("old:%d", i)] = rateWindow{started: start, count: 1}
	}
	if !limiter.Allow("new", 1, time.Minute, start.Add(2*time.Minute)) {
		t.Fatal("new bucket after eviction was rejected")
	}
	if _, ok := limiter.windows["old:10000"]; ok {
		t.Fatal("last expired bucket remained after the sweep")
	}
}

func TestRequestLimiterRetainsBucketAtExpiryBoundary(t *testing.T) {
	limiter := newRequestLimiter()
	start := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	for i := 0; i < 10_001; i++ {
		limiter.windows[fmt.Sprintf("old:%d", i)] = rateWindow{started: start, count: 1}
	}
	if !limiter.Allow("new", 1, time.Minute, start.Add(time.Minute)) {
		t.Fatal("new bucket was rejected")
	}
	if _, ok := limiter.windows["old:0"]; !ok {
		t.Fatal("bucket exactly one window old was removed from cleanup cache")
	}
}

func TestRequestLimiterRetainsActiveBucketAfterCleanup(t *testing.T) {
	limiter := newRequestLimiter()
	start := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	for i := 0; i < 10_001; i++ {
		limiter.windows[fmt.Sprintf("expired:%d", i)] = rateWindow{started: start, count: 1}
	}
	limiter.windows["active"] = rateWindow{started: start.Add(time.Minute), count: 1}
	if !limiter.Allow("new", 1, time.Minute, start.Add(2*time.Minute)) {
		t.Fatal("new bucket after cleanup was rejected")
	}
	if _, ok := limiter.windows["active"]; !ok {
		t.Fatal("cleanup removed active bucket")
	}
	if _, ok := limiter.windows["expired:10000"]; ok {
		t.Fatal("cleanup retained expired bucket")
	}
}

func TestRateLimitAdvancesAllowedHandler(t *testing.T) {
	server := &Server{Limiter: newRequestLimiter()}
	handler := server.rateLimit("custom", 1, time.Minute)
	next := &nextRecorder{}
	router := gin.New()
	router.GET("/", handler, func(c *gin.Context) { next.ServeHTTP(c.Writer, c.Request) })
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "192.0.2.31:1234"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || next.calls != 1 {
		t.Fatalf("allowed request status = %d with %d downstream calls, want 204 and one call", recorder.Code, next.calls)
	}
}

func TestRequestLimiterEnforcesLimitAndWindowExpiry(t *testing.T) {
	limiter := newRequestLimiter()
	start := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	if !limiter.Allow("login:192.0.2.1", 2, time.Minute, start) {
		t.Fatal("first request was rejected")
	}
	if !limiter.Allow("login:192.0.2.1", 2, time.Minute, start.Add(time.Second)) {
		t.Fatal("request within limit was rejected")
	}
	if limiter.Allow("login:192.0.2.1", 2, time.Minute, start.Add(2*time.Second)) {
		t.Fatal("request over limit was accepted")
	}
	if !limiter.Allow("login:192.0.2.1", 2, time.Minute, start.Add(time.Minute)) {
		t.Fatal("request after window expiry was rejected")
	}
}

func TestSignupRateLimitRejectsExcessRequests(t *testing.T) {
	limiter := newRequestLimiter()
	for range 5 {
		if !limiter.Allow("signup:192.0.2.10", 5, time.Hour, time.Now()) {
			t.Fatal("failed to prepare full signup quota")
		}
	}
	identity := &identityStub{}
	router := (&Server{Limiter: limiter, Identity: identity}).Router()
	request := httptest.NewRequest(http.MethodPost, "/v1/signup", strings.NewReader(`{"email":"user@example.test","password":"long-enough-password"}`))
	request.RemoteAddr = "192.0.2.10:1234"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("excess signup status = %d, want 429", recorder.Code)
	}
	if identity.signUpCalls != 0 {
		t.Fatalf("rate-limited signup reached identity service %d times", identity.signUpCalls)
	}
	if recorder.Header().Get("Retry-After") != "3600" {
		t.Fatalf("Retry-After = %q, want 3600", recorder.Header().Get("Retry-After"))
	}
}

func TestSignupRateLimitCallsIdentityForAllowedRequest(t *testing.T) {
	identity := &identityStub{}
	router := (&Server{Identity: identity}).Router()
	request := httptest.NewRequest(http.MethodPost, "/v1/signup", strings.NewReader(`{"email":"user@example.test","password":"long-enough-password"}`))
	request.RemoteAddr = "192.0.2.13:1234"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted || identity.signUpCalls != 1 {
		t.Fatalf("allowed signup response = %d with %d identity calls, want 202 and one call", recorder.Code, identity.signUpCalls)
	}
}

func TestLoginRateLimitStopsAfterConfiguredRequests(t *testing.T) {
	identity := &identityStub{}
	router := (&Server{Identity: identity}).Router()
	for i := 0; i < 10; i++ {
		request := httptest.NewRequest(http.MethodPost, "/v1/login", strings.NewReader(`{"email":"user@example.test","password":"long-enough-password"}`))
		request.RemoteAddr = "192.0.2.20:1234"
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("allowed login %d status = %d, want 200", i+1, recorder.Code)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/login", strings.NewReader(`{"email":"user@example.test","password":"long-enough-password"}`))
	request.RemoteAddr = "192.0.2.20:1234"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("login after the allowed quota status = %d, want 429", recorder.Code)
	}
	if identity.loginCalls != 10 {
		t.Fatalf("identity login calls = %d, want 10 calls before the rate limit", identity.loginCalls)
	}
}

func TestLoginRateLimitCallsIdentityForAllowedRequest(t *testing.T) {
	identity := &identityStub{}
	router := (&Server{Identity: identity}).Router()
	request := httptest.NewRequest(http.MethodPost, "/v1/login", strings.NewReader(`{"email":"user@example.test","password":"long-enough-password"}`))
	request.RemoteAddr = "192.0.2.21:1234"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || identity.loginCalls != 1 {
		t.Fatalf("allowed login response = %d with %d identity calls, want 200 and one call", recorder.Code, identity.loginCalls)
	}
}

func TestLoginRejectsMalformedRequestBody(t *testing.T) {
	identity := &identityStub{}
	router := (&Server{Identity: identity}).Router()
	request := httptest.NewRequest(http.MethodPost, "/v1/login", strings.NewReader(`{`))
	request.RemoteAddr = "192.0.2.22:1234"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("malformed login status = %d, want 400", recorder.Code)
	}
	if identity.loginCalls != 0 {
		t.Fatalf("malformed login reached identity service %d times", identity.loginCalls)
	}
}

func TestLoginRejectsInvalidCredentials(t *testing.T) {
	identity := &identityStub{loginError: errors.New("invalid credentials")}
	router := (&Server{Identity: identity}).Router()
	request := httptest.NewRequest(http.MethodPost, "/v1/login", strings.NewReader(`{"email":"user@example.test","password":"incorrect-password"}`))
	request.RemoteAddr = "192.0.2.23:1234"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("invalid login status = %d, want 401", recorder.Code)
	}
}
