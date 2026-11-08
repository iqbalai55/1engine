package compute

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"billing-backend/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Observation struct {
	Tenant     domain.TenantID
	Stream     string
	ID         string
	Version    string
	ObservedAt time.Time
	Payload    json.RawMessage
}

type ObservationPage struct {
	Observations []Observation
	NextAt       *time.Time
	NextID       string
	Done         bool
}

type ObservationSource interface {
	ListTenantRuns(context.Context, domain.TenantID, *time.Time, string, time.Time) (ObservationPage, error)
	ListTenantInstances(context.Context, domain.TenantID, *time.Time, string, time.Time) (ObservationPage, error)
	ListTenantEvents(context.Context, domain.TenantID, *time.Time, string, time.Time) (ObservationPage, error)
}

type Reconciler struct {
	DB     *pgxpool.Pool
	Source ObservationSource
	Now    func() time.Time
}

const observationOverlap = 24 * time.Hour

type Interval struct {
	Start time.Time
	End   time.Time
}

func ActiveInterval(start time.Time, finishedAt *time.Time, now time.Time) (Interval, bool) {
	end := now
	if finishedAt != nil {
		end = *finishedAt
	}
	if !end.After(start) {
		return Interval{}, false
	}
	return Interval{Start: start.UTC(), End: end.UTC()}, true
}

func ReconcileUsageInterval(observedStart time.Time, finishedAt *time.Time, now time.Time, previouslyChargedThrough *time.Time) (Interval, bool) {
	interval, ok := ActiveInterval(observedStart, finishedAt, now)
	if !ok {
		return Interval{}, false
	}
	if previouslyChargedThrough == nil {
		return interval, true
	}
	start := previouslyChargedThrough.UTC()
	if start.After(interval.End) {
		return Interval{}, false
	}
	if start.After(interval.Start) {
		interval.Start = start
	}
	return interval, interval.End.After(interval.Start)
}

func (r *Reconciler) PollTenant(ctx context.Context, tenant domain.TenantID) error {
	if r.Source == nil {
		return errors.New("dstack observation source is unavailable")
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	for _, stream := range []string{"runs", "instances", "events"} {
		if err := r.pollStream(ctx, tenant, stream, now); err != nil {
			return err
		}
	}
	return r.meterPersistedInstances(ctx, tenant, now)
}

func (r *Reconciler) meterPersistedInstances(ctx context.Context, tenant domain.TenantID, now time.Time) error {
	rows, err := r.DB.Query(ctx, `SELECT DISTINCT ON (observation_id) observation_id,source_version,payload FROM dstack_observations WHERE tenant_id=$1 AND stream='instances' ORDER BY observation_id,observed_at DESC,recorded_at DESC LIMIT 10000`, string(tenant))
	if err != nil {
		return err
	}
	type persistedInstance struct {
		id      string
		version string
		payload []byte
	}
	instances := make([]persistedInstance, 0)
	for rows.Next() {
		var instance persistedInstance
		if err := rows.Scan(&instance.id, &instance.version, &instance.payload); err != nil {
			rows.Close()
			return err
		}
		instances = append(instances, instance)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	meter := &Meter{DB: r.DB}
	for _, instance := range instances {
		snapshot, err := decodeInstanceUsageSnapshot(instance.id, instance.version, instance.payload)
		if err != nil {
			return err
		}
		if snapshot.FinishedAt == nil && now.Before(snapshot.Created) {
			continue
		}
		var previous *time.Time
		var lastEnd time.Time
		err = r.DB.QueryRow(ctx, `SELECT interval_end FROM usage_records WHERE tenant_id=$1 AND allocation_id=$2 ORDER BY interval_end DESC LIMIT 1`, string(tenant), instance.id).Scan(&lastEnd)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			previous = &lastEnd
		}
		price := InstancePrice{}
		err = r.DB.QueryRow(ctx, `SELECT hourly_usd_nano,rate_unit,verified,version FROM dstack_instance_prices WHERE price_key='default'`).Scan(&price.HourlyUSDNano, &price.RateUnit, &price.Verified, &price.Version)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if errors.Is(err, pgx.ErrNoRows) {
			price = InstancePrice{}
		}
		evidence, ok := BuildInstanceUsageEvidence(tenant, snapshot, price, now, previous)
		if !ok {
			continue
		}
		if err := meter.PostUsage(ctx, evidence); err != nil {
			return err
		}
	}
	return nil
}

func decodeInstanceUsageSnapshot(id, sourceVersion string, payload []byte) (InstanceUsageSnapshot, error) {
	var wire struct {
		Created      time.Time       `json:"created"`
		FinishedAt   *time.Time      `json:"finished_at"`
		Price        json.RawMessage `json:"price"`
		InstanceType *struct {
			Resources *struct {
				GPUs json.RawMessage `json:"gpus"`
			} `json:"resources"`
		} `json:"instance_type"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		return InstanceUsageSnapshot{}, err
	}
	if wire.Created.IsZero() {
		return InstanceUsageSnapshot{}, errors.New("persisted dstack instance has no creation time")
	}
	snapshot := InstanceUsageSnapshot{ID: id, Created: wire.Created, FinishedAt: wire.FinishedAt, SourceVersion: sourceVersion, PriceRaw: string(wire.Price)}
	if wire.InstanceType != nil && wire.InstanceType.Resources != nil {
		if len(wire.InstanceType.Resources.GPUs) == 0 || wire.InstanceType.Resources.GPUs[0] != '[' {
			return snapshot, nil
		}
		var gpus []json.RawMessage
		if err := json.Unmarshal(wire.InstanceType.Resources.GPUs, &gpus); err == nil {
			snapshot.GPUCountKnown = true
			if uint64(len(gpus)) > uint64(^uint32(0)) {
				return InstanceUsageSnapshot{}, errors.New("dstack instance GPU count exceeds supported range")
			}
			snapshot.GPUCount = uint32(len(gpus))
		} else {
			return snapshot, nil
		}
	}
	return snapshot, nil
}

func (r *Reconciler) pollStream(ctx context.Context, tenant domain.TenantID, stream string, now time.Time) error {
	conn, err := r.DB.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	lockKey := string(tenant) + ":" + stream
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, lockKey).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return nil
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1,0))`, lockKey)
	var checkpoint *time.Time
	var checkpointID string
	var resumeAt *time.Time
	var resumeID string
	var inProgress bool
	err = r.DB.QueryRow(ctx, `SELECT checkpoint_at,checkpoint_id,resume_at,resume_id FROM dstack_poll_cursors WHERE tenant_id=$1 AND stream=$2`, string(tenant), stream).Scan(&checkpoint, &checkpointID, &resumeAt, &resumeID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	inProgress = resumeAt != nil
	if errors.Is(err, pgx.ErrNoRows) && stream == "events" {
		if err := r.DB.QueryRow(ctx, `SELECT cursor FROM usage_cursors WHERE tenant_id=$1`, string(tenant)).Scan(&checkpoint); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	if !inProgress && checkpoint != nil {
		// dstack documents delayed event visibility, so overlap the completed checkpoint window.
		if stream == "events" {
			resume := checkpoint.Add(-observationOverlap)
			resumeAt = &resume
			resumeID = ""
		} else {
			resumeAt = checkpoint
			resumeID = checkpointID
		}
	}
	for pages := 0; pages < 100; pages++ {
		page, err := listStream(ctx, r.Source, tenant, stream, resumeAt, resumeID, now)
		if err != nil {
			return err
		}
		if err := r.persistPage(ctx, tenant, stream, checkpoint, page, now); err != nil {
			return err
		}
		if page.Done {
			return nil
		}
		if page.NextAt == nil || page.NextID == "" {
			return errors.New("dstack observation source returned an invalid pagination cursor")
		}
		resumeAt = page.NextAt
		resumeID = page.NextID
	}
	return errors.New("dstack observation pagination exceeded page limit")
}

func listStream(ctx context.Context, source ObservationSource, tenant domain.TenantID, stream string, at *time.Time, id string, now time.Time) (ObservationPage, error) {
	switch stream {
	case "runs":
		return source.ListTenantRuns(ctx, tenant, at, id, now)
	case "instances":
		return source.ListTenantInstances(ctx, tenant, at, id, now)
	case "events":
		return source.ListTenantEvents(ctx, tenant, at, id, now)
	default:
		return ObservationPage{}, errors.New("unsupported dstack observation stream")
	}
}

func (r *Reconciler) persistPage(ctx context.Context, tenant domain.TenantID, stream string, checkpoint *time.Time, page ObservationPage, now time.Time) error {
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, observation := range page.Observations {
		if observation.Tenant != tenant || observation.Stream != stream || observation.ID == "" || !json.Valid(observation.Payload) {
			return errors.New("dstack observation is invalid or not tenant-scoped")
		}
		version := observation.Version
		if version == "" {
			sum := sha256.Sum256(observation.Payload)
			version = hex.EncodeToString(sum[:])
		}
		if _, err := tx.Exec(ctx, `INSERT INTO dstack_observations(tenant_id,stream,observation_id,source_version,observed_at,payload) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, string(tenant), stream, observation.ID, version, observation.ObservedAt, observation.Payload); err != nil {
			return err
		}
		if stream == "runs" {
			if _, err := tx.Exec(ctx, `INSERT INTO dstack_runs(tenant_id,run_id,payload,observed_at) VALUES($1,$2,$3,$4) ON CONFLICT(tenant_id,run_id) DO UPDATE SET payload=EXCLUDED.payload,observed_at=EXCLUDED.observed_at`, string(tenant), observation.ID, observation.Payload, observation.ObservedAt); err != nil {
				return err
			}
		}
	}
	if page.Done {
		var checkpointID string
		if checkpoint != nil {
			_ = tx.QueryRow(ctx, `SELECT checkpoint_id FROM dstack_poll_cursors WHERE tenant_id=$1 AND stream=$2`, string(tenant), stream).Scan(&checkpointID)
		}
		_, err = tx.Exec(ctx, `INSERT INTO dstack_poll_cursors(tenant_id,stream,checkpoint_at,checkpoint_id,resume_at,resume_id,updated_at) VALUES($1,$2,$3,$4,NULL,NULL,$5) ON CONFLICT(tenant_id,stream) DO UPDATE SET checkpoint_at=EXCLUDED.checkpoint_at,checkpoint_id=EXCLUDED.checkpoint_id,resume_at=NULL,resume_id=NULL,updated_at=EXCLUDED.updated_at`, string(tenant), stream, page.NextAt, page.NextID, now)
	} else {
		if page.NextAt == nil || page.NextID == "" {
			return fmt.Errorf("%s observation page has no continuation cursor", stream)
		}
		var checkpointID string
		if checkpoint != nil {
			_ = tx.QueryRow(ctx, `SELECT checkpoint_id FROM dstack_poll_cursors WHERE tenant_id=$1 AND stream=$2`, string(tenant), stream).Scan(&checkpointID)
		}
		_, err = tx.Exec(ctx, `INSERT INTO dstack_poll_cursors(tenant_id,stream,checkpoint_at,checkpoint_id,resume_at,resume_id,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(tenant_id,stream) DO UPDATE SET checkpoint_at=EXCLUDED.checkpoint_at,checkpoint_id=EXCLUDED.checkpoint_id,resume_at=EXCLUDED.resume_at,resume_id=EXCLUDED.resume_id,updated_at=EXCLUDED.updated_at`, string(tenant), checkpoint, checkpointID, page.NextAt, page.NextID, now)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
