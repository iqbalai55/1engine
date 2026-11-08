package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"billing-backend/internal/apikey"
	"billing-backend/internal/domain"
	"billing-backend/internal/dstack"
	"billing-backend/internal/identity"
	"billing-backend/internal/ledger"
	"billing-backend/internal/payments"
	"billing-backend/internal/tenant"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Server struct {
	DB               *pgxpool.Pool
	Identity         *identity.Service
	Keys             *apikey.Service
	Ledger           *ledger.Service
	Tenants          *tenant.Service
	Payments         *payments.Service
	DStack           *dstack.Proxy
	PaymentServerKey string
	Limiter          *requestLimiter
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
	portal.POST("/credit-purchases", s.createPurchase)
	provider := v1.Group("/provider", s.authenticate(false), s.providerOnly())
	provider.GET("/tenants", s.providerTenants)
	provider.PATCH("/tenants/:id/markup", s.setMarkup)
	provider.GET("/ledger", s.providerLedger)
	provider.GET("/price", s.getPrice)
	provider.PUT("/price", s.setPrice)
	v1.POST("/payments/midtrans/notification", s.notification)
	proxy := v1.Group("/dstack", s.authenticate(true))
	proxy.Any("/*path", s.proxyDStack)
	proxyAPI := v1.Group("/api", s.authenticate(true))
	proxyAPI.Any("/*path", s.proxyDStack)
	return r
}

func (s *Server) rateLimit(action string, limit int, window time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := action + ":" + c.ClientIP()
		if !s.Limiter.Allow(key, limit, window, time.Now()) {
			c.Header("Retry-After", strconv.Itoa(int(window.Seconds())))
			respondError(c, http.StatusTooManyRequests, errors.New("too many requests"))
			c.Abort()
			return
		}
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
	session, err := s.Identity.Login(c, req.Email, req.Password)
	if err != nil {
		respondError(c, 401, err)
		return
	}
	c.JSON(200, gin.H{"token": session.Token, "expires_at": session.ExpiresAt})
}

func (s *Server) logout(c *gin.Context) {
	auth := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	h := sha256.Sum256([]byte(auth))
	mac := hmac.New(sha256.New, s.Identity.SessionSecret)
	_, _ = mac.Write(h[:])
	if auth == "" {
		c.Status(http.StatusNoContent)
		return
	}
	if _, err := s.DB.Exec(c, `UPDATE portal_sessions SET revoked_at=now() WHERE token_hash=$1 AND revoked_at IS NULL`, mac.Sum(nil)); err != nil {
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
			var tenantID domain.TenantID
			provider, _ := s.Identity.IsProvider(c, userID)
			err = s.DB.QueryRow(c, `SELECT id FROM tenants WHERE owner_user_id=$1`, userID).Scan(&tenantID)
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
	var email string
	_ = s.DB.QueryRow(c, `SELECT email FROM users WHERE id=$1`, p.UserID).Scan(&email)
	t, err := s.Tenants.Get(c, p.TenantID)
	if err != nil {
		respondError(c, 500, err)
		return
	}
	c.JSON(200, gin.H{"user_id": p.UserID, "email": email, "tenant_id": p.TenantID, "tenant_state": t.State})
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
	rows, err := s.DB.Query(c, `SELECT allocation_id,submission_id,interval_start,interval_end,gpu_count,rate_usd_nano,rate_unit,usd_nano,markup_bps,credit_delta_micros,source_version FROM usage_records WHERE tenant_id=$1 ORDER BY interval_start DESC LIMIT 500`, string(p.TenantID))
	if err != nil {
		respondError(c, 500, err)
		return
	}
	defer rows.Close()
	type item struct {
		Allocation string    `json:"allocation_id"`
		Submission string    `json:"submission_id"`
		Start      time.Time `json:"start"`
		End        time.Time `json:"end"`
		GPUs       int       `json:"gpu_count"`
		Rate       *int64    `json:"rate_usd_nano"`
		RateUnit   string    `json:"rate_unit"`
		USD        *int64    `json:"usd_nano"`
		Markup     int       `json:"markup_bps"`
		Credits    int64     `json:"credit_delta_micros"`
		Version    string    `json:"source_version"`
	}
	var records []item
	for rows.Next() {
		var v item
		if err := rows.Scan(&v.Allocation, &v.Submission, &v.Start, &v.End, &v.GPUs, &v.Rate, &v.RateUnit, &v.USD, &v.Markup, &v.Credits, &v.Version); err != nil {
			respondError(c, 500, fmt.Errorf("read tenant usage: %w", err))
			return
		}
		records = append(records, v)
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
	rows, err := s.DB.Query(c, `SELECT id,order_id,package_size,credit_micros,amount_idr,state,snap_redirect_url,created_at FROM purchases WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT 200`, string(p.TenantID))
	if err != nil {
		respondError(c, 500, err)
		return
	}
	defer rows.Close()
	type purchase struct {
		ID       string    `json:"id"`
		Order    string    `json:"order_id"`
		Package  int       `json:"package_credits"`
		Credits  int64     `json:"credit_micros"`
		Amount   int64     `json:"amount_idr"`
		State    string    `json:"state"`
		Redirect string    `json:"redirect_url"`
		Created  time.Time `json:"created_at"`
	}
	var list []purchase
	for rows.Next() {
		var v purchase
		if err := rows.Scan(&v.ID, &v.Order, &v.Package, &v.Credits, &v.Amount, &v.State, &v.Redirect, &v.Created); err != nil {
			respondError(c, 500, err)
			return
		}
		list = append(list, v)
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
	var exists bool
	if err := s.DB.QueryRow(c, `SELECT EXISTS(SELECT 1 FROM tenants WHERE id=$1)`, c.Param("id")).Scan(&exists); err != nil || !exists {
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
	rows, err := s.DB.Query(c, `SELECT id,delta_micros,kind,idempotency_key,source,snapshot,created_at FROM credit_entries WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT 500`, tenantID)
	if err != nil {
		respondError(c, 500, err)
		return
	}
	defer rows.Close()
	type entry struct {
		ID          string          `json:"id"`
		Delta       int64           `json:"delta_micros"`
		Kind        string          `json:"kind"`
		Idempotency string          `json:"idempotency_key"`
		Source      string          `json:"source"`
		Snapshot    json.RawMessage `json:"snapshot"`
		Created     time.Time       `json:"created_at"`
	}
	var entries []entry
	for rows.Next() {
		var v entry
		if err := rows.Scan(&v.ID, &v.Delta, &v.Kind, &v.Idempotency, &v.Source, &v.Snapshot, &v.Created); err != nil {
			respondError(c, 500, err)
			return
		}
		entries = append(entries, v)
	}
	c.JSON(200, entries)
}
func (s *Server) getPrice(c *gin.Context) {
	var price int64
	var verified bool
	var version string
	err := s.DB.QueryRow(c, `SELECT hourly_usd_nano,verified,version FROM dstack_instance_prices WHERE price_key='default'`).Scan(&price, &verified, &version)
	if err != nil {
		respondError(c, 404, err)
		return
	}
	c.JSON(200, gin.H{"hourly_usd_nano": price, "verified": verified, "version": version})
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
	verifiedBy := any(nil)
	if req.Verified {
		verifiedBy = p.UserID
	}
	_, err := s.DB.Exec(c, `INSERT INTO dstack_instance_prices(price_key,hourly_usd_nano,rate_unit,verified,version,verified_by) VALUES('default',$1,'instance_hour',$2,$3,$4) ON CONFLICT(price_key) DO UPDATE SET hourly_usd_nano=EXCLUDED.hourly_usd_nano,verified=EXCLUDED.verified,version=EXCLUDED.version,verified_by=EXCLUDED.verified_by,observed_at=now()`, req.HourlyUSDNano, req.Verified, req.Version, verifiedBy)
	if err != nil {
		respondError(c, 500, err)
		return
	}
	c.Status(204)
}
