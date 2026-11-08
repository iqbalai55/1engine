package domain

import (
	"errors"
	"testing"
	"time"
)

func TestPackagesHaveFixedCreditAndIDRValues(t *testing.T) {
	want := map[PackageSize]Package{10: {Size: 10, Credits: 10_000_000, Amount: 180_000}, 50: {Size: 50, Credits: 50_000_000, Amount: 900_000}, 100: {Size: 100, Credits: 100_000_000, Amount: 1_800_000}}
	for size, expected := range want {
		got, err := PackageFor(size)
		if err != nil || got != expected {
			t.Fatalf("package %d = %#v, %v; want %#v", size, got, err, expected)
		}
	}
	if _, err := PackageFor(25); !errors.Is(err, ErrInvalidPackage) {
		t.Fatalf("unsupported package error = %v", err)
	}
}

func TestComputeAdmissionRequiresReadyTenantPositiveBalanceAndVerifiedPrice(t *testing.T) {
	oneCredit := CreditMicros(1_000_000)
	if _, err := ComputeAdmission(oneCredit, 100_000_000, true, 1000, TenantReady); err != nil {
		t.Fatalf("one hour reserve at $0.10/hour should fit one credit: %v", err)
	}
	if _, err := ComputeAdmission(109_999, 100_000_000, true, 1000, TenantReady); !errors.Is(err, ErrInsufficientCredits) {
		t.Fatalf("under-reserved balance error = %v", err)
	}
	if got, err := ComputeAdmission(110_000, 100_000_000, true, 1000, TenantReady); err != nil || got.Reserved != 110_000 {
		t.Fatalf("exact reserve = %#v, %v", got, err)
	}
	if _, err := ComputeAdmission(oneCredit, 100_000_000, false, 1000, TenantReady); !errors.Is(err, ErrPriceUnverified) {
		t.Fatalf("unverified price error = %v", err)
	}
	if _, err := ComputeAdmission(oneCredit, 100_000_000, true, 1000, TenantProvisioning); !errors.Is(err, ErrTenantUnavailable) {
		t.Fatalf("provisioning state error = %v", err)
	}
	if _, err := ComputeAdmission(110_000, 100_000_000, true, 1000, TenantReady); err != nil {
		t.Fatalf("exact balance admission failed: %v", err)
	}
	if _, err := ComputeAdmission(0, 100_000_000, true, 1000, TenantReady); !errors.Is(err, ErrInsufficientCredits) {
		t.Fatalf("zero balance error=%v", err)
	}
	if _, err := ComputeAdmission(-1, 100_000_000, true, 1000, TenantReady); !errors.Is(err, ErrInsufficientCredits) {
		t.Fatalf("negative balance error=%v", err)
	}
}

func TestUsageChargeUsesFixedPointAndMarkup(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	credits, usd, err := UsageCharge(start, start.Add(time.Hour), 100_000_000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if usd != 110_000_000 || credits != 110_000 {
		t.Fatalf("charge = %d nanos USD and %d credit micros, want 110000000 and 110000", usd, credits)
	}
}
