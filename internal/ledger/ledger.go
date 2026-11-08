package ledger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"billing-backend/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct{ DB *pgxpool.Pool }
type Posting struct {
	Tenant         domain.TenantID
	Delta          domain.CreditMicros
	Kind           domain.EntryKind
	IdempotencyKey string
	Source         string
	Snapshot       domain.EntrySnapshot
}
type Reservation struct {
	Admission      domain.Admission
	ResponseStatus int
	ResponseBody   []byte
	Replayed       bool
}

func (s *Service) Balance(ctx context.Context, tenant domain.TenantID) (domain.CreditMicros, error) {
	var balance domain.CreditMicros
	err := s.DB.QueryRow(ctx, `SELECT balance_micros FROM credit_accounts WHERE tenant_id=$1`, string(tenant)).Scan(&balance)
	return balance, err
}

func (s *Service) Post(ctx context.Context, post Posting) (domain.CreditEntry, error) {
	if post.Delta == 0 || post.IdempotencyKey == "" || post.Source == "" || post.Delta == domain.CreditMicros(math.MinInt64) {
		return domain.CreditEntry{}, errors.New("posting requires nonzero delta, idempotency key, and source")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return domain.CreditEntry{}, err
	}
	defer tx.Rollback(ctx)
	var balance domain.CreditMicros
	if err = tx.QueryRow(ctx, `SELECT balance_micros FROM credit_accounts WHERE tenant_id=$1 FOR UPDATE`, string(post.Tenant)).Scan(&balance); err != nil {
		return domain.CreditEntry{}, err
	}
	var prior domain.CreditEntry
	var snapshot []byte
	err = tx.QueryRow(ctx, `SELECT id,tenant_id,delta_micros,kind,idempotency_key,source,snapshot,created_at FROM credit_entries WHERE tenant_id=$1 AND idempotency_key=$2`, string(post.Tenant), post.IdempotencyKey).Scan(&prior.ID, &prior.Tenant, &prior.Delta, &prior.Kind, &prior.IdempotencyKey, &prior.Source, &snapshot, &prior.CreatedAt)
	if err == nil {
		if prior.Delta != post.Delta || prior.Kind != post.Kind || prior.Source != post.Source {
			return domain.CreditEntry{}, domain.ErrIdempotencyConflict
		}
		_ = json.Unmarshal(snapshot, &prior.Snapshot)
		if err = tx.Commit(ctx); err != nil {
			return domain.CreditEntry{}, err
		}
		return prior, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.CreditEntry{}, err
	}
	if len(post.IdempotencyKey) > 200 || len(post.Source) > 200 {
		return domain.CreditEntry{}, errors.New("posting key and source must not exceed 200 characters")
	}
	encoded, _ := json.Marshal(post.Snapshot)
	err = tx.QueryRow(ctx, `INSERT INTO credit_entries(tenant_id,delta_micros,kind,idempotency_key,source,snapshot) VALUES($1,$2,$3,$4,$5,$6) RETURNING id,created_at`, string(post.Tenant), int64(post.Delta), string(post.Kind), post.IdempotencyKey, post.Source, encoded).Scan(&prior.ID, &prior.CreatedAt)
	if err != nil {
		return domain.CreditEntry{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE credit_accounts SET balance_micros=balance_micros+$2,version=version+1,updated_at=now() WHERE tenant_id=$1`, string(post.Tenant), int64(post.Delta)); err != nil {
		return domain.CreditEntry{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.CreditEntry{}, err
	}
	return domain.CreditEntry{ID: prior.ID, Tenant: post.Tenant, Delta: post.Delta, Kind: post.Kind, IdempotencyKey: post.IdempotencyKey, Source: post.Source, Snapshot: post.Snapshot, CreatedAt: prior.CreatedAt}, nil
}

func (s *Service) Admit(ctx context.Context, tenant domain.TenantID, priceKey string) (domain.Admission, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return domain.Admission{}, err
	}
	defer tx.Rollback(ctx)
	var balance domain.CreditMicros
	var hourly domain.USDNano
	var verified bool
	var markup domain.MarkupBPS
	var state domain.TenantState
	err = tx.QueryRow(ctx, `SELECT a.balance_micros,COALESCE(p.hourly_usd_nano,0),COALESCE(p.verified,false),t.markup_bps,t.state FROM tenants t JOIN credit_accounts a ON a.tenant_id=t.id LEFT JOIN dstack_instance_prices p ON p.price_key=$2 WHERE t.id=$1 FOR UPDATE OF a`, string(tenant), priceKey).Scan(&balance, &hourly, &verified, &markup, &state)
	if err != nil {
		return domain.Admission{}, err
	}
	admission, err := domain.ComputeAdmission(balance, hourly, verified, markup, state)
	if err != nil {
		return domain.Admission{}, err
	}
	admission.Tenant = tenant
	return admission, tx.Commit(ctx)
}

func (s *Service) Reserve(ctx context.Context, tenant domain.TenantID, priceKey, requestKey string, requestHash []byte, ttl time.Duration) (Reservation, error) {
	if requestKey == "" || len(requestKey) > 200 || len(requestHash) != 32 || ttl <= 0 || ttl > 24*time.Hour {
		return Reservation{}, errors.New("compute reservation identity or expiry is invalid")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Reservation{}, err
	}
	defer tx.Rollback(ctx)
	var balance domain.CreditMicros
	var hourly domain.USDNano
	var verified bool
	var markup domain.MarkupBPS
	var state domain.TenantState
	err = tx.QueryRow(ctx, `SELECT a.balance_micros,COALESCE(p.hourly_usd_nano,0),COALESCE(p.verified,false),t.markup_bps,t.state FROM tenants t JOIN credit_accounts a ON a.tenant_id=t.id LEFT JOIN dstack_instance_prices p ON p.price_key=$2 WHERE t.id=$1 FOR UPDATE OF a`, string(tenant), priceKey).Scan(&balance, &hourly, &verified, &markup, &state)
	if err != nil {
		return Reservation{}, err
	}
	var existingHash []byte
	var existingReserve domain.CreditMicros
	var existingState string
	var responseStatus *int
	var responseBody []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,reserved_micros,state,response_status,response_body FROM compute_reservations WHERE tenant_id=$1 AND request_key=$2 FOR UPDATE`, string(tenant), requestKey).Scan(&existingHash, &existingReserve, &existingState, &responseStatus, &responseBody)
	if err == nil && !bytes.Equal(existingHash, requestHash) {
		return Reservation{}, domain.ErrIdempotencyConflict
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, err
	}
	if err == nil && existingState == "reserved" {
		if responseStatus != nil {
			if err := tx.Commit(ctx); err != nil {
				return Reservation{}, err
			}
			return Reservation{ResponseStatus: *responseStatus, ResponseBody: responseBody, Replayed: true}, nil
		}
		if err := tx.Commit(ctx); err != nil {
			return Reservation{}, err
		}
		return Reservation{}, errors.New("identical compute request is still in progress")
	}
	var outstanding domain.CreditMicros
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(reserved_micros),0) FROM compute_reservations WHERE tenant_id=$1 AND state IN ('pending','reserved') AND expires_at>now() AND NOT (request_key=$2)`, string(tenant), requestKey).Scan(&outstanding); err != nil {
		return Reservation{}, err
	}
	available := balance - outstanding
	if existingState == "pending" || existingState == "reserved" {
		available += existingReserve
	}
	admission, err := domain.ComputeAdmission(available, hourly, verified, markup, state)
	if err != nil {
		return Reservation{}, err
	}
	if existingState == "pending" || existingState == "reserved" {
		if _, err := tx.Exec(ctx, `UPDATE compute_reservations SET state='reserved',expires_at=now()+$3,updated_at=now(),response_status=NULL,response_body=NULL WHERE tenant_id=$1 AND request_key=$2`, string(tenant), requestKey, ttl); err != nil {
			return Reservation{}, err
		}
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO compute_reservations(tenant_id,request_key,request_hash,reserved_micros,state,expires_at) VALUES($1,$2,$3,$4,'reserved',now()+$5) ON CONFLICT(tenant_id,request_key) DO UPDATE SET request_hash=EXCLUDED.request_hash,reserved_micros=EXCLUDED.reserved_micros,state='reserved',expires_at=EXCLUDED.expires_at,updated_at=now()`, string(tenant), requestKey, requestHash, int64(admission.Reserved), ttl)
		if err != nil {
			return Reservation{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Reservation{}, err
	}
	return Reservation{Admission: admission}, nil
}

func (s *Service) ReleaseReservation(ctx context.Context, tenant domain.TenantID, requestKey string) error {
	_, err := s.DB.Exec(ctx, `UPDATE compute_reservations SET state='released',response_status=NULL,response_body=NULL,updated_at=now() WHERE tenant_id=$1 AND request_key=$2 AND state IN ('pending','reserved')`, string(tenant), requestKey)
	return err
}

func (s *Service) MarkReservationUncertain(ctx context.Context, tenant domain.TenantID, requestKey string) error {
	_, err := s.DB.Exec(ctx, `UPDATE compute_reservations SET expires_at=GREATEST(expires_at,now()+interval '1 hour'),updated_at=now() WHERE tenant_id=$1 AND request_key=$2 AND state='reserved'`, string(tenant), requestKey)
	return err
}

func (s *Service) CompleteReservation(ctx context.Context, tenant domain.TenantID, requestKey string, status int, body []byte) error {
	if status < 100 || status > 599 || len(body) > 8<<20 {
		return errors.New("compute response is invalid")
	}
	_, err := s.DB.Exec(ctx, `UPDATE compute_reservations SET response_status=$3,response_body=$4,updated_at=now() WHERE tenant_id=$1 AND request_key=$2 AND state='reserved'`, string(tenant), requestKey, status, body)
	return err
}

func (s *Service) ApplyPayment(ctx context.Context, tenant domain.TenantID, purchaseID domain.PurchaseID, delta domain.CreditMicros, reason string) error {
	_, err := s.Post(ctx, Posting{Tenant: tenant, Delta: delta, Kind: domain.EntryPurchase, IdempotencyKey: "purchase:" + string(purchaseID) + ":" + reason, Source: string(purchaseID), Snapshot: domain.EntrySnapshot{PurchaseID: purchaseID, Description: reason}})
	return err
}

func (s *Service) ReconcileBalance(ctx context.Context, tenant domain.TenantID) error {
	var cached, sum int64
	if err := s.DB.QueryRow(ctx, `SELECT balance_micros FROM credit_accounts WHERE tenant_id=$1`, string(tenant)).Scan(&cached); err != nil {
		return err
	}
	if err := s.DB.QueryRow(ctx, `SELECT COALESCE(sum(delta_micros),0) FROM credit_entries WHERE tenant_id=$1`, string(tenant)).Scan(&sum); err != nil {
		return err
	}
	if cached != sum {
		return errors.New("credit account cache differs from immutable ledger")
	}
	return nil
}
