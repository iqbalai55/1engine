package payments

import (
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestMidtransSignatureUsesExactGrossAmountString(t *testing.T) {
	n := Notification{OrderID: "order-1", StatusCode: "200", GrossAmount: "180000.00"}
	hash := sha512.Sum512([]byte(n.OrderID + n.StatusCode + n.GrossAmount + "server-key"))
	n.SignatureKey = hex.EncodeToString(hash[:])
	if !ValidSignature(n, "server-key") {
		t.Fatal("valid notification signature rejected")
	}
	n.GrossAmount = "180000"
	if ValidSignature(n, "server-key") {
		t.Fatal("signature accepted after amount string changed")
	}
}

func TestUnknownNotificationStatusCannotGrant(t *testing.T) {
	if got := normalizeStatus("some-future-value"); got != "pending" {
		t.Fatalf("unknown status normalized to %q", got)
	}
}

func TestNotificationRequiresSuccessfulMidtransStatusCode(t *testing.T) {
	n := Notification{OrderID: "order-1", StatusCode: "201", GrossAmount: "180000", SignatureKey: "valid"}
	if n.StatusCode == "200" {
		t.Fatal("test status unexpectedly successful")
	}
	if err := validateNotificationStatusCode(n); err == nil {
		t.Fatal("non-200 status_code was accepted")
	}
	n.StatusCode = "200"
	if err := validateNotificationStatusCode(n); err != nil {
		t.Fatal(err)
	}
}

func TestNon200SignedNotificationCannotGrantSettlement(t *testing.T) {
	n := Notification{OrderID: "order-1", StatusCode: "201", GrossAmount: "180000", TransactionStatus: "settlement", FraudStatus: "accept"}
	hash := sha512.Sum512([]byte(n.OrderID + n.StatusCode + n.GrossAmount + "server-key"))
	n.SignatureKey = hex.EncodeToString(hash[:])
	raw, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Notification
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if !ValidSignature(decoded, "server-key") {
		t.Fatal("test notification signature is invalid")
	}
	if err := validateNotificationStatusCode(decoded); err == nil {
		t.Fatal("signed non-200 settlement became eligible for a grant")
	}
}

func TestAmbiguousNotificationStatusesRequireStatusLookup(t *testing.T) {
	for _, status := range []string{"pending", "capture", "authorize", "partial_capture", "refund", "partial_refund", "chargeback"} {
		if !isAmbiguousPaymentStatus(status) {
			t.Errorf("%q did not require status reconciliation", status)
		}
	}
	for _, status := range []string{"settlement", "deny", "cancel", "expire"} {
		if isAmbiguousPaymentStatus(status) {
			t.Errorf("%q unexpectedly requires reconciliation", status)
		}
	}
}

func TestPurchaseTransitionsAreMonotonicAndRefundCompensatesSettlement(t *testing.T) {
	allowed := [][2]string{{"pending", "settlement"}, {"pending", "cancel"}, {"settlement", "refund"}, {"cancel", "refund"}}
	for _, transition := range allowed {
		if !canAdvancePurchase(transition[0], transition[1]) {
			t.Errorf("transition %s -> %s was rejected", transition[0], transition[1])
		}
	}
	denied := [][2]string{{"settlement", "pending"}, {"settlement", "deny"}, {"refund", "settlement"}, {"cancel", "settlement"}, {"expire", "pending"}}
	for _, transition := range denied {
		if canAdvancePurchase(transition[0], transition[1]) {
			t.Errorf("transition %s -> %s was allowed", transition[0], transition[1])
		}
	}
}

func TestRefundDeltaIsCumulativeAndBounded(t *testing.T) {
	credits, amount, prior, current := int64(10_000_000), int64(180_000), int64(45_000), int64(90_000)
	if delta := refundCreditDelta(credits, amount, prior, current); delta != 2_500_000 {
		t.Fatalf("cumulative refund delta = %d, want 2500000", delta)
	}
	if delta := refundCreditDelta(credits, amount, current, current); delta != 0 {
		t.Fatalf("duplicate cumulative refund delta = %d, want 0", delta)
	}
}

func TestInitialStatusNotFoundDoesNotTriggerOrderRecreation(t *testing.T) {
	if ErrStatusNotFound == nil {
		t.Fatal("status 404 is not classified")
	}
	if ErrStatusNotFound.Error() != "Midtrans purchase status not found" {
		t.Fatalf("unexpected 404 classification %q", ErrStatusNotFound)
	}
}

func TestIDRParsingDoesNotUseFloatingPoint(t *testing.T) {
	for _, input := range []string{"180000", "180000.00"} {
		got, err := parseIDR(input)
		if err != nil || got != 180000 {
			t.Fatalf("parse IDR %q = %d, %v", input, got, err)
		}
	}
	for _, input := range []string{"180000.01", "NaN", "999999999999999999999999"} {
		if _, err := parseIDR(input); err == nil {
			t.Fatalf("invalid IDR %q was accepted", input)
		}
	}
}
