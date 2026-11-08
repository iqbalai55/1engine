package domain

import (
	"errors"
	"math/big"
	"time"
)

type TenantID string
type UserID string
type KeyID string
type PurchaseID string
type ProjectName string
type CreditMicros int64
type USDNano int64
type IDR int64
type MarkupBPS uint32
type PackageSize uint32

const (
	CreditPerUSD CreditMicros = 1_000_000
	IDRPerCredit IDR          = 18_000
	MaxMarkupBPS MarkupBPS    = 10_000
)

var (
	ErrInvalidPackage      = errors.New("unsupported credit package")
	ErrInsufficientCredits = errors.New("compute requires a positive credit balance")
	ErrPriceUnverified     = errors.New("compute price is missing or unverified")
	ErrTenantUnavailable   = errors.New("tenant is not ready for compute")
	ErrIdempotencyConflict = errors.New("idempotency key was used for a different operation")
)

type Package struct {
	Size    PackageSize
	Credits CreditMicros
	Amount  IDR
}

var packages = map[PackageSize]Package{
	10:  {Size: 10, Credits: 10 * CreditPerUSD, Amount: 180_000},
	50:  {Size: 50, Credits: 50 * CreditPerUSD, Amount: 900_000},
	100: {Size: 100, Credits: 100 * CreditPerUSD, Amount: 1_800_000},
}

func PackageFor(size PackageSize) (Package, error) {
	p, ok := packages[size]
	if !ok {
		return Package{}, ErrInvalidPackage
	}
	return p, nil
}

type TenantState string

const (
	TenantProvisioning TenantState = "provisioning"
	TenantReady        TenantState = "ready"
	TenantSuspended    TenantState = "suspended"
)

type ProvisionState string

const (
	ProvisionPending ProvisionState = "pending"
	ProvisionReady   ProvisionState = "ready"
	ProvisionFailed  ProvisionState = "failed"
)

type Tenant struct {
	ID     TenantID
	Owner  UserID
	DStack DStackBinding
	Markup MarkupBPS
	State  TenantState
}

type DStackBinding struct {
	Username  string
	Project   ProjectName
	ProjectID string
	Provision ProvisionState
}

type Admission struct {
	Tenant      TenantID
	Balance     CreditMicros
	HourlyPrice USDNano
	Markup      MarkupBPS
	Reserved    CreditMicros
}

func ComputeAdmission(balance CreditMicros, hourlyPrice USDNano, priceVerified bool, markup MarkupBPS, tenantState TenantState) (Admission, error) {
	if tenantState != TenantReady {
		return Admission{}, ErrTenantUnavailable
	}
	if balance <= 0 {
		return Admission{}, ErrInsufficientCredits
	}
	if !priceVerified || hourlyPrice <= 0 {
		return Admission{}, ErrPriceUnverified
	}
	if markup > MaxMarkupBPS {
		return Admission{}, errors.New("markup exceeds maximum")
	}
	// Reserve one hour at the verified instance price plus customer markup.
	numerator := new(big.Int).Mul(big.NewInt(int64(hourlyPrice)), big.NewInt(int64(10_000+markup)))
	numerator.Mul(numerator, big.NewInt(int64(CreditPerUSD)))
	creditDenominator := new(big.Int).Mul(big.NewInt(10_000), big.NewInt(1_000_000_000))
	creditNumerator := new(big.Int).Mul(big.NewInt(int64(hourlyPrice)), big.NewInt(int64(10_000+markup)))
	creditNumerator.Mul(creditNumerator, big.NewInt(int64(CreditPerUSD)))
	creditNumerator.Add(creditNumerator, new(big.Int).Sub(creditDenominator, big.NewInt(1)))
	creditNumerator.Quo(creditNumerator, creditDenominator)
	numerator = creditNumerator
	if !numerator.IsInt64() {
		return Admission{}, errors.New("hourly reserve exceeds supported credit range")
	}
	reserved := CreditMicros(numerator.Int64())
	if reserved < 1 {
		reserved = 1
	}
	if balance < reserved {
		return Admission{}, ErrInsufficientCredits
	}
	return Admission{Balance: balance, HourlyPrice: hourlyPrice, Markup: markup, Reserved: reserved}, nil
}

type EntryKind string

const (
	EntryPurchase EntryKind = "purchase"
	EntryUsage    EntryKind = "usage"
	EntryRefund   EntryKind = "refund"
	EntryReversal EntryKind = "reversal"
)

type CreditEntry struct {
	ID             string
	Tenant         TenantID
	Delta          CreditMicros
	Kind           EntryKind
	IdempotencyKey string
	Source         string
	Snapshot       EntrySnapshot
	CreatedAt      time.Time
}

type EntrySnapshot struct {
	PurchaseID  PurchaseID
	USD         USDNano
	Markup      MarkupBPS
	Description string
}

type PurchaseState string

const (
	PurchasePending    PurchaseState = "pending"
	PurchaseSettlement PurchaseState = "settlement"
	PurchaseDenied     PurchaseState = "deny"
	PurchaseCancelled  PurchaseState = "cancel"
	PurchaseExpired    PurchaseState = "expire"
	PurchaseRefunded   PurchaseState = "refund"
)

type Purchase struct {
	ID          PurchaseID
	Tenant      TenantID
	Package     Package
	OrderID     string
	State       PurchaseState
	RefundedIDR IDR
	CreatedAt   time.Time
}

type PriceSnapshot struct {
	HourlyUSD  USDNano
	RateUnit   string
	Verified   bool
	Version    string
	ObservedAt time.Time
}

func (current PurchaseState) CanTransitionTo(next PurchaseState) bool {
	if current == next || current == PurchaseRefunded {
		return false
	}
	if current == PurchasePending {
		return next == PurchaseSettlement || next == PurchaseDenied || next == PurchaseCancelled || next == PurchaseExpired || next == PurchaseRefunded
	}
	return next == PurchaseRefunded && (current == PurchaseSettlement || current == PurchaseDenied || current == PurchaseCancelled || current == PurchaseExpired)
}

func UsageCharge(start, end time.Time, rate USDNano, markup MarkupBPS) (CreditMicros, USDNano, error) {
	if !end.After(start) || rate <= 0 || markup > MaxMarkupBPS {
		return 0, 0, errors.New("invalid usage evidence or pricing policy")
	}
	nanos := end.Sub(start).Nanoseconds()
	numerator := new(big.Int).Mul(big.NewInt(int64(rate)), big.NewInt(nanos))
	numerator.Mul(numerator, big.NewInt(int64(10_000+markup)))
	denominator := new(big.Int).Mul(big.NewInt(int64(time.Hour)), big.NewInt(10_000))
	marked := new(big.Int).Add(new(big.Int).Set(numerator), new(big.Int).Sub(denominator, big.NewInt(1)))
	marked.Quo(marked, denominator)
	creditNumerator := new(big.Int).Mul(new(big.Int).Set(numerator), big.NewInt(int64(CreditPerUSD)))
	creditDenominator := new(big.Int).Mul(denominator, big.NewInt(1_000_000_000))
	credits := new(big.Int).Add(creditNumerator, new(big.Int).Sub(creditDenominator, big.NewInt(1)))
	credits.Quo(credits, creditDenominator)
	if !marked.IsInt64() || !credits.IsInt64() {
		return 0, 0, errors.New("usage amount exceeds supported fixed-point range")
	}
	return CreditMicros(credits.Int64()), USDNano(marked.Int64()), nil
}
