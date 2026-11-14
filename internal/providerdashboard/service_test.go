package providerdashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPageSizeBounds(t *testing.T) {
	for _, tc := range []struct {
		input int
		want  int
		bad   bool
	}{{0, 20, false}, {1, 1, false}, {50, 50, false}, {-1, 0, true}, {51, 0, true}} {
		got, err := pageSize(tc.input)
		if (err != nil) != tc.bad || got != tc.want {
			t.Fatalf("pageSize(%d) = %d, %v; want %d, error=%v", tc.input, got, err, tc.want, tc.bad)
		}
	}
}

func TestCursorRoundTripAndRejectsInvalidValues(t *testing.T) {
	created := time.Date(2026, 11, 14, 12, 30, 0, 123, time.UTC)
	cursor := encodeCursor(pageCursor{Kind: "customer", At: created, ID: "f91f9f77-f52d-472f-93d1-d780df056f42", Search: "alice%_"})
	decoded, err := decodeCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Kind != "customer" || !decoded.At.Equal(created) || decoded.Search != "alice%_" {
		t.Fatalf("cursor round trip = %#v", decoded)
	}
	for _, invalid := range []string{"", "not-base64", "eA", "Y3VzdG9tZXIKbm90LWRhdGUKbm90LXV1aWQK"} {
		if _, err := decodeCursor(invalid); err == nil {
			t.Errorf("decodeCursor(%q) accepted invalid cursor", invalid)
		}
	}
}

func TestDashboardAmountsSerializeAsStrings(t *testing.T) {
	amount := "9223372036854775807"
	snapshot := Snapshot{
		Overview: Overview{
			CreditBalanceMicros:   &amount,
			CreditBalanceComplete: true,
			SettledNetIDR30d:      amount,
		},
		Billing: BillingPage{Items: []Purchase{{CreditMicros: amount, AmountIDR: amount, RefundedIDR: "0"}}},
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, expected := range []string{`"credit_balance_micros":"` + amount + `"`, `"settled_net_idr_30d":"` + amount + `"`, `"credit_micros":"` + amount + `"`} {
		if !strings.Contains(text, expected) {
			t.Errorf("JSON does not contain exact amount %s: %s", expected, text)
		}
	}
}
