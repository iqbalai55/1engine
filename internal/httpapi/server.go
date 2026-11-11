package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"billing-backend/internal/apikey"
	"billing-backend/internal/compute"
	"billing-backend/internal/domain"
	"billing-backend/internal/dstack"
	"billing-backend/internal/identity"
	"billing-backend/internal/ledger"
	"billing-backend/internal/payments"
	"billing-backend/internal/pricing"
	"billing-backend/internal/tenant"
	"github.com/gin-gonic/gin"
)

type Server struct {
	Identity         identityService
	Keys             *apikey.Service
	Ledger           *ledger.Service
	Meter            *compute.Meter
	Tenants          *tenant.Service
	Payments         *payments.Service
	Pricing          *pricing.Service
	DStack           *dstack.Proxy
	PaymentServerKey string
	Limiter          *requestLimiter
	TrustedProxies   []string
}

type identityService interface {
	SignUp(context.Context, string, string) error
	VerifyEmail(context.Context, string) (string, error)
	Login(context.Context, string, string) (identity.Session, error)
	Authenticate(context.Context, string) (string, error)
	IsProvider(context.Context, string) (bool, error)
	Email(context.Context, string) (string, error)
	RevokeSession(context.Context, string) error
}

type rateWindow struct {
	started time.Time
	count   int
}
type requestLimiter struct {
	mu      sync.Mutex
	windows map[string]rateWindow
}

func newRequestLimiter() *requestLimiter {
	return &requestLimiter{windows: make(map[string]rateWindow)}
}
func (l *requestLimiter) Allow(key string, limit int, window time.Duration, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.windows) > 10_000 {
		for key, bucket := range l.windows {
			if now.Sub(bucket.started) > window {
				delete(l.windows, key)
			}
		}
	}
	bucket, ok := l.windows[key]
	if !ok || now.Sub(bucket.started) >= window {
		l.windows[key] = rateWindow{started: now, count: 1}
		return true
	}
	if bucket.count >= limit {
		return false
	}
	bucket.count++
	l.windows[key] = bucket
	return true
}

type principal struct {
	UserID   string
	TenantID domain.TenantID
	Provider bool
	Kind     string
}
type contextKey string

const principalKey contextKey = "principal"

func (s *Server) Router() *gin.Engine {
	if s.Limiter == nil {
		s.Limiter = newRequestLimiter()
	}
	r := gin.New()
	if err := r.SetTrustedProxies(s.TrustedProxies); err != nil {
		panic(err)
	}
	r.Use(gin.Recovery())
	v1 := r.Group("/v1")
	v1.POST("/signup", s.rateLimit("signup", 5, time.Hour), s.signup)
	v1.POST("/email-verifications/confirm", s.rateLimit("verify", 10, time.Hour), s.verify)
	v1.POST("/login", s.rateLimit("login", 10, time.Minute), s.login)
	v1.POST("/logout", s.logout)
	v1.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	portal := v1.Group("")
	portal.Use(s.authenticate(false))
	portal.GET("/me", s.me)
	portal.GET("/me/credits", s.credits)
	portal.GET("/me/usage", s.usage)
	portal.GET("/me/purchases", s.purchaseList)
	portal.POST("/api-keys", s.rateLimit("key-issue", 10, time.Hour), s.issueKey)
	portal.GET("/api-keys", s.listKeys)
	portal.POST("/api-keys/:id/rotate", s.rateLimit("key-issue", 10, time.Hour), s.rotateKey)
	portal.DELETE("/api-keys/:id", s.revokeKey)
	portal.POST("/credit-purchases", s.rateLimitBy("purchase", 10, time.Hour, tenantKey), s.createPurchase)
	provider := v1.Group("/provider", s.authenticate(false), s.providerOnly())
	provider.GET("/tenants", s.providerTenants)
	provider.PATCH("/tenants/:id/markup", s.setMarkup)
	provider.GET("/ledger", s.providerLedger)
	provider.GET("/price", s.getPrice)
	provider.PUT("/price", s.setPrice)
	v1.POST("/payments/midtrans/notification", s.notification)
	proxy := v1.Group("/dstack", s.authenticate(true), s.rateLimitBy("proxy", 300, time.Minute, tenantKey))
	proxy.Any("/*path", s.proxyDStack)
	proxyAPI := v1.Group("/api", s.authenticate(true), s.rateLimitBy("proxy", 300, time.Minute, tenantKey))
	proxyAPI.Any("/*path", s.proxyDStack)
	return r
}

func (s *Server) rateLimit(action string, limit int, window time.Duration) gin.HandlerFunc {
	return s.rateLimitBy(action, limit, window, func(c *gin.Context) string { return c.ClientIP() })
}

func tenantKey(c *gin.Context) string {
	return string(mustPrincipal(c).TenantID)
}

func (s *Server) rateLimitBy(action string, limit int, window time.Duration, subject func(*gin.Context) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := action + ":" + subject(c)
		if !s.Limiter.Allow(key, limit, window, time.Now()) {
			c.Header("Retry-After", strconv.Itoa(int(window.Seconds())))
			respondError(c, http.StatusTooManyRequests, errors.New("too many requests"))
			c.Abort()
			return
		}
		// mutator-disable-next-line statement/remove
		c.Next()
	}
}

func respondError(c *gin.Context, status int, err error) { c.JSON(status, gin.H{"error": err.Error()}) }
func decode(c *gin.Context, out any) error {
	dec := json.NewDecoder(io.LimitReader(c.Request.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}

func (s *Server) signup(c *gin.Context) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(c, &req); err != nil {
		respondError(c, 400, errors.New("invalid request body"))
		return
	}
	if err := s.Identity.SignUp(c, req.Email, req.Password); err != nil {
		respondError(c, 400, err)
		return
	}
	c.Status(http.StatusAccepted)
}
func (s *Server) verify(c *gin.Context) {
	var req struct {
		Token string `json:"token"`
	}
	if err := decode(c, &req); err != nil {
		respondError(c, 400, errors.New("invalid request body"))
		return
	}
	id, err := s.Identity.VerifyEmail(c, req.Token)
	if err != nil {
		respondError(c, 400, err)
		return
	}
	c.JSON(200, gin.H{"user_id": id, "tenant_state": "provisioning"})
}
func (s *Server) login(c *gin.Context) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(c, &req); err != nil {
		respondError(c, 400, errors.New("invalid request body"))
		return
	}
	account := "login-account:" + strings.ToLower(strings.TrimSpace(req.Email))
	if !s.Limiter.Allow(account, 10, 15*time.Minute, time.Now()) {
		c.Header("Retry-After", strconv.Itoa(int((15 * time.Minute).Seconds())))
		respondError(c, http.StatusTooManyRequests, errors.New("too many requests"))
		return
	}
	session, err := s.Identity.Login(c, req.Email, req.Password)
	if err != nil {
		respondError(c, 401, err)
		return
	}
	c.JSON(200, gin.H{"token": session.Token, "expires_at": session.ExpiresAt})
}

func (s *Server) logout(c *gin.Context) {
	auth := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	if auth == "" {
		c.Status(http.StatusNoContent)
		return
	}
	if err := s.Identity.RevokeSession(c, auth); err != nil {
		respondError(c, 500, errors.New("could not revoke session"))
		return
	}
	c.Status(http.StatusNoContent)
}
func (s *Server) authenticate(apiOnly bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			respondError(c, http.StatusUnauthorized, errors.New("authentication required"))
			c.Abort()
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		var p principal
		if apiOnly {
			tenantID, err := s.Keys.Authenticate(c, token)
			if err != nil {
				respondError(c, http.StatusUnauthorized, errors.New("invalid API key"))
				c.Abort()
				return
			}
			p = principal{TenantID: domain.TenantID(tenantID), Kind: "api"}
		} else {
			userID, err := s.Identity.Authenticate(c, token)
			if err != nil {
				respondError(c, http.StatusUnauthorized, errors.New("invalid portal session"))
				c.Abort()
				return
			}
			provider, _ := s.Identity.IsProvider(c, userID)
			tenantID, err := s.Tenants.FindByOwner(c, domain.UserID(userID))
			if err != nil {
				if provider {
					p = principal{UserID: userID, Provider: true, Kind: "portal"}
					c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), principalKey, p))
					c.Next()
					return
				}
				respondError(c, http.StatusForbidden, errors.New("verified tenant required"))
				c.Abort()
				return
			}
			p = principal{UserID: userID, TenantID: tenantID, Provider: provider, Kind: "portal"}
		}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), principalKey, p))
		c.Next()
	}
}

func (s *Server) providerOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		p := mustPrincipal(c)
		if !p.Provider {
			respondError(c, 403, errors.New("provider access required"))
			c.Abort()
			return
		}
		c.Next()
	}
}
func mustPrincipal(c *gin.Context) principal {
	return c.Request.Context().Value(principalKey).(principal)
}
func (s *Server) me(c *gin.Context) {
	p := mustPrincipal(c)
	if p.TenantID == "" {
		respondError(c, http.StatusForbidden, errors.New("provider account has no customer tenant"))
		return
	}
	email, _ := s.Identity.Email(c, p.UserID)
	t, err := s.Tenants.Get(c, p.TenantID)
	if err != nil {
		respondError(c, 500, err)
		return
	}
	c.JSON(200, gin.H{"user_id": p.UserID, "email": email, "tenant_id": p.TenantID, "tenant_state": t.State, "dstack_project": t.DStack.Project})
}
func (s *Server) credits(c *gin.Context) {
	p := mustPrincipal(c)
	if p.TenantID == "" {
		respondError(c, http.StatusForbidden, errors.New("provider account has no customer tenant"))
		return
	}
	balance, err := s.Ledger.Balance(c, p.TenantID)
	if err != nil {
		respondError(c, 500, err)
		return
	}
	c.JSON(200, gin.H{"balance_micros": balance, "credits": float64(balance) / 1e6})
}
func (s *Server) usage(c *gin.Context) {
	p := mustPrincipal(c)
	if p.TenantID == "" {
		respondError(c, http.StatusForbidden, errors.New("provider account has no customer tenant"))
		return
	}
	records, err := s.Meter.ListUsage(c, p.TenantID, 500)
	if err != nil {
		respondError(c, 500, err)
		return
	}
	c.JSON(200, records)
}

func (s *Server) issueKey(c *gin.Context) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(c, &req); err != nil {
		respondError(c, 400, errors.New("invalid request body"))
		return
	}
	p := mustPrincipal(c)
	if p.TenantID == "" {
		respondError(c, http.StatusForbidden, errors.New("provider account has no customer tenant"))
		return
	}
	id, plain, err := s.Keys.Issue(c, apikey.TenantID(p.TenantID), req.Name)
	if err != nil {
		respondError(c, 400, err)
		return
	}
	c.JSON(201, gin.H{"id": id, "name": req.Name, "key": plain})
}
func (s *Server) rotateKey(c *gin.Context) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(c, &req); err != nil {
		respondError(c, 400, errors.New("invalid request body"))
		return
	}
	p := mustPrincipal(c)
	if p.TenantID == "" {
		respondError(c, http.StatusForbidden, errors.New("provider account has no customer tenant"))
		return
	}
	id, plain, err := s.Keys.Rotate(c, apikey.TenantID(p.TenantID), apikey.KeyID(c.Param("id")), req.Name)
	if err != nil {
		respondError(c, 400, err)
		return
	}
	c.JSON(201, gin.H{"id": id, "name": req.Name, "key": plain})
}
func (s *Server) revokeKey(c *gin.Context) {
	p := mustPrincipal(c)
	if p.TenantID == "" {
		respondError(c, http.StatusForbidden, errors.New("provider account has no customer tenant"))
		return
	}
	if err := s.Keys.Revoke(c, apikey.TenantID(p.TenantID), apikey.KeyID(c.Param("id"))); err != nil {
		respondError(c, 404, err)
		return
	}
	c.Status(204)
}
func (s *Server) listKeys(c *gin.Context) {
	p := mustPrincipal(c)
	if p.TenantID == "" {
		respondError(c, http.StatusForbidden, errors.New("provider account has no customer tenant"))
		return
	}
	keys, err := s.Keys.List(c, apikey.TenantID(p.TenantID))
	if err != nil {
		respondError(c, 500, err)
		return
	}
	c.JSON(200, keys)
}

func (s *Server) createPurchase(c *gin.Context) {
	var req struct {
		Package domain.PackageSize `json:"package_credits"`
	}
	if err := decode(c, &req); err != nil {
		respondError(c, 400, errors.New("invalid request body"))
		return
	}
	p := mustPrincipal(c)
	if p.TenantID == "" {
		respondError(c, http.StatusForbidden, errors.New("provider account has no customer tenant"))
		return
	}
	purchase, err := s.Payments.Create(c, p.TenantID, req.Package)
	if err != nil {
		respondError(c, 502, err)
		return
	}
	c.JSON(201, purchase)
}
func (s *Server) purchaseList(c *gin.Context) {
	p := mustPrincipal(c)
	if p.TenantID == "" {
		respondError(c, http.StatusForbidden, errors.New("provider account has no customer tenant"))
		return
	}
	list, err := s.Payments.ListPurchases(c, p.TenantID, 200)
	if err != nil {
		respondError(c, 500, err)
		return
	}
	c.JSON(200, list)
}

func (s *Server) notification(c *gin.Context) {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		respondError(c, 400, errors.New("invalid notification"))
		return
	}
	if err := s.Payments.ObserveNotification(c, raw, s.PaymentServerKey); err != nil {
		respondError(c, 400, err)
		return
	}
	c.JSON(200, gin.H{"status": "ok"})
}

func (s *Server) proxyDStack(c *gin.Context) {
	p := mustPrincipal(c)
	binding, err := s.Tenants.Get(c, p.TenantID)
	if err != nil {
		respondError(c, 403, domain.ErrTenantUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 4<<20))
	if err != nil {
		respondError(c, 400, errors.New("invalid request body"))
		return
	}
	path := c.Param("path")
	if strings.HasPrefix(c.Request.URL.Path, "/v1/dstack/") {
		path = "/api" + path
	}
	if strings.HasPrefix(c.Request.URL.Path, "/v1/api/") {
		path = "/api" + path
	}
	headers := make(http.Header)
	headers.Set("Content-Type", c.GetHeader("Content-Type"))
	op := dstack.Operation{Method: c.Request.Method, Path: path, Query: c.Request.URL.Query(), Body: body, Headers: headers, IdempotencyKey: c.GetHeader("Idempotency-Key")}
	response, err := s.DStack.Execute(c, p.TenantID, binding.DStack, op)
	if err != nil {
		status := 403
		if errors.Is(err, domain.ErrInsufficientCredits) || errors.Is(err, domain.ErrPriceUnverified) {
			status = 402
		}
		if errors.Is(err, domain.ErrTenantUnavailable) {
			status = 503
		}
		respondError(c, status, err)
		return
	}
	for key, values := range response.Headers {
		for _, value := range values {
			c.Header(key, value)
		}
	}
	c.Status(response.Status)
	_, _ = c.Writer.Write(response.Body)
}

func (s *Server) providerTenants(c *gin.Context) {
	tenants, err := s.Tenants.List(c)
	if err != nil {
		respondError(c, 500, err)
		return
	}
	c.JSON(200, tenants)
}
func (s *Server) setMarkup(c *gin.Context) {
	var req struct {
		MarkupBPS domain.MarkupBPS `json:"markup_bps"`
	}
	if err := decode(c, &req); err != nil {
		respondError(c, 400, errors.New("invalid request body"))
		return
	}
	exists, err := s.Tenants.Exists(c, domain.TenantID(c.Param("id")))
	if err != nil || !exists {
		respondError(c, 404, errors.New("tenant not found"))
		return
	}
	if err := s.Tenants.SetMarkup(c, domain.TenantID(c.Param("id")), req.MarkupBPS); err != nil {
		respondError(c, 400, err)
		return
	}
	c.Status(204)
}
func (s *Server) providerLedger(c *gin.Context) {
	tenantID := c.Query("tenant_id")
	if tenantID == "" {
		respondError(c, 400, errors.New("tenant_id is required"))
		return
	}
	entries, err := s.Ledger.ListEntries(c, tenantID, 500)
	if err != nil {
		respondError(c, 500, err)
		return
	}
	c.JSON(200, entries)
}
func (s *Server) getPrice(c *gin.Context) {
	price, err := s.Pricing.Current(c, "default")
	if err != nil {
		respondError(c, 404, err)
		return
	}
	c.JSON(200, gin.H{"hourly_usd_nano": price.HourlyUSD, "verified": price.Verified, "version": price.Version})
}
func (s *Server) setPrice(c *gin.Context) {
	var req struct {
		HourlyUSDNano int64  `json:"hourly_usd_nano"`
		Version       string `json:"version"`
		Verified      bool   `json:"verified"`
	}
	if err := decode(c, &req); err != nil || req.HourlyUSDNano <= 0 || req.Version == "" || req.HourlyUSDNano > math.MaxInt64/2 {
		respondError(c, 400, errors.New("price, version, and verification state are required"))
		return
	}
	if len(req.Version) > 200 {
		respondError(c, 400, errors.New("price version is too long"))
		return
	}
	p := mustPrincipal(c)
	err := s.Pricing.Set(c, "default", domain.USDNano(req.HourlyUSDNano), req.Verified, req.Version, p.UserID)
	if err != nil {
		respondError(c, 500, err)
		return
	}
	c.Status(204)
}
