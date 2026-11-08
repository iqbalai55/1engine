package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"billing-backend/internal/domain"
	"billing-backend/internal/identity"
	"billing-backend/internal/ledger"
)

func TestConcurrentIdempotentLedgerPosting(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is unset")
	}
	ctx := context.Background()
	db, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var user, tenant string
	nonce, _, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `INSERT INTO users(email,password_hash,email_verified_at) VALUES($1,'test',now()) RETURNING id`, fmt.Sprintf("integration-%d-%s@example.test", os.Getpid(), nonce)).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `INSERT INTO tenants(owner_user_id,dstack_username,dstack_project,state) VALUES($1,$2,$3,'ready') RETURNING id`, user, "integration-user-"+user, "integration-project-"+user).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO credit_accounts(tenant_id) VALUES($1)`, tenant); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.Pool.Exec(ctx, `DELETE FROM payment_observations WHERE purchase_id IN (SELECT id FROM purchases WHERE tenant_id=$1)`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM purchases WHERE tenant_id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM compute_reservations WHERE tenant_id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM api_keys WHERE tenant_id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM usage_records WHERE tenant_id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM dstack_events WHERE tenant_id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM usage_cursors WHERE tenant_id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM dstack_runs WHERE tenant_id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM dstack_observations WHERE tenant_id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM dstack_poll_cursors WHERE tenant_id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM tenant_dstack_credentials WHERE tenant_id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM credit_entries WHERE tenant_id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM credit_accounts WHERE tenant_id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM tenants WHERE id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, user)
	}()
	service := ledger.Service{DB: db.Pool}
	const workers = 20
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := service.Post(ctx, ledger.Posting{Tenant: domain.TenantID(tenant), Delta: 1_000_000, Kind: domain.EntryPurchase, IdempotencyKey: "test-purchase", Source: "test-order"})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	balance, err := service.Balance(ctx, domain.TenantID(tenant))
	if err != nil {
		t.Fatal(err)
	}
	if balance != 1_000_000 {
		t.Fatalf("balance after concurrent duplicate grants = %d, want 1000000", balance)
	}
	var entries int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM credit_entries WHERE tenant_id=$1 AND idempotency_key='test-purchase'`, tenant).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if entries != 1 {
		t.Fatalf("ledger entries = %d, want 1", entries)
	}
	if err := service.ReconcileBalance(ctx, domain.TenantID(tenant)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO dstack_instance_prices(price_key,hourly_usd_nano,rate_unit,verified,version) VALUES('integration-reserve',500000000,'instance_hour',true,'test-v1')`); err != nil {
		t.Fatal(err)
	}
	defer db.Pool.Exec(ctx, `DELETE FROM dstack_instance_prices WHERE price_key='integration-reserve'`)
	hash := sha256.Sum256([]byte("start-one"))
	reservation, err := service.Reserve(ctx, domain.TenantID(tenant), "integration-reserve", "start-one", hash[:], time.Hour)
	if err != nil || reservation.Admission.Reserved != 550_000 {
		t.Fatalf("first reservation = %+v, %v", reservation, err)
	}
	if _, err := service.Reserve(ctx, domain.TenantID(tenant), "integration-reserve", "start-one", hash[:], time.Hour); err == nil {
		t.Fatal("identical in-flight start was admitted twice")
	}
	secondHash := sha256.Sum256([]byte("start-two"))
	if _, err := service.Reserve(ctx, domain.TenantID(tenant), "integration-reserve", "start-two", secondHash[:], time.Hour); err == nil {
		t.Fatal("second start ignored the outstanding reservation")
	}
	if err := service.CompleteReservation(ctx, domain.TenantID(tenant), "start-one", 201, []byte(`{"run":"run-1"}`)); err != nil {
		t.Fatal(err)
	}
	replay, err := service.Reserve(ctx, domain.TenantID(tenant), "integration-reserve", "start-one", hash[:], time.Hour)
	if err != nil || !replay.Replayed || replay.ResponseStatus != 201 || string(replay.ResponseBody) != `{"run":"run-1"}` {
		t.Fatalf("completed start replay = %+v, %v", replay, err)
	}
	if err := service.ReleaseReservation(ctx, domain.TenantID(tenant), "start-one"); err != nil {
		t.Fatal(err)
	}
}
