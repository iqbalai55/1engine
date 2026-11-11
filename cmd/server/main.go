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
	"billing-backend/internal/dstack"
	"billing-backend/internal/httpapi"
	"billing-backend/internal/identity"
	"billing-backend/internal/ledger"
	"billing-backend/internal/payments"
	"billing-backend/internal/pricing"
	"billing-backend/internal/store"
	"billing-backend/internal/tenant"
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
	providerBackends, err := dstack.LoadProviderBackends(cfg.ProviderBackendsPath)
	if err != nil {
		log.Fatal(err)
	}
	var mailer identity.Mailer = identity.SMTPMailer{Host: cfg.SMTPHost, Port: os.Getenv("SMTP_PORT"), Username: cfg.SMTPUser, Password: cfg.SMTPPassword, From: cfg.SMTPFrom, PublicBaseURL: cfg.PublicBaseURL}
	if cfg.EmailDelivery == "log" {
		mailer = identity.LogMailer{PublicBaseURL: cfg.PublicBaseURL}
	}
	keyService := &apikey.Service{DB: db.Pool}
	tenantService := &tenant.Service{DB: db.Pool}
	priceService := &pricing.Service{DB: db.Pool}
	ledgerService := &ledger.Service{DB: db.Pool, Prices: priceService, Admission: tenantService}
	identityService := &identity.Service{DB: db.Pool, Mailer: mailer, Tenants: tenantService, Accounts: ledgerService, SessionTTL: 24 * time.Hour, SessionSecret: cfg.SessionSecret}
	snap := payments.SnapClient{Config: payments.SnapConfig{ServerKey: cfg.MidtransServerKey, ClientKey: cfg.MidtransClientKey, BaseURL: cfg.MidtransBaseURL, StatusURL: cfg.MidtransStatusURL, Environment: cfg.MidtransEnvironment, PublicBaseURL: cfg.PublicBaseURL}}
	paymentService := &payments.Service{DB: db.Pool, Ledger: ledgerService, Snap: snap}
	proxy := dstack.NewProxy(cfg.DStackURL, tenantService, cfg.EncryptionKey, ledgerService)
	proxy.Backends = providerBackends
	provisioner := &dstack.Provisioner{Identity: identityService, Admin: dstack.AdminClient{BaseURL: cfg.DStackURL, Token: cfg.DStackAdminToken}, Tenant: tenantService, Backends: providerBackends, EncryptionKey: cfg.EncryptionKey, KeyVersion: "v1"}
	reconciler := &compute.Reconciler{DB: db.Pool, Meter: &compute.Meter{DB: db.Pool, Ledger: ledgerService}, Prices: priceService, Source: &dstack.ObservationClient{BaseURL: cfg.DStackURL, Credentials: tenantService, Tenants: tenantService, EncryptionKey: cfg.EncryptionKey}}
	go runProvisioner(ctx, provisioner)
	go runProviderBackendSync(ctx, provisioner)
	go runUsageReconciler(ctx, tenantService, reconciler)
	go runPaymentReconciler(ctx, paymentService)
	api := &httpapi.Server{Identity: identityService, Keys: keyService, Ledger: ledgerService, Meter: reconciler.Meter, Tenants: tenantService, Payments: paymentService, Pricing: priceService, DStack: proxy, PaymentServerKey: cfg.MidtransServerKey, TrustedProxies: cfg.TrustedProxyCIDRs}
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

func runUsageReconciler(ctx context.Context, tenants *tenant.Service, reconciler *compute.Reconciler) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ids, err := tenants.ListReadyForUsage(ctx, 100)
			if err != nil {
				continue
			}
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

func runProvisioner(ctx context.Context, provisioner *dstack.Provisioner) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = provisioner.ProvisionPending(ctx, 25)
		}
	}
}

func runProviderBackendSync(ctx context.Context, provisioner *dstack.Provisioner) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	if err := provisioner.SyncConfiguredBackends(ctx); err != nil {
		log.Printf("provider backend sync failed: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := provisioner.SyncConfiguredBackends(ctx); err != nil {
				log.Printf("provider backend sync failed: %v", err)
			}
		}
	}
}
