package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"billing-backend/internal/apikey"
	"billing-backend/internal/compute"
	"billing-backend/internal/config"
	"billing-backend/internal/domain"
	"billing-backend/internal/dstack"
	"billing-backend/internal/httpapi"
	"billing-backend/internal/identity"
	"billing-backend/internal/ledger"
	"billing-backend/internal/payments"
	"billing-backend/internal/store"
	"billing-backend/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		log.Fatal(err)
	}
	mailer := identity.SMTPMailer{Host: cfg.SMTPHost, Port: os.Getenv("SMTP_PORT"), Username: cfg.SMTPUser, Password: cfg.SMTPPassword, From: cfg.SMTPFrom, PublicBaseURL: cfg.PublicBaseURL}
	identityService := &identity.Service{DB: db.Pool, Mailer: mailer, SessionTTL: 24 * time.Hour, SessionSecret: cfg.SessionSecret}
	keyService := &apikey.Service{DB: db.Pool}
	ledgerService := &ledger.Service{DB: db.Pool}
	tenantService := &tenant.Service{DB: db.Pool}
	snap := payments.SnapClient{Config: payments.SnapConfig{ServerKey: cfg.MidtransServerKey, ClientKey: cfg.MidtransClientKey, BaseURL: cfg.MidtransBaseURL, StatusURL: cfg.MidtransStatusURL, Environment: cfg.MidtransEnvironment, PublicBaseURL: cfg.PublicBaseURL}}
	paymentService := &payments.Service{DB: db.Pool, Ledger: ledgerService, Snap: snap}
	proxy := dstack.NewProxy(cfg.DStackURL, tenantService, cfg.EncryptionKey, ledgerService)
	provisioner := &dstack.Provisioner{DB: db.Pool, Admin: dstack.AdminClient{BaseURL: cfg.DStackURL, Token: cfg.DStackAdminToken}, Tenant: tenantService, EncryptionKey: cfg.EncryptionKey, KeyVersion: "v1"}
	reconciler := &compute.Reconciler{DB: db.Pool, Source: &dstack.ObservationClient{BaseURL: cfg.DStackURL, Credentials: tenantService, Tenants: tenantService, EncryptionKey: cfg.EncryptionKey}}
	go runProvisioner(ctx, db.Pool, provisioner)
	go runUsageReconciler(ctx, db.Pool, reconciler)
	go runPaymentReconciler(ctx, paymentService)
	api := &httpapi.Server{DB: db.Pool, Identity: identityService, Keys: keyService, Ledger: ledgerService, Tenants: tenantService, Payments: paymentService, DStack: proxy, PaymentServerKey: cfg.MidtransServerKey}
	server := &http.Server{Addr: cfg.ListenAddr, Handler: api.Router(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 60 * time.Second}
	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stopped
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("billing backend listening on %s", cfg.ListenAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func runUsageReconciler(ctx context.Context, db *pgxpool.Pool, reconciler *compute.Reconciler) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rows, err := db.Query(ctx, `SELECT t.id FROM tenants t WHERE t.state='ready' AND t.dstack_provision_state='ready' ORDER BY COALESCE((SELECT min(c.updated_at) FROM dstack_poll_cursors c WHERE c.tenant_id=t.id),to_timestamp(0)),t.created_at LIMIT 100`)
			if err != nil {
				continue
			}
			var ids []domain.TenantID
			for rows.Next() {
				var id domain.TenantID
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
			for _, id := range ids {
				_ = reconciler.PollTenant(ctx, id)
			}
		}
	}
}

func runPaymentReconciler(ctx context.Context, service *payments.Service) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = service.ReconcileOpen(ctx, 100)
		}
	}
}

func runProvisioner(ctx context.Context, db *pgxpool.Pool, provisioner *dstack.Provisioner) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rows, err := db.Query(ctx, `SELECT id FROM tenants WHERE dstack_provision_state IN ('pending','failed') ORDER BY created_at LIMIT 25`)
			if err != nil {
				continue
			}
			var ids []domain.TenantID
			for rows.Next() {
				var id domain.TenantID
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
			for _, id := range ids {
				_ = provisioner.Provision(ctx, id)
			}
		}
	}
}
