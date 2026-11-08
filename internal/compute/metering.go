package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"billing-backend/internal/domain"
	"billing-backend/internal/store"
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

type UsageRecord struct {
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
	Ledger interface {
		PostUsageInTx(context.Context, store.Querier, domain.TenantID, domain.CreditMicros, string, string, []byte) error
	}
}

func (m *Meter) ListUsage(ctx context.Context, tenant domain.TenantID, limit int) ([]UsageRecord, error) {
	rows, err := m.DB.Query(ctx, `SELECT allocation_id,submission_id,interval_start,interval_end,gpu_count,rate_usd_nano,rate_unit,usd_nano,markup_bps,credit_delta_micros,source_version FROM usage_records WHERE tenant_id=$1 ORDER BY interval_start DESC LIMIT $2`, string(tenant), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []UsageRecord
	for rows.Next() {
		var record UsageRecord
		if err := rows.Scan(&record.Allocation, &record.Submission, &record.Start, &record.End, &record.GPUs, &record.Rate, &record.RateUnit, &record.USD, &record.Markup, &record.Credits, &record.Version); err != nil {
			return nil, fmt.Errorf("read tenant usage: %w", err)
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (m *Meter) PostUsage(ctx context.Context, e UsageEvidence) error {
	if m.Ledger == nil {
		return errors.New("credit ledger is unavailable")
	}
	if e.Tenant == "" || e.AllocationID == "" || e.SourceVersion == "" || !e.IntervalEnd.After(e.IntervalStart) {
		return errors.New("usage evidence is incomplete")
	}
	tx, err := m.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var markup domain.MarkupBPS
	if err = tx.QueryRow(ctx, `SELECT t.markup_bps FROM tenants t WHERE t.id=$1`, string(e.Tenant)).Scan(&markup); err != nil {
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
		snapshot, err := json.Marshal(map[string]any{"usd_nano": int64(cost), "markup_bps": int64(markup), "allocation_id": e.AllocationID})
		if err != nil {
			return err
		}
		err = m.Ledger.PostUsageInTx(ctx, tx, e.Tenant, -credits, key, e.AllocationID, snapshot)
		if err != nil {
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
