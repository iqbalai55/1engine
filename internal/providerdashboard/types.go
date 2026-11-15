package providerdashboard

import "time"

type Snapshot struct {
	GeneratedAt time.Time    `json:"generated_at"`
	WindowStart time.Time    `json:"window_start"`
	WindowEnd   time.Time    `json:"window_end"`
	Overview    Overview     `json:"overview"`
	Customers   CustomerPage `json:"customers"`
	Billing     BillingPage  `json:"billing"`
	Price       Price        `json:"price"`
}

type Overview struct {
	TenantTotal           int64   `json:"tenant_total"`
	TenantReady           int64   `json:"tenant_ready"`
	TenantProvisioning    int64   `json:"tenant_provisioning"`
	TenantSuspended       int64   `json:"tenant_suspended"`
	ProvisionPending      int64   `json:"provision_pending"`
	ProvisionFailed       int64   `json:"provision_failed"`
	CreditBalanceMicros   *string `json:"credit_balance_micros"`
	CreditBalanceComplete bool    `json:"credit_balance_complete"`
	UsageDebitedMicros30d string  `json:"usage_debited_micros_30d"`
	SettledNetIDR30d      string  `json:"settled_net_idr_30d"`
	PendingPurchaseCount  int64   `json:"pending_purchase_count"`
	PendingPurchaseAmount string  `json:"pending_purchase_amount_idr"`
	SettledRefundsIDR30d  string  `json:"settled_refunds_idr_30d"`
}

type Price struct {
	HourlyUSDNano *string    `json:"hourly_usd_nano"`
	Verified      bool       `json:"verified"`
	Version       *string    `json:"version"`
	ObservedAt    *time.Time `json:"observed_at"`
}

type CustomerPage struct {
	Items      []Customer `json:"items"`
	Limit      int        `json:"limit"`
	NextCursor *string    `json:"next_cursor"`
}

type Customer struct {
	TenantID       string    `json:"tenant_id"`
	Email          string    `json:"email"`
	CreatedAt      time.Time `json:"created_at"`
	TenantState    string    `json:"tenant_state"`
	ProvisionState string    `json:"provision_state"`
	MarkupBPS      int32     `json:"markup_bps"`
	DStackUsername string    `json:"dstack_username"`
	DStackProject  string    `json:"dstack_project"`
}

type BillingPage struct {
	Items      []Purchase `json:"items"`
	Limit      int        `json:"limit"`
	NextCursor *string    `json:"next_cursor"`
}

type Purchase struct {
	ID           string    `json:"id"`
	TenantID     string    `json:"tenant_id"`
	Email        string    `json:"email"`
	OrderID      string    `json:"order_id"`
	PackageSize  int32     `json:"package_credits"`
	CreditMicros string    `json:"credit_micros"`
	AmountIDR    string    `json:"amount_idr"`
	RefundedIDR  string    `json:"refunded_idr"`
	State        string    `json:"state"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}
