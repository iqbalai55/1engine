package payments

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"billing-backend/internal/domain"
	"billing-backend/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

type SnapConfig struct {
	ServerKey     string
	ClientKey     string
	BaseURL       string
	StatusURL     string
	Environment   string
	PublicBaseURL string
}
type SnapClient struct {
	Config SnapConfig
	HTTP   *http.Client
}
type snapRequest struct {
	TransactionDetails snapTransaction `json:"transaction_details"`
	CustomerDetails    snapCustomer    `json:"customer_details"`
	Callbacks          snapCallbacks   `json:"callbacks"`
}
type snapTransaction struct {
	OrderID     string `json:"order_id"`
	GrossAmount int64  `json:"gross_amount"`
}
type snapCustomer struct {
	Email string `json:"email"`
}
type snapCallbacks struct {
	Finish string `json:"finish"`
}
type snapResponse struct {
	Token       string `json:"token"`
	RedirectURL string `json:"redirect_url"`
}
type Notification struct {
	OrderID           string `json:"order_id"`
	StatusCode        string `json:"status_code"`
	GrossAmount       string `json:"gross_amount"`
	SignatureKey      string `json:"signature_key"`
	TransactionStatus string `json:"transaction_status"`
	FraudStatus       string `json:"fraud_status"`
	TransactionID     string `json:"transaction_id"`
	RefundedAmount    string `json:"refund_amount"`
}
type PurchaseView struct {
	ID          domain.PurchaseID    `json:"id"`
	OrderID     string               `json:"order_id"`
	Credits     domain.CreditMicros  `json:"credits_micros"`
	Amount      domain.IDR           `json:"amount_idr"`
	RedirectURL string               `json:"redirect_url"`
	State       domain.PurchaseState `json:"state"`
}

type PurchaseSummary struct {
	ID       string    `json:"id"`
	Order    string    `json:"order_id"`
	Package  int       `json:"package_credits"`
	Credits  int64     `json:"credit_micros"`
	Amount   int64     `json:"amount_idr"`
	State    string    `json:"state"`
	Redirect string    `json:"redirect_url"`
	Created  time.Time `json:"created_at"`
}
type Service struct {
	DB     *pgxpool.Pool
	Ledger interface {
		PostPurchaseInTx(context.Context, store.Querier, domain.TenantID, domain.CreditMicros, string, string, []byte) error
		PostReversalInTx(context.Context, store.Querier, domain.TenantID, domain.CreditMicros, string, string, []byte) error
	}
	Snap SnapClient
}

var ErrStatusNotFound = errors.New("Midtrans purchase status not found")

func (s *Service) ListPurchases(ctx context.Context, tenant domain.TenantID, limit int) ([]PurchaseSummary, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,order_id,package_size,credit_micros,amount_idr,state,snap_redirect_url,created_at FROM purchases WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT $2`, string(tenant), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var purchases []PurchaseSummary
	for rows.Next() {
		var purchase PurchaseSummary
		if err := rows.Scan(&purchase.ID, &purchase.Order, &purchase.Package, &purchase.Credits, &purchase.Amount, &purchase.State, &purchase.Redirect, &purchase.Created); err != nil {
			return nil, err
		}
		purchases = append(purchases, purchase)
	}
	return purchases, rows.Err()
}

func (c SnapClient) Create(ctx context.Context, orderID, email string, amount domain.IDR) (snapResponse, error) {
	if c.Config.ServerKey == "" || c.Config.ClientKey == "" || c.Config.BaseURL == "" {
		return snapResponse{}, errors.New("Midtrans configuration is unavailable")
	}
	body, _ := json.Marshal(snapRequest{TransactionDetails: snapTransaction{OrderID: orderID, GrossAmount: int64(amount)}, CustomerDetails: snapCustomer{Email: email}, Callbacks: snapCallbacks{Finish: strings.TrimRight(c.Config.PublicBaseURL, "/") + "/dashboard/billing"}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Config.BaseURL, "/")+"/snap/v1/transactions", bytes.NewReader(body))
	if err != nil {
		return snapResponse{}, err
	}
	req.SetBasicAuth(c.Config.ServerKey, "")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return snapResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return snapResponse{}, fmt.Errorf("Midtrans Snap returned status %d", resp.StatusCode)
	}
	var result snapResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return snapResponse{}, err
	}
	if result.Token == "" || result.RedirectURL == "" {
		return snapResponse{}, errors.New("Midtrans Snap returned an incomplete response")
	}
	return result, nil
}

func NewOrderID() (string, error) {
	var b [16]byte
	if _, err := randRead(b[:]); err != nil {
		return "", err
	}
	return "inference-" + hex.EncodeToString(b[:]), nil
}

var randRead = rand.Read

func (s *Service) Create(ctx context.Context, tenant domain.TenantID, size domain.PackageSize) (PurchaseView, error) {
	pkg, err := domain.PackageFor(size)
	if err != nil {
		return PurchaseView{}, err
	}
	order, err := NewOrderID()
	if err != nil {
		return PurchaseView{}, err
	}
	var id domain.PurchaseID
	var email string
	err = s.DB.QueryRow(ctx, `SELECT u.email FROM users u JOIN tenants t ON t.owner_user_id=u.id WHERE t.id=$1`, string(tenant)).Scan(&email)
	if err != nil {
		return PurchaseView{}, err
	}
	err = s.DB.QueryRow(ctx, `INSERT INTO purchases(tenant_id,package_size,credit_micros,amount_idr,order_id,snap_redirect_url) VALUES($1,$2,$3,$4,$5,'pending') RETURNING id`, string(tenant), int64(pkg.Size), int64(pkg.Credits), int64(pkg.Amount), order).Scan(&id)
	if err != nil {
		return PurchaseView{}, err
	}
	result, err := s.Snap.Create(ctx, order, email, pkg.Amount)
	if err != nil {
		_, _ = s.DB.Exec(ctx, `UPDATE purchases SET snap_redirect_url='unavailable',updated_at=now() WHERE id=$1`, string(id))
		return PurchaseView{}, err
	}
	if _, err = s.DB.Exec(ctx, `UPDATE purchases SET snap_redirect_url=$2,updated_at=now() WHERE id=$1`, string(id), result.RedirectURL); err != nil {
		return PurchaseView{}, err
	}
	return PurchaseView{ID: id, OrderID: order, Credits: pkg.Credits, Amount: pkg.Amount, RedirectURL: result.RedirectURL, State: domain.PurchasePending}, nil
}

func ValidSignature(n Notification, serverKey string) bool {
	if serverKey == "" || n.OrderID == "" || n.StatusCode == "" || n.GrossAmount == "" || n.SignatureKey == "" {
		return false
	}
	sum := sha512.Sum512([]byte(n.OrderID + n.StatusCode + n.GrossAmount + serverKey))
	want := []byte(hex.EncodeToString(sum[:]))
	got := []byte(strings.ToLower(n.SignatureKey))
	return len(got) == len(want) && subtle.ConstantTimeCompare(got, want) == 1
}

func validateNotificationStatusCode(n Notification) error {
	if n.StatusCode != "200" {
		return errors.New("Midtrans notification status_code is not successful")
	}
	return nil
}

func (s *Service) ObserveNotification(ctx context.Context, raw []byte, serverKey string) error {
	var n Notification
	if err := json.Unmarshal(raw, &n); err != nil {
		return errors.New("invalid notification JSON")
	}
	if !ValidSignature(n, serverKey) {
		return errors.New("invalid Midtrans notification signature")
	}
	if err := validateNotificationStatusCode(n); err != nil {
		return err
	}
	if isAmbiguousPaymentStatus(n.TransactionStatus) {
		status, statusErr := s.Snap.notificationStatus(ctx, n.OrderID)
		if statusErr != nil {
			return statusErr
		}
		if status.TransactionStatus == "settlement" && status.FraudStatus != "accept" {
			return nil
		}
		if status.TransactionStatus == "refund" || status.TransactionStatus == "partial_refund" || status.TransactionStatus == "chargeback" {
			return s.ApplyRefund(ctx, n.OrderID, status.RefundedAmount, status.TransactionID)
		}
		return s.ApplyStatus(ctx, n.OrderID, status.TransactionStatus, status.TransactionID)
	}
	var amount string
	parsedAmount, err := parseIDR(n.GrossAmount)
	if err != nil || parsedAmount <= 0 {
		return errors.New("invalid gross_amount")
	}
	amount = n.GrossAmount
	encoded, err := json.Marshal(n)
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var purchaseID string
	var purchaseAmount int64
	var currentState string
	err = tx.QueryRow(ctx, `SELECT id,amount_idr,state FROM purchases WHERE order_id=$1 FOR UPDATE`, n.OrderID).Scan(&purchaseID, &purchaseAmount, &currentState)
	if err != nil {
		return errors.New("unknown purchase order")
	}
	if parsedAmount != purchaseAmount {
		return errors.New("Midtrans gross_amount does not match purchase")
	}
	if (currentState == "settlement" || currentState == "refund") && n.TransactionStatus != "refund" && n.TransactionStatus != "partial_refund" && n.TransactionStatus != "chargeback" {
		return nil
	}
	if (currentState == "deny" || currentState == "cancel" || currentState == "expire") && n.TransactionStatus != "refund" && n.TransactionStatus != "partial_refund" && n.TransactionStatus != "chargeback" {
		return nil
	}
	var eventKey = n.OrderID + ":" + n.TransactionID + ":" + n.TransactionStatus + ":" + n.FraudStatus + ":" + amount
	_, err = tx.Exec(ctx, `INSERT INTO payment_observations(purchase_id,source,event_key,status,gross_amount,payload) VALUES($1,'notification',$2,$3,$4,$5) ON CONFLICT(purchase_id,source,event_key) DO NOTHING`, purchaseID, eventKey, n.TransactionStatus, amount, encoded)
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if n.TransactionStatus == "settlement" {
		if n.FraudStatus != "accept" && n.FraudStatus != "" {
			return nil
		}
		return s.ApplyStatus(ctx, n.OrderID, "settlement", n.TransactionID)
	}
	if n.TransactionStatus == "refund" || n.TransactionStatus == "partial_refund" || n.TransactionStatus == "chargeback" {
		return s.ApplyRefund(ctx, n.OrderID, n.RefundedAmount, n.TransactionID)
	}
	if n.TransactionStatus != "deny" && n.TransactionStatus != "cancel" && n.TransactionStatus != "expire" {
		return nil
	}
	return s.ApplyStatus(ctx, n.OrderID, n.TransactionStatus, n.TransactionID)
}

func isAmbiguousPaymentStatus(status string) bool {
	switch status {
	case "pending", "capture", "authorize", "partial_capture", "refund", "partial_refund", "chargeback":
		return true
	default:
		return false
	}
}

func normalizeStatus(status string) string {
	switch status {
	case "settlement":
		return "settlement"
	case "deny":
		return "deny"
	case "cancel":
		return "cancel"
	case "expire":
		return "expire"
	case "refund", "partial_refund", "chargeback":
		return "refund"
	default:
		return "pending"
	}
}

func parseIDR(value string) (int64, error) {
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" {
		return 0, errors.New("invalid IDR amount")
	}
	whole, ok := new(big.Int).SetString(parts[0], 10)
	if !ok || whole.Sign() < 0 {
		return 0, errors.New("invalid IDR amount")
	}
	if len(parts) == 2 && strings.Trim(parts[1], "0") != "" {
		return 0, errors.New("fractional IDR amount is invalid")
	}
	if !whole.IsInt64() {
		return 0, errors.New("IDR amount exceeds supported range")
	}
	return whole.Int64(), nil
}

func (s *Service) ReconcileOpen(ctx context.Context, limit int) error {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.DB.Query(ctx, `SELECT order_id,amount_idr FROM purchases WHERE state='pending' ORDER BY created_at LIMIT $1`, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	type pending struct {
		order  string
		amount int64
	}
	var orders []pending
	for rows.Next() {
		var item pending
		if err := rows.Scan(&item.order, &item.amount); err != nil {
			return err
		}
		orders = append(orders, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, order := range orders {
		var notification map[string]any
		if err := s.Snap.status(ctx, order.order, &notification); err != nil {
			continue
		}
		statusCode, _ := notification["status_code"].(string)
		if statusCode != "200" {
			continue
		}
		status, _ := notification["transaction_status"].(string)
		fraudStatus, _ := notification["fraud_status"].(string)
		txID, _ := notification["transaction_id"].(string)
		if status != "" {
			if status == "refund" || status == "partial_refund" || status == "chargeback" {
				refund, _ := notification["refund_amount"].(string)
				if err := s.ApplyRefund(ctx, order.order, refund, txID); err != nil {
					return err
				}
				continue
			}
			if status == "settlement" && fraudStatus != "accept" {
				continue
			}
			if status != "settlement" && status != "deny" && status != "cancel" && status != "expire" {
				continue
			}
			if err := s.ApplyStatus(ctx, order.order, status, txID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c SnapClient) status(ctx context.Context, order string, out *map[string]any) error {
	base := c.Config.StatusURL
	if base == "" {
		base = c.Config.BaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v2/"+url.PathEscape(order)+"/status", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.Config.ServerKey, "")
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrStatusNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Midtrans status returned %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func (c SnapClient) notificationStatus(ctx context.Context, order string) (Notification, error) {
	var body map[string]any
	if err := c.status(ctx, order, &body); err != nil {
		return Notification{}, err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return Notification{}, err
	}
	var status Notification
	if err := json.Unmarshal(encoded, &status); err != nil {
		return Notification{}, err
	}
	if status.StatusCode != "200" {
		return Notification{}, errors.New("Midtrans status response is not successful")
	}
	return status, nil
}

func (s *Service) ApplyStatus(ctx context.Context, orderID, status, transactionID string) error {
	if s.Ledger == nil {
		return errors.New("credit ledger is unavailable")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var tenant, purchaseID, currentState string
	var credits, amount int64
	err = tx.QueryRow(ctx, `SELECT id,tenant_id,credit_micros,amount_idr,state FROM purchases WHERE order_id=$1 FOR UPDATE`, orderID).Scan(&purchaseID, &tenant, &credits, &amount, &currentState)
	if err != nil {
		return err
	}
	if status != "settlement" && status != "deny" && status != "cancel" && status != "expire" {
		return tx.Commit(ctx)
	}
	_, err = tx.Exec(ctx, `INSERT INTO payment_observations(purchase_id,source,event_key,status,gross_amount,payload) VALUES($1,'status',$2,$3,$4,jsonb_build_object('order_id',$5::text)) ON CONFLICT(purchase_id,source,event_key) DO NOTHING`, purchaseID, orderID+":"+transactionID+":"+status, status, strconv.FormatInt(amount, 10), orderID)
	if err != nil {
		return err
	}
	if status == "settlement" && currentState != "settlement" && currentState != "refund" && currentState != "deny" && currentState != "cancel" && currentState != "expire" {
		snapshot, marshalErr := json.Marshal(map[string]any{"purchase_id": purchaseID, "amount_idr": amount})
		if marshalErr != nil {
			return marshalErr
		}
		postErr := s.Ledger.PostPurchaseInTx(ctx, tx, domain.TenantID(tenant), domain.CreditMicros(credits), "purchase:"+purchaseID+":settlement", purchaseID, snapshot)
		if postErr != nil {
			return postErr
		}
	}
	nextState := normalizeStatus(status)
	if domain.PurchaseState(currentState).CanTransitionTo(domain.PurchaseState(nextState)) {
		if _, err = tx.Exec(ctx, `UPDATE purchases SET state=$2,updated_at=now() WHERE id=$1`, purchaseID, nextState); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Service) ApplyRefund(ctx context.Context, orderID, refundedAmount, transactionID string) error {
	if s.Ledger == nil {
		return errors.New("credit ledger is unavailable")
	}
	if refundedAmount == "" {
		return errors.New("refund notification is missing cumulative refund amount")
	}
	refund, err := parseIDR(refundedAmount)
	if err != nil || refund < 0 {
		return errors.New("invalid refunded amount")
	}
	if transactionID == "" {
		return errors.New("refund notification is missing transaction ID")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var purchaseID, tenant string
	var credits, amount, prior int64
	var state string
	err = tx.QueryRow(ctx, `SELECT id,tenant_id,credit_micros,amount_idr,refunded_idr,state FROM purchases WHERE order_id=$1 FOR UPDATE`, orderID).Scan(&purchaseID, &tenant, &credits, &amount, &prior, &state)
	if err != nil {
		return err
	}
	newRefund := int64(refund)
	if newRefund > amount {
		newRefund = amount
	}
	_, err = tx.Exec(ctx, `INSERT INTO payment_observations(purchase_id,source,event_key,status,gross_amount,payload) VALUES($1,'status',$2,'refund',$3,jsonb_build_object('order_id',$4::text,'transaction_id',$5::text)) ON CONFLICT(purchase_id,source,event_key) DO NOTHING`, purchaseID, orderID+":"+transactionID+":refund:"+strconv.FormatInt(newRefund, 10), strconv.FormatInt(newRefund, 10), orderID, transactionID)
	if err != nil {
		return err
	}
	if state != "settlement" && state != "refund" {
		if domain.PurchaseState(state).CanTransitionTo(domain.PurchaseRefunded) {
			_, err = tx.Exec(ctx, `UPDATE purchases SET state='refund',refunded_idr=GREATEST(refunded_idr,$2),updated_at=now() WHERE id=$1`, purchaseID, newRefund)
		} else {
			_, err = tx.Exec(ctx, `UPDATE purchases SET refunded_idr=GREATEST(refunded_idr,$2),updated_at=now() WHERE id=$1`, purchaseID, newRefund)
		}
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if newRefund <= prior {
		return tx.Commit(ctx)
	}
	delta := refundCreditDelta(credits, amount, prior, newRefund)
	if delta < 0 {
		return errors.New("cumulative refund credits regressed")
	}
	if delta == 0 {
		_, err = tx.Exec(ctx, `UPDATE purchases SET refunded_idr=GREATEST(refunded_idr,$2),state='refund',updated_at=now() WHERE id=$1`, purchaseID, newRefund)
		if err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	snapshot, err := json.Marshal(map[string]any{"purchase_id": purchaseID, "refunded_idr": newRefund})
	if err != nil {
		return err
	}
	err = s.Ledger.PostReversalInTx(ctx, tx, domain.TenantID(tenant), -delta, "purchase:"+purchaseID+":refund:"+strconv.FormatInt(newRefund, 10), purchaseID, snapshot)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE purchases SET refunded_idr=GREATEST(refunded_idr,$2),state='refund',updated_at=now() WHERE id=$1`, purchaseID, newRefund)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func refundCreditDelta(credits, amount, prior, current int64) domain.CreditMicros {
	newCumulative := (credits/amount)*current + (credits%amount*current+amount-1)/amount
	oldCumulative := (credits/amount)*prior + (credits%amount*prior+amount-1)/amount
	return domain.CreditMicros(newCumulative - oldCumulative)
}
