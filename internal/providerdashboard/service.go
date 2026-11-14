package providerdashboard

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"billing-backend/internal/pricing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultPageSize = 20
	maxPageSize     = 50
	defaultWindow   = 30 * 24 * time.Hour
)

var (
	ErrInvalidCursor = errors.New("invalid page cursor")
	ErrInvalidQuery  = errors.New("invalid provider dashboard query")
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type Service struct {
	DB      *pgxpool.Pool
	Pricing *pricing.Service
}

func (s *Service) Snapshot(ctx context.Context, limit int) (Snapshot, error) {
	limit, err := pageSize(limit)
	if err != nil {
		return Snapshot{}, err
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Snapshot{}, err
	}
	defer tx.Rollback(ctx)

	var generatedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&generatedAt); err != nil {
		return Snapshot{}, err
	}
	windowStart := generatedAt.Add(-defaultWindow)
	overview, err := readOverview(ctx, tx, windowStart, generatedAt)
	if err != nil {
		return Snapshot{}, err
	}
	customers, err := readCustomers(ctx, tx, limit, "", nil)
	if err != nil {
		return Snapshot{}, err
	}
	billing, err := readBilling(ctx, tx, limit, nil)
	if err != nil {
		return Snapshot{}, err
	}
	price := Price{Verified: false}
	if s.Pricing != nil {
		current, priceErr := s.Pricing.CurrentIn(ctx, tx, "default")
		if priceErr != nil && !errors.Is(priceErr, pgx.ErrNoRows) {
			return Snapshot{}, priceErr
		}
		if priceErr == nil {
			usd := strconv.FormatInt(int64(current.HourlyUSD), 10)
			price = Price{HourlyUSDNano: &usd, Verified: current.Verified, Version: &current.Version, ObservedAt: &current.ObservedAt}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Snapshot{}, err
	}
	return Snapshot{GeneratedAt: generatedAt, WindowStart: windowStart, WindowEnd: generatedAt, Overview: overview, Customers: customers, Billing: billing, Price: price}, nil
}

func (s *Service) Customers(ctx context.Context, limit int, cursor, query string) (CustomerPage, error) {
	limit, err := pageSize(limit)
	if err != nil {
		return CustomerPage{}, err
	}
	query = strings.TrimSpace(query)
	if len(query) > 200 {
		return CustomerPage{}, fmt.Errorf("%w: search is too long", ErrInvalidQuery)
	}
	query = strings.ToLower(query)
	var after *pageCursor
	if cursor != "" {
		parsed, err := decodeCursor(cursor)
		if err != nil || parsed.Kind != "customer" {
			return CustomerPage{}, ErrInvalidCursor
		}
		if parsed.Search != query {
			return CustomerPage{}, ErrInvalidCursor
		}
		if !uuidPattern.MatchString(parsed.ID) {
			return CustomerPage{}, ErrInvalidCursor
		}
		after = &parsed
	}
	return readCustomers(ctx, s.DB, limit, query, after)
}

func (s *Service) Billing(ctx context.Context, limit int, cursor string) (BillingPage, error) {
	limit, err := pageSize(limit)
	if err != nil {
		return BillingPage{}, err
	}
	var after *pageCursor
	if cursor != "" {
		parsed, err := decodeCursor(cursor)
		if err != nil || parsed.Kind != "billing" {
			return BillingPage{}, ErrInvalidCursor
		}
		after = &parsed
	}
	return readBilling(ctx, s.DB, limit, after)
}

func readOverview(ctx context.Context, q queryer, from, to time.Time) (Overview, error) {
	var result Overview
	var balance, usage, settledNet, pendingAmount, refunds string
	var accounts, tenants int64
	err := q.QueryRow(ctx, `
WITH tenant_totals AS (
  SELECT count(*) AS total,
    count(*) FILTER (WHERE state='ready') AS ready,
    count(*) FILTER (WHERE state='provisioning') AS provisioning,
    count(*) FILTER (WHERE state='suspended') AS suspended,
    count(*) FILTER (WHERE dstack_provision_state='pending') AS provision_pending,
    count(*) FILTER (WHERE dstack_provision_state='failed') AS provision_failed
  FROM tenants
), account_totals AS (
  SELECT count(*) AS accounts, COALESCE(sum(balance_micros),0)::text AS balance FROM credit_accounts
), window_usage AS (
  SELECT COALESCE(-(sum(delta_micros) FILTER (WHERE delta_micros < 0)),0)::text AS debited
  FROM credit_entries WHERE kind='usage' AND created_at >= $1 AND created_at < $2
), settled AS (
	SELECT p.id, p.amount_idr, p.refunded_idr FROM purchases p
  WHERE p.created_at >= $1 AND p.created_at < $2
    AND EXISTS (SELECT 1 FROM credit_entries e WHERE e.kind='purchase' AND e.source=p.id::text AND e.idempotency_key='purchase:' || p.id::text || ':settlement')
), payment_totals AS (
  SELECT COALESCE(sum(amount_idr - refunded_idr),0)::text AS net,
    COALESCE(sum(refunded_idr),0)::text AS refunds FROM settled
), pending AS (
  SELECT count(*) AS count, COALESCE(sum(amount_idr),0)::text AS amount FROM purchases WHERE state='pending'
)
SELECT tenant_totals.total, tenant_totals.ready, tenant_totals.provisioning, tenant_totals.suspended,
 tenant_totals.provision_pending, tenant_totals.provision_failed,
 account_totals.accounts, account_totals.balance,
 window_usage.debited, payment_totals.net, payment_totals.refunds,
 pending.count, pending.amount
 FROM tenant_totals CROSS JOIN account_totals CROSS JOIN window_usage CROSS JOIN payment_totals CROSS JOIN pending`, from, to).Scan(
		&tenants, &result.TenantReady, &result.TenantProvisioning, &result.TenantSuspended,
		&result.ProvisionPending, &result.ProvisionFailed, &accounts, &balance,
		&usage, &settledNet, &refunds, &result.PendingPurchaseCount, &pendingAmount,
	)
	if err != nil {
		return Overview{}, err
	}
	result.TenantTotal = tenants
	result.CreditBalanceComplete = tenants == accounts
	if result.CreditBalanceComplete {
		result.CreditBalanceMicros = &balance
	}
	result.UsageDebitedMicros30d = usage
	result.SettledNetIDR30d = settledNet
	result.SettledRefundsIDR30d = refunds
	result.PendingPurchaseAmount = pendingAmount
	return result, nil
}

type queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readCustomers(ctx context.Context, q queryer, limit int, search string, after *pageCursor) (CustomerPage, error) {
	pattern := "%" + strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(strings.ToLower(search)) + "%"
	if after != nil && !uuidPattern.MatchString(after.ID) {
		return CustomerPage{}, ErrInvalidCursor
	}
	var afterAt any
	var afterID any
	if after != nil {
		afterAt, afterID = after.At, after.ID
	}
	rows, err := q.Query(ctx, `
SELECT t.id::text, u.email, t.created_at, t.state, t.dstack_provision_state, t.markup_bps, t.dstack_username, t.dstack_project
FROM tenants t JOIN users u ON u.id=t.owner_user_id
WHERE ($1='' OR lower(u.email) LIKE $2 ESCAPE E'\\' OR t.id::text LIKE $2 ESCAPE E'\\' OR lower(t.dstack_username) LIKE $2 ESCAPE E'\\' OR lower(t.dstack_project) LIKE $2 ESCAPE E'\\')
  AND ($3::timestamptz IS NULL OR (t.created_at,t.id) < ($3,$4::uuid))
ORDER BY t.created_at DESC, t.id DESC LIMIT $5`, search, pattern, afterAt, afterID, limit+1)
	if err != nil {
		return CustomerPage{}, err
	}
	defer rows.Close()
	page := CustomerPage{Items: make([]Customer, 0, limit), Limit: limit}
	for rows.Next() {
		var customer Customer
		if err := rows.Scan(&customer.TenantID, &customer.Email, &customer.CreatedAt, &customer.TenantState, &customer.ProvisionState, &customer.MarkupBPS, &customer.DStackUsername, &customer.DStackProject); err != nil {
			return CustomerPage{}, err
		}
		page.Items = append(page.Items, customer)
	}
	if err := rows.Err(); err != nil {
		return CustomerPage{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[len(page.Items)-1]
		cursor := encodeCursor(pageCursor{Kind: "customer", At: last.CreatedAt, ID: last.TenantID, Search: search})
		page.NextCursor = &cursor
	}
	return page, nil
}

func readBilling(ctx context.Context, q queryer, limit int, after *pageCursor) (BillingPage, error) {
	if after != nil && !uuidPattern.MatchString(after.ID) {
		return BillingPage{}, ErrInvalidCursor
	}
	var afterAt any
	var afterID any
	if after != nil {
		afterAt, afterID = after.At, after.ID
	}
	rows, err := q.Query(ctx, `
SELECT p.id::text, p.tenant_id::text, u.email, p.order_id, p.package_size, p.credit_micros::text,
 p.amount_idr::text, p.refunded_idr::text, p.state, p.created_at, p.updated_at
FROM purchases p JOIN tenants t ON t.id=p.tenant_id JOIN users u ON u.id=t.owner_user_id
WHERE ($1::timestamptz IS NULL OR (p.created_at,p.id) < ($1,$2::uuid))
ORDER BY p.created_at DESC, p.id DESC LIMIT $3`, afterAt, afterID, limit+1)
	if err != nil {
		return BillingPage{}, err
	}
	defer rows.Close()
	page := BillingPage{Items: make([]Purchase, 0, limit), Limit: limit}
	for rows.Next() {
		var purchase Purchase
		if err := rows.Scan(&purchase.ID, &purchase.TenantID, &purchase.Email, &purchase.OrderID, &purchase.PackageSize, &purchase.CreditMicros, &purchase.AmountIDR, &purchase.RefundedIDR, &purchase.State, &purchase.CreatedAt, &purchase.UpdatedAt); err != nil {
			return BillingPage{}, err
		}
		page.Items = append(page.Items, purchase)
	}
	if err := rows.Err(); err != nil {
		return BillingPage{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[len(page.Items)-1]
		cursor := encodeCursor(pageCursor{Kind: "billing", At: last.CreatedAt, ID: last.ID})
		page.NextCursor = &cursor
	}
	return page, nil
}

func pageSize(limit int) (int, error) {
	if limit == 0 {
		return defaultPageSize, nil
	}
	if limit < 1 || limit > maxPageSize {
		return 0, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidQuery, maxPageSize)
	}
	return limit, nil
}

type pageCursor struct {
	Kind   string
	At     time.Time
	ID     string
	Search string
}

func encodeCursor(cursor pageCursor) string {
	raw := cursor.Kind + "\n" + cursor.At.UTC().Format(time.RFC3339Nano) + "\n" + cursor.ID + "\n" + base64.RawURLEncoding.EncodeToString([]byte(cursor.Search))
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(value string) (pageCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return pageCursor{}, ErrInvalidCursor
	}
	parts := strings.Split(string(decoded), "\n")
	if len(parts) != 4 || (parts[0] != "customer" && parts[0] != "billing") || !uuidPattern.MatchString(parts[2]) {
		return pageCursor{}, ErrInvalidCursor
	}
	at, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil || parts[2] == "" {
		return pageCursor{}, ErrInvalidCursor
	}
	search, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return pageCursor{}, ErrInvalidCursor
	}
	return pageCursor{Kind: parts[0], At: at, ID: parts[2], Search: string(search)}, nil
}
