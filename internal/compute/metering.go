package compute

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"billing-backend/internal/domain"
	"billing-backend/internal/ledger"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RateUnit string

const (
	RateInstanceHour RateUnit = "instance_hour"
	RateGPUHour      RateUnit = "gpu_hour"
	RateUnpriced     RateUnit = "unpriced"
)

type UsageEvidence struct {
	Tenant        domain.TenantID
	AllocationID  string
	SubmissionID  string
	IntervalStart time.Time
	IntervalEnd   time.Time
	GPUCount      uint32
	Rate          domain.USDNano
	RateUnit      RateUnit
	PriceVerified bool
	SourceVersion string
	Provenance    map[string]any
}

type InstanceUsageSnapshot struct {
	ID            string
	Created       time.Time
	FinishedAt    *time.Time
	GPUCount      uint32
	GPUCountKnown bool
	PriceBasis    RateUnit
	PriceUSDNano  *domain.USDNano
	PriceRaw      string
	SourceVersion string
}

type InstancePrice struct {
	HourlyUSDNano domain.USDNano
	RateUnit      RateUnit
	Verified      bool
	Version       string
}

func BuildInstanceUsageEvidence(tenant domain.TenantID, snapshot InstanceUsageSnapshot, price InstancePrice, now time.Time, previouslyRecordedThrough *time.Time) (UsageEvidence, bool) {
	if tenant == "" || snapshot.ID == "" || snapshot.SourceVersion == "" || snapshot.Created.IsZero() {
		return UsageEvidence{}, false
	}
	if snapshot.FinishedAt != nil && snapshot.FinishedAt.Before(snapshot.Created) {
		return UsageEvidence{}, false
	}
	if snapshot.GPUCountKnown && snapshot.GPUCount == 0 {
		return UsageEvidence{}, false
	}
	observedEnd := now.UTC()
	if snapshot.FinishedAt != nil {
		observedEnd = snapshot.FinishedAt.UTC()
	} else {
		observedEnd = observedEnd.Truncate(time.Minute)
		if observedEnd.Sub(snapshot.Created.UTC()) < time.Minute {
			return UsageEvidence{}, false
		}
	}
	start := snapshot.Created.UTC()
	if previouslyRecordedThrough != nil {
		start = previouslyRecordedThrough.UTC()
	}
	if !observedEnd.After(start) {
		return UsageEvidence{}, false
	}
	end := start.Truncate(time.Minute).Add(time.Minute)
	if end.After(observedEnd) {
		if snapshot.FinishedAt == nil {
			return UsageEvidence{}, false
		}
		end = observedEnd
	}
	if !end.After(start) {
		return UsageEvidence{}, false
	}

	verified := snapshot.GPUCountKnown && snapshot.GPUCount > 0 && snapshot.PriceBasis == RateInstanceHour && snapshot.PriceUSDNano != nil && price.Verified && price.Version != "" && price.RateUnit == RateInstanceHour && price.HourlyUSDNano > 0 && *snapshot.PriceUSDNano == price.HourlyUSDNano
	rate, unit := domain.USDNano(0), RateUnpriced
	if verified {
		rate, unit = price.HourlyUSDNano, RateInstanceHour
	}
	provenance := map[string]any{
		"source":               "dstack_instance",
		"price_version":        price.Version,
		"instance_price_raw":   snapshot.PriceRaw,
		"instance_price_basis": snapshot.PriceBasis,
		"gpu_count_known":      snapshot.GPUCountKnown,
	}
	if snapshot.PriceUSDNano != nil {
		provenance["instance_price_usd_nano"] = int64(*snapshot.PriceUSDNano)
	}
	return UsageEvidence{
		Tenant:        tenant,
		AllocationID:  snapshot.ID,
		IntervalStart: start,
		IntervalEnd:   end,
		GPUCount:      snapshot.GPUCount,
		Rate:          rate,
		RateUnit:      unit,
		PriceVerified: verified,
		SourceVersion: snapshot.SourceVersion,
		Provenance:    provenance,
	}, true
}

type Meter struct {
	DB     *pgxpool.Pool
	Ledger *ledger.Service
}

func (m *Meter) PostUsage(ctx context.Context, e UsageEvidence) error {
	if e.Tenant == "" || e.AllocationID == "" || e.SourceVersion == "" || !e.IntervalEnd.After(e.IntervalStart) {
		return errors.New("usage evidence is incomplete")
	}
	tx, err := m.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var balance domain.CreditMicros
	var markup domain.MarkupBPS
	if err = tx.QueryRow(ctx, `SELECT a.balance_micros,t.markup_bps FROM tenants t JOIN credit_accounts a ON a.tenant_id=t.id WHERE t.id=$1 FOR UPDATE OF a`, string(e.Tenant)).Scan(&balance, &markup); err != nil {
		return err
	}
	unit := string(e.RateUnit)
	var cost domain.USDNano
	var credits domain.CreditMicros
	rate := e.Rate
	if e.RateUnit != RateInstanceHour && e.RateUnit != RateGPUHour {
		unit = string(RateUnpriced)
	}
	if e.PriceVerified && (e.RateUnit == RateInstanceHour || e.RateUnit == RateGPUHour) {
		if e.RateUnit == RateGPUHour {
			if e.GPUCount == 0 {
				unit = string(RateUnpriced)
			} else if uint64(rate) > uint64(math.MaxInt64)/uint64(e.GPUCount) {
				return errors.New("GPU-hour rate exceeds supported range")
			} else {
				rate = domain.USDNano(int64(rate) * int64(e.GPUCount))
			}
		}
		if unit == string(RateUnpriced) {
			rate = 0
		}
		if rate > 0 {
			var chargeErr error
			credits, cost, chargeErr = domain.UsageCharge(e.IntervalStart, e.IntervalEnd, rate, markup)
			if chargeErr != nil {
				return chargeErr
			}
		}
	} else {
		unit = string(RateUnpriced)
	}
	provenance, err := json.Marshal(e.Provenance)
	if err != nil {
		return err
	}
	var inserted bool
	err = tx.QueryRow(ctx, `INSERT INTO usage_records(tenant_id,allocation_id,submission_id,interval_start,interval_end,gpu_count,rate_usd_nano,rate_unit,usd_nano,markup_bps,credit_delta_micros,source_version,provenance) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,0),$8,NULLIF($9,0),$10,$11,$12,$13) ON CONFLICT(tenant_id,allocation_id,interval_start,interval_end) DO NOTHING RETURNING true`, string(e.Tenant), e.AllocationID, e.SubmissionID, e.IntervalStart, e.IntervalEnd, e.GPUCount, int64(e.Rate), unit, int64(cost), int64(markup), int64(credits), e.SourceVersion, provenance).Scan(&inserted)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if !inserted {
		var sourceVersion string
		if err := tx.QueryRow(ctx, `SELECT source_version FROM usage_records WHERE tenant_id=$1 AND allocation_id=$2 AND interval_start=$3 AND interval_end=$4`, string(e.Tenant), e.AllocationID, e.IntervalStart, e.IntervalEnd).Scan(&sourceVersion); err != nil {
			return err
		}
		if sourceVersion != e.SourceVersion {
			return errors.New("usage interval was repeated with a different dstack source version")
		}
		return tx.Commit(ctx)
	}
	if len(e.AllocationID) > 200 || len(e.SubmissionID) > 200 || len(e.SourceVersion) > 200 {
		return errors.New("usage identifiers exceed supported length")
	}
	if credits > 0 {
		key := "usage:" + string(e.Tenant) + ":" + e.AllocationID + ":" + e.IntervalStart.UTC().Format(time.RFC3339Nano) + ":" + e.IntervalEnd.UTC().Format(time.RFC3339Nano)
		if _, err = tx.Exec(ctx, `INSERT INTO credit_entries(tenant_id,delta_micros,kind,idempotency_key,source,snapshot) VALUES($1,$2,'usage',$3,$4,jsonb_build_object('usd_nano',$5,'markup_bps',$6,'allocation_id',$7))`, string(e.Tenant), -int64(credits), key, e.AllocationID, int64(cost), int64(markup)); err != nil {
			return err
		}
		var reserved domain.CreditMicros
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(reserved_micros),0) FROM compute_reservations WHERE tenant_id=$1 AND state='reserved' AND expires_at>now()`, string(e.Tenant)).Scan(&reserved); err != nil {
			return err
		}
		if balance+reserved-credits < 0 {
			if _, err := tx.Exec(ctx, `UPDATE credit_accounts SET balance_micros=balance_micros-$2,version=version+1,updated_at=now() WHERE tenant_id=$1`, string(e.Tenant), int64(credits)); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
		if _, err = tx.Exec(ctx, `UPDATE credit_accounts SET balance_micros=balance_micros-$2,version=version+1,updated_at=now() WHERE tenant_id=$1`, string(e.Tenant), int64(credits)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (m *Meter) RecordEvent(ctx context.Context, tenant domain.TenantID, eventID string, observed time.Time, payload json.RawMessage) (bool, error) {
	if eventID == "" || !json.Valid(payload) {
		return false, errors.New("invalid dstack event")
	}
	result, err := m.DB.Exec(ctx, `INSERT INTO dstack_events(tenant_id,event_id,observed_at,payload) VALUES($1,$2,$3,$4) ON CONFLICT(tenant_id,event_id) DO NOTHING`, string(tenant), eventID, observed, payload)
	return err == nil && result.RowsAffected() == 1, err
}
