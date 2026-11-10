package compute

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"billing-backend/internal/domain"
	"billing-backend/internal/identity"
	"billing-backend/internal/ledger"
	"billing-backend/internal/store"
)

func TestUsageChargeWholeInstanceAndGPUHourRates(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	whole, usd, err := domain.UsageCharge(start, end, 100_000_000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if whole != 110_000 || usd != 110_000_000 {
		t.Fatalf("whole instance charge = %d/%d", whole, usd)
	}
	perGPU, usd, err := domain.UsageCharge(start, end, 200_000_000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if perGPU != 220_000 || usd != 220_000_000 {
		t.Fatalf("per GPU charge for 2 GPUs = %d/%d", perGPU, usd)
	}
}

func TestActiveIntervalUsesOnlyObservedStartAndFinish(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start.Add(45 * time.Minute)
	interval, ok := ActiveInterval(start, nil, now)
	if !ok || !interval.Start.Equal(start) || !interval.End.Equal(now) {
		t.Fatalf("active interval = %+v, %v", interval, ok)
	}
	finished := start.Add(30 * time.Minute)
	interval, ok = ActiveInterval(start, &finished, now)
	if !ok || !interval.Start.Equal(start) || !interval.End.Equal(finished) {
		t.Fatalf("finished interval = %+v, %v", interval, ok)
	}
	if _, ok := ActiveInterval(start, &start, now); ok {
		t.Fatal("zero-duration interval was accepted")
	}
}

func TestReconcileUsageIntervalDoesNotDoubleCountOverlap(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	previous := start.Add(45 * time.Minute)
	now := start.Add(time.Hour)
	interval, ok := ReconcileUsageInterval(start, nil, now, &previous)
	if !ok || !interval.Start.Equal(previous) || !interval.End.Equal(now) {
		t.Fatalf("incremental interval = %+v, %v", interval, ok)
	}
	interval, ok = ReconcileUsageInterval(start, nil, previous, &previous)
	if ok {
		t.Fatalf("duplicate poll generated interval %+v", interval)
	}
}

func TestBuildInstanceUsageEvidenceUsesStableMinuteBoundaries(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 23, 0, time.UTC)
	now := time.Date(2026, 1, 1, 0, 2, 48, 0, time.UTC)
	rate := InstancePrice{Verified: true, Version: "verified-v1"}
	rateValue := domain.USDNano(100_000_000)
	snapshot := InstanceUsageSnapshot{ID: "instance-1", Created: start, GPUCount: 2, GPUCountKnown: true, PriceBasis: RateInstanceHour, PriceUSDNano: &rateValue, SourceVersion: "observation-v1"}

	evidence, ok := BuildInstanceUsageEvidence("tenant-1", snapshot, rate, now, nil)
	if !ok {
		t.Fatal("active instance did not produce a usage interval")
	}
	if evidence.IntervalStart != start || !evidence.IntervalEnd.Equal(time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)) {
		t.Fatalf("active interval = %v to %v", evidence.IntervalStart, evidence.IntervalEnd)
	}
	if !evidence.PriceVerified || evidence.Rate != 100_000_000 || evidence.GPUCount != 2 {
		t.Fatalf("active evidence = %+v", evidence)
	}

	previous := evidence.IntervalEnd
	evidence, ok = BuildInstanceUsageEvidence("tenant-1", snapshot, rate, now, &previous)
	if !ok || !evidence.IntervalStart.Equal(previous) || !evidence.IntervalEnd.Equal(time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC)) {
		t.Fatalf("next active interval = %+v, %v", evidence, ok)
	}
	previous = evidence.IntervalEnd
	if _, ok := BuildInstanceUsageEvidence("tenant-1", snapshot, rate, now, &previous); ok {
		t.Fatal("same minute poll produced another active interval")
	}

	finished := time.Date(2026, 1, 1, 0, 3, 31, 0, time.UTC)
	snapshot.FinishedAt = &finished
	previous = evidence.IntervalEnd
	evidence, ok = BuildInstanceUsageEvidence("tenant-1", snapshot, rate, now, &previous)
	if !ok || !evidence.IntervalStart.Equal(previous) || !evidence.IntervalEnd.Equal(time.Date(2026, 1, 1, 0, 3, 0, 0, time.UTC)) {
		t.Fatalf("terminal interval = %+v, %v", evidence, ok)
	}
	previous = evidence.IntervalEnd
	evidence, ok = BuildInstanceUsageEvidence("tenant-1", snapshot, rate, now, &previous)
	if !ok || !evidence.IntervalStart.Equal(previous) || !evidence.IntervalEnd.Equal(finished) {
		t.Fatalf("terminal remainder = %+v, %v", evidence, ok)
	}
	if _, ok := BuildInstanceUsageEvidence("tenant-1", snapshot, rate, now, &evidence.IntervalEnd); ok {
		t.Fatal("repeated terminal poll produced another interval")
	}
}

func TestBuildInstanceUsageEvidenceChargesReportedInstancePrice(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rateValue := domain.USDNano(100_000_000)
	snapshot := InstanceUsageSnapshot{ID: "instance-1", Created: start, GPUCount: 1, GPUCountKnown: true, PriceBasis: RateInstanceHour, PriceUSDNano: &rateValue, SourceVersion: "observation-v1"}
	rate := InstancePrice{Verified: false, Version: "unverified-v1"}
	now := start.Add(2 * time.Minute)

	evidence, ok := BuildInstanceUsageEvidence("tenant-1", snapshot, rate, now, nil)
	if !ok || evidence.PriceVerified || evidence.RateUnit != RateUnpriced || evidence.Rate != 0 {
		t.Fatalf("unverified price evidence = %+v, %v", evidence, ok)
	}

	rate.Verified = true
	otherRate := domain.USDNano(200_000_000)
	snapshot.PriceUSDNano = &otherRate
	evidence, ok = BuildInstanceUsageEvidence("tenant-1", snapshot, rate, now, nil)
	if !ok || !evidence.PriceVerified || evidence.RateUnit != RateInstanceHour || evidence.Rate != otherRate {
		t.Fatalf("reported instance price evidence = %+v, %v", evidence, ok)
	}

	snapshot.PriceUSDNano = nil
	evidence, ok = BuildInstanceUsageEvidence("tenant-1", snapshot, rate, now, nil)
	if !ok || evidence.PriceVerified || evidence.RateUnit != RateUnpriced || evidence.Rate != 0 {
		t.Fatalf("missing price evidence = %+v, %v", evidence, ok)
	}

	snapshot.PriceRaw = "0.1"
	snapshot.PriceUSDNano = &rateValue
	snapshot.PriceBasis = ""
	evidence, ok = BuildInstanceUsageEvidence("tenant-1", snapshot, rate, now, nil)
	if !ok || evidence.PriceVerified || evidence.RateUnit != RateUnpriced || evidence.Rate != 0 {
		t.Fatalf("unverified price basis evidence = %+v, %v", evidence, ok)
	}
}

func TestDecodeInstanceUsageSnapshotReadsPerInstancePrice(t *testing.T) {
	payload := []byte(`{"created":"2026-01-01T00:00:00Z","finished_at":null,"price":0.1,"instance_type":{"resources":{"gpus":[{},{}]}}}`)
	snapshot, err := decodeInstanceUsageSnapshot("instance-1", "observation-v1", payload)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ID != "instance-1" || snapshot.Created.IsZero() || !snapshot.GPUCountKnown || snapshot.GPUCount != 2 || snapshot.PriceRaw != "0.1" {
		t.Fatalf("decoded instance snapshot = %+v", snapshot)
	}
	if snapshot.PriceUSDNano == nil || *snapshot.PriceUSDNano != 100_000_000 || snapshot.PriceBasis != RateInstanceHour {
		t.Fatalf("decoded instance price = %v basis %q", snapshot.PriceUSDNano, snapshot.PriceBasis)
	}
	for _, raw := range []string{`null`, `0`, `"abc"`} {
		payload := []byte(`{"created":"2026-01-01T00:00:00Z","price":` + raw + `}`)
		snapshot, err := decodeInstanceUsageSnapshot("instance-1", "observation-v1", payload)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.PriceUSDNano != nil || snapshot.PriceBasis != "" {
			t.Fatalf("price %s was accepted as %v basis %q", raw, snapshot.PriceUSDNano, snapshot.PriceBasis)
		}
	}
}

func TestDecodeInstanceUsageSnapshotRequiresCreated(t *testing.T) {
	if _, err := decodeInstanceUsageSnapshot("instance-1", "observation-v1", []byte(`{"price":0.1}`)); err == nil {
		t.Fatal("snapshot without creation time was accepted")
	}
}

func TestUsageIntervalsAreDeduplicatedAndUnverifiedRatesDoNotCharge(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is unset")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var user, tenant string
	token, _, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Pool.QueryRow(ctx, `INSERT INTO users(email,password_hash,email_verified_at) VALUES($1,'test',now()) RETURNING id`, "meter-"+token+"@example.test").Scan(&user); err != nil {
		t.Fatal(err)
	}
	if err = db.Pool.QueryRow(ctx, `INSERT INTO tenants(owner_user_id,dstack_username,dstack_project,state,markup_bps) VALUES($1,$2,$3,'ready',1000) RETURNING id`, user, "meter-user-"+token, "meter-project-"+token).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Pool.Exec(ctx, `INSERT INTO credit_accounts(tenant_id) VALUES($1)`, tenant); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.Pool.Exec(ctx, `DELETE FROM usage_records WHERE tenant_id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM dstack_events WHERE tenant_id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM credit_entries WHERE tenant_id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM credit_accounts WHERE tenant_id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM tenants WHERE id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, user)
	}()
	meter := Meter{DB: db.Pool, Ledger: &ledger.Service{DB: db.Pool}}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	evidence := UsageEvidence{Tenant: domain.TenantID(tenant), AllocationID: "allocation-1", SubmissionID: "submission-1", IntervalStart: start, IntervalEnd: start.Add(time.Hour), GPUCount: 1, Rate: 100_000_000, RateUnit: RateInstanceHour, PriceVerified: false, SourceVersion: "dstack-test", Provenance: map[string]any{"source": "test"}}
	if err := meter.PostUsage(ctx, evidence); err != nil {
		t.Fatal(err)
	}
	if err := meter.PostUsage(ctx, evidence); err != nil {
		t.Fatal(err)
	}
	var charged, balance int64
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM credit_entries WHERE tenant_id=$1`, tenant).Scan(&charged); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT balance_micros FROM credit_accounts WHERE tenant_id=$1`, tenant).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if charged != 0 || balance != 0 {
		t.Fatalf("unverified duplicate usage charged %d entries and balance %d", charged, balance)
	}
	var recorded int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM usage_records WHERE tenant_id=$1 AND rate_unit='unpriced' AND credit_delta_micros=0`, tenant).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 1 {
		t.Fatalf("unpriced interval records=%d, want 1", recorded)
	}
	mismatched := evidence
	mismatched.AllocationID = "allocation-2"
	mismatched.IntervalStart = start.Add(time.Hour)
	mismatched.IntervalEnd = start.Add(2 * time.Hour)
	mismatched.PriceVerified = false
	mismatched.RateUnit = RateUnpriced
	mismatched.Rate = 0
	mismatched.SourceVersion = "dstack-test-mismatch"
	if err := meter.PostUsage(ctx, mismatched); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM credit_entries WHERE tenant_id=$1`, tenant).Scan(&charged); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT balance_micros FROM credit_accounts WHERE tenant_id=$1`, tenant).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if charged != 0 || balance != 0 {
		t.Fatalf("mismatched price charged %d entries and balance %d", charged, balance)
	}
	payload, _ := json.Marshal(map[string]string{"type": "run.updated"})
	first, err := meter.RecordEvent(ctx, domain.TenantID(tenant), "event-1", start, payload)
	if err != nil {
		t.Fatal(err)
	}
	again, err := meter.RecordEvent(ctx, domain.TenantID(tenant), "event-1", start, payload)
	if err != nil {
		t.Fatal(err)
	}
	if !first || again {
		t.Fatalf("event insert results were %v then %v", first, again)
	}
}

func TestUsageDebitUsesLedgerInSameTransaction(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is unset")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var user, tenant string
	token, _, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `INSERT INTO users(email,password_hash,email_verified_at) VALUES($1,'test',now()) RETURNING id`, "meter-ledger-"+token+"@example.test").Scan(&user); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `INSERT INTO tenants(owner_user_id,dstack_username,dstack_project,state,markup_bps) VALUES($1,$2,$3,'ready',1000) RETURNING id`, user, "meter-ledger-user-"+token, "meter-ledger-project-"+token).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO credit_accounts(tenant_id,balance_micros) VALUES($1,1000000)`, tenant); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.Pool.Exec(ctx, `DELETE FROM usage_records WHERE tenant_id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM credit_entries WHERE tenant_id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM credit_accounts WHERE tenant_id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM tenants WHERE id=$1`, tenant)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, user)
	}()
	meter := Meter{DB: db.Pool, Ledger: &ledger.Service{DB: db.Pool}}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	evidence := UsageEvidence{Tenant: domain.TenantID(tenant), AllocationID: "allocation-ledger", IntervalStart: start, IntervalEnd: start.Add(time.Hour), GPUCount: 1, Rate: 100_000_000, RateUnit: RateInstanceHour, PriceVerified: true, SourceVersion: "dstack-ledger-test"}
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- meter.PostUsage(ctx, evidence)
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	var balance domain.CreditMicros
	if err := db.Pool.QueryRow(ctx, `SELECT balance_micros FROM credit_accounts WHERE tenant_id=$1`, tenant).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	var entries int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM credit_entries WHERE tenant_id=$1 AND kind='usage'`, tenant).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if balance != 890_000 || entries != 1 {
		t.Fatalf("duplicate verified usage left balance %d and %d ledger entries, want 890000 and 1", balance, entries)
	}
}
