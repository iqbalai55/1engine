package providerdashboard

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"strings"
	"testing"

	"billing-backend/internal/identity"
	"billing-backend/internal/store"
)

func TestSnapshotCustomerAndBillingPages(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is unset")
	}
	ctx := context.Background()
	adminDB, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer adminDB.Close()
	schema := fmt.Sprintf("provider_dashboard_%d", os.Getpid())
	if _, err := adminDB.Pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer adminDB.Pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	db := &store.Postgres{Pool: pool}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	nonce, _, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	var user, tenant string
	var account string
	email := fmt.Sprintf("monitor-%s@example.test", nonce)
	if err := db.Pool.QueryRow(ctx, `INSERT INTO users(email,password_hash,email_verified_at) VALUES($1,'test',now()) RETURNING id`, email).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `INSERT INTO tenants(owner_user_id,dstack_username,dstack_project,state,dstack_provision_state) VALUES($1,$2,$3,'ready','ready') RETURNING id`, user, "monitor-user-"+nonce, "monitor-project-"+nonce).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `INSERT INTO credit_accounts(tenant_id,balance_micros) VALUES($1,9000000) RETURNING tenant_id::text`, tenant).Scan(&account); err != nil {
		t.Fatal(err)
	}
	var purchase string
	order := "monitor-" + nonce
	if err := db.Pool.QueryRow(ctx, `INSERT INTO purchases(tenant_id,package_size,credit_micros,amount_idr,order_id,state,refunded_idr,snap_redirect_url) VALUES($1,10,10000000,180000,$2,'settlement',18000,'https://example.test') RETURNING id`, tenant, order).Scan(&purchase); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO credit_entries(tenant_id,delta_micros,kind,idempotency_key,source) VALUES($1,10000000,'purchase',$2,$3)`, tenant, "purchase:"+purchase+":settlement", purchase); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO credit_entries(tenant_id,delta_micros,kind,idempotency_key,source) VALUES($1,-1000000,$2,$3,$4)`, tenant, "usage", "monitor-usage-"+nonce, "monitor-usage"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.Pool.Exec(ctx, `DELETE FROM payment_observations WHERE purchase_id=$1`, purchase)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM purchases WHERE id=$1`, purchase)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM credit_entries WHERE tenant_id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM credit_accounts WHERE tenant_id=$1`, account)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM tenants WHERE id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, user)
	}()

	service := &Service{DB: db.Pool}
	snapshot, err := service.Snapshot(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Overview.TenantReady < 1 || snapshot.Overview.UsageDebitedMicros30d != "1000000" {
		t.Fatalf("overview = %#v", snapshot.Overview)
	}
	if snapshot.Overview.SettledNetIDR30d != "162000" || snapshot.Overview.SettledRefundsIDR30d != "18000" {
		t.Fatalf("payment totals = net %s, refunded %s", snapshot.Overview.SettledNetIDR30d, snapshot.Overview.SettledRefundsIDR30d)
	}
	if !snapshot.Overview.CreditBalanceComplete || snapshot.Overview.CreditBalanceMicros == nil || *snapshot.Overview.CreditBalanceMicros != "9000000" {
		t.Fatalf("credit balance = %#v, complete=%v", snapshot.Overview.CreditBalanceMicros, snapshot.Overview.CreditBalanceComplete)
	}
	if len(snapshot.Customers.Items) != 1 || snapshot.Customers.Items[0].Email != email {
		t.Fatalf("customer page = %#v", snapshot.Customers)
	}
	if len(snapshot.Billing.Items) != 1 || snapshot.Billing.Items[0].AmountIDR != "180000" {
		t.Fatalf("billing page = %#v", snapshot.Billing)
	}
	if _, err := service.Customers(ctx, 20, "", strings.Repeat("x", 201)); err == nil {
		t.Fatal("oversized customer search accepted")
	}
	if _, err := service.Customers(ctx, 20, "bad", ""); err == nil {
		t.Fatal("malformed customer cursor accepted")
	}
	if _, err := service.Billing(ctx, 20, "bad"); err == nil {
		t.Fatal("malformed billing cursor accepted")
	}
}
