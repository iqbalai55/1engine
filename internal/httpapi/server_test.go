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
	startError    error
	startCalls    int
	completeError error
	completeCalls int
	code          string
	state         string
}

type nextRecorder struct{ calls int }

func (n *nextRecorder) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	n.calls++
	w.WriteHeader(http.StatusNoContent)
}

func (s *identityStub) StartGoogle() (string, string, error) {
	s.startCalls++
	if s.startError != nil {
		return "", "", s.startError
	}
	return "https://accounts.google.com/o/oauth2/v2/auth", "signed-state", nil
}

func (s *identityStub) CompleteGoogle(_ context.Context, code, state string) (identity.Session, error) {
	s.completeCalls++
	s.code = code
	s.state = state
	return identity.Session{Token: "session-token", UserID: "user-1"}, s.completeError
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
	// The compute proxy has one customer prefix. The old /v1/dstack alias is gone.
	removed := httptest.NewRecorder()
	router.ServeHTTP(removed, httptest.NewRequest(http.MethodPost, "/v1/dstack/runs/list", nil))
	if removed.Code != http.StatusNotFound {
		t.Fatalf("removed /v1/dstack prefix status = %d, want 404", removed.Code)
	}
	customerProxy := httptest.NewRecorder()
	router.ServeHTTP(customerProxy, httptest.NewRequest(http.MethodPost, "/v1/api/runs/list", nil))
	if customerProxy.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /v1/api status = %d, want 401", customerProxy.Code)
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

func TestTenantRateLimitSharesBucketAcrossClientIPs(t *testing.T) {
	server := &Server{Limiter: newRequestLimiter()}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), principalKey, principal{TenantID: "tenant-a", Kind: "api"}))
		c.Next()
	})
	router.GET("/", server.rateLimitBy("proxy", 2, time.Minute, tenantKey), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	want := []int{http.StatusNoContent, http.StatusNoContent, http.StatusTooManyRequests}
	for i, addr := range []string{"192.0.2.41:1234", "192.0.2.42:1234", "192.0.2.43:1234"} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.RemoteAddr = addr
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != want[i] {
			t.Fatalf("request %d from %s status = %d, want %d", i, addr, recorder.Code, want[i])
		}
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

func TestRemovedPasswordRoutesAreGone(t *testing.T) {
	router := (&Server{}).Router()
	for _, path := range []string{"/v1/signup", "/v1/login", "/v1/email-verifications/confirm"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		if recorder.Code != http.StatusNotFound {
			t.Errorf("removed route %s status = %d, want 404", path, recorder.Code)
		}
	}
}

func TestGoogleStartReturnsAuthorizationURL(t *testing.T) {
	identity := &identityStub{}
	router := (&Server{Identity: identity}).Router()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/auth/google/start", nil))
	if recorder.Code != http.StatusOK || identity.startCalls != 1 {
		t.Fatalf("google start response = %d with %d identity calls, want 200 and one call", recorder.Code, identity.startCalls)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "accounts.google.com") || !strings.Contains(body, "signed-state") {
		t.Fatalf("google start body = %q, want authorization URL and state", body)
	}
}

func TestGoogleStartFailsClosedWithoutIdentity(t *testing.T) {
	router := (&Server{Identity: &identityStub{startError: errors.New("google is not configured")}}).Router()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/auth/google/start", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable google start status = %d, want 503", recorder.Code)
	}
}

func TestGoogleCallbackReturnsSession(t *testing.T) {
	identity := &identityStub{}
	router := (&Server{Identity: identity}).Router()
	request := httptest.NewRequest(http.MethodPost, "/v1/auth/google/callback", strings.NewReader(`{"code":"code-1","state":"state-1"}`))
	request.RemoteAddr = "192.0.2.21:1234"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || identity.completeCalls != 1 {
		t.Fatalf("google callback response = %d with %d identity calls, want 200 and one call", recorder.Code, identity.completeCalls)
	}
	if identity.code != "code-1" || identity.state != "state-1" {
		t.Fatalf("google callback received (%q, %q), want the submitted code and state", identity.code, identity.state)
	}
	if !strings.Contains(recorder.Body.String(), "session-token") {
		t.Fatalf("google callback body = %q, want the portal session", recorder.Body.String())
	}
}

func TestGoogleCallbackRejectsMalformedBody(t *testing.T) {
	identity := &identityStub{}
	router := (&Server{Identity: identity}).Router()
	for _, body := range []string{`{`, `{"code":"code-1","state":"state-1","extra":true}`} {
		request := httptest.NewRequest(http.MethodPost, "/v1/auth/google/callback", strings.NewReader(body))
		request.RemoteAddr = "192.0.2.22:1234"
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("google callback body %q status = %d, want 400", body, recorder.Code)
		}
	}
	if identity.completeCalls != 0 {
		t.Fatalf("malformed callback reached identity service %d times", identity.completeCalls)
	}
}

func TestGoogleCallbackRejectsFailedExchange(t *testing.T) {
	router := (&Server{Identity: &identityStub{completeError: errors.New("exchange rejected")}}).Router()
	request := httptest.NewRequest(http.MethodPost, "/v1/auth/google/callback", strings.NewReader(`{"code":"code-1","state":"state-1"}`))
	request.RemoteAddr = "192.0.2.23:1234"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("failed google callback status = %d, want 401", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "exchange rejected") {
		t.Fatal("callback response leaked the identity failure detail")
	}
}

func TestGoogleStartRateLimitRejectsExcessRequests(t *testing.T) {
	limiter := newRequestLimiter()
	for range 60 {
		if !limiter.Allow("google-start:192.0.2.10", 60, time.Minute, time.Now()) {
			t.Fatal("failed to prepare full google start quota")
		}
	}
	identity := &identityStub{}
	router := (&Server{Limiter: limiter, Identity: identity}).Router()
	request := httptest.NewRequest(http.MethodGet, "/v1/auth/google/start", nil)
	request.RemoteAddr = "192.0.2.10:1234"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("excess google start status = %d, want 429", recorder.Code)
	}
	if identity.startCalls != 0 {
		t.Fatalf("rate-limited google start reached identity service %d times", identity.startCalls)
	}
	if recorder.Header().Get("Retry-After") != "60" {
		t.Fatalf("Retry-After = %q, want 60", recorder.Header().Get("Retry-After"))
	}
}

func TestGoogleStartRateLimitStopsAfterConfiguredRequests(t *testing.T) {
	identity := &identityStub{}
	router := (&Server{Identity: identity}).Router()
	for i := 0; i < 60; i++ {
		request := httptest.NewRequest(http.MethodGet, "/v1/auth/google/start", nil)
		request.RemoteAddr = "192.0.2.20:1234"
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("allowed google start %d status = %d, want 200", i+1, recorder.Code)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/auth/google/start", nil)
	request.RemoteAddr = "192.0.2.20:1234"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("google start after the allowed quota status = %d, want 429", recorder.Code)
	}
	if identity.startCalls != 60 {
		t.Fatalf("identity start calls = %d, want 60 calls before the rate limit", identity.startCalls)
	}
}

func TestForwardedForIsIgnoredWithoutTrustedProxy(t *testing.T) {
	identity := &identityStub{}
	router := (&Server{Identity: identity}).Router()
	statuses := []int{}
	for i := 0; i < 61; i++ {
		request := httptest.NewRequest(http.MethodGet, "/v1/auth/google/start", nil)
		request.RemoteAddr = "203.0.113.9:1234"
		request.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i+1))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		statuses = append(statuses, recorder.Code)
	}
	if statuses[60] != http.StatusTooManyRequests {
		t.Fatalf("statuses with rotating X-Forwarded-For = %v, want the 61st sign-in limited", statuses)
	}
}

func TestForwardedForIsHonoredFromTrustedProxy(t *testing.T) {
	identity := &identityStub{}
	router := (&Server{Identity: identity, TrustedProxies: []string{"203.0.113.0/24"}}).Router()
	for i := 0; i < 60; i++ {
		request := httptest.NewRequest(http.MethodGet, "/v1/auth/google/start", nil)
		request.RemoteAddr = "203.0.113.9:1234"
		request.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i+1))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("google start %d from distinct forwarded client status = %d, want 200", i+1, recorder.Code)
		}
	}
}
