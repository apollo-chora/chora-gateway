// tx_mappers_whitebox_test.go — white-box (package clients) coverage for the
// unexported proto↔token mappers in transaction_history_client.go +
// tenancy_admin_client.go + payments_client.go that the black-box suite can
// only reach indirectly.
package clients

import (
	"testing"
	"time"

	paymentsv1 "github.com/apollo-chora/chora-contracts/gen/go/chora/services/payments/v1"
	tenancyv1 "github.com/apollo-chora/chora-contracts/gen/go/chora/services/tenancy/v1"
)

func TestTokenToFormat_Branches(t *testing.T) {
	cases := map[string]tenancyv1.ExportFormat{
		"csv":  tenancyv1.ExportFormat_EXPORT_FORMAT_CSV,
		"json": tenancyv1.ExportFormat_EXPORT_FORMAT_JSON,
		"xlsx": tenancyv1.ExportFormat_EXPORT_FORMAT_UNSPECIFIED,
		"":     tenancyv1.ExportFormat_EXPORT_FORMAT_UNSPECIFIED,
	}
	for tok, want := range cases {
		if got := tokenToFormat(tok); got != want {
			t.Errorf("tokenToFormat(%q) = %v; want %v", tok, got, want)
		}
	}
}

func TestFormatToToken_Branches(t *testing.T) {
	if got := formatToToken(tenancyv1.ExportFormat_EXPORT_FORMAT_CSV); got != "csv" {
		t.Errorf("formatToToken(csv) = %q", got)
	}
	if got := formatToToken(tenancyv1.ExportFormat_EXPORT_FORMAT_JSON); got != "json" {
		t.Errorf("formatToToken(json) = %q", got)
	}
	if got := formatToToken(tenancyv1.ExportFormat_EXPORT_FORMAT_UNSPECIFIED); got != "" {
		t.Errorf("formatToToken(unspecified) = %q; want empty", got)
	}
}

func TestExportStatusToToken_Branches(t *testing.T) {
	cases := map[tenancyv1.ExportStatus]string{
		tenancyv1.ExportStatus_EXPORT_STATUS_PENDING:     "pending",
		tenancyv1.ExportStatus_EXPORT_STATUS_BUILDING:    "building",
		tenancyv1.ExportStatus_EXPORT_STATUS_READY:       "ready",
		tenancyv1.ExportStatus_EXPORT_STATUS_FAILED:      "failed",
		tenancyv1.ExportStatus_EXPORT_STATUS_UNSPECIFIED: "",
	}
	for st, want := range cases {
		if got := exportStatusToToken(st); got != want {
			t.Errorf("exportStatusToToken(%v) = %q; want %q", st, got, want)
		}
	}
}

func TestTokenToKind_Branches(t *testing.T) {
	cases := map[string]tenancyv1.TransactionKind{
		"purchase":         tenancyv1.TransactionKind_TRANSACTION_KIND_PURCHASE,
		"mana_topup":       tenancyv1.TransactionKind_TRANSACTION_KIND_MANA_TOPUP,
		"mana_spend_daily": tenancyv1.TransactionKind_TRANSACTION_KIND_MANA_SPEND_DAILY,
		"bogus":            tenancyv1.TransactionKind_TRANSACTION_KIND_UNSPECIFIED,
		"":                 tenancyv1.TransactionKind_TRANSACTION_KIND_UNSPECIFIED,
	}
	for tok, want := range cases {
		if got := tokenToKind(tok); got != want {
			t.Errorf("tokenToKind(%q) = %v; want %v", tok, got, want)
		}
	}
}

func TestKindToToken_Branches(t *testing.T) {
	if got := kindToToken(tenancyv1.TransactionKind_TRANSACTION_KIND_PURCHASE); got != "purchase" {
		t.Errorf("kindToToken(purchase) = %q", got)
	}
	if got := kindToToken(tenancyv1.TransactionKind_TRANSACTION_KIND_MANA_TOPUP); got != "mana_topup" {
		t.Errorf("kindToToken(mana_topup) = %q", got)
	}
	if got := kindToToken(tenancyv1.TransactionKind_TRANSACTION_KIND_UNSPECIFIED); got != "" {
		t.Errorf("kindToToken(unspecified) = %q; want empty", got)
	}
}

func TestTokenToStatus_Branches(t *testing.T) {
	cases := map[string]tenancyv1.TransactionStatus{
		"captured": tenancyv1.TransactionStatus_TRANSACTION_STATUS_CAPTURED,
		"refunded": tenancyv1.TransactionStatus_TRANSACTION_STATUS_REFUNDED,
		"failed":   tenancyv1.TransactionStatus_TRANSACTION_STATUS_FAILED,
		"expired":  tenancyv1.TransactionStatus_TRANSACTION_STATUS_EXPIRED,
		"posted":   tenancyv1.TransactionStatus_TRANSACTION_STATUS_POSTED,
		"all":      tenancyv1.TransactionStatus_TRANSACTION_STATUS_UNSPECIFIED,
	}
	for tok, want := range cases {
		if got := tokenToStatus(tok); got != want {
			t.Errorf("tokenToStatus(%q) = %v; want %v", tok, got, want)
		}
	}
}

func TestStatusToToken_Branches(t *testing.T) {
	cases := map[tenancyv1.TransactionStatus]string{
		tenancyv1.TransactionStatus_TRANSACTION_STATUS_CAPTURED:    "captured",
		tenancyv1.TransactionStatus_TRANSACTION_STATUS_REFUNDED:    "refunded",
		tenancyv1.TransactionStatus_TRANSACTION_STATUS_FAILED:      "failed",
		tenancyv1.TransactionStatus_TRANSACTION_STATUS_EXPIRED:     "expired",
		tenancyv1.TransactionStatus_TRANSACTION_STATUS_POSTED:      "posted",
		tenancyv1.TransactionStatus_TRANSACTION_STATUS_UNSPECIFIED: "",
	}
	for st, want := range cases {
		if got := statusToToken(st); got != want {
			t.Errorf("statusToToken(%v) = %q; want %q", st, got, want)
		}
	}
}

func TestNonNilInt64Map_Branches(t *testing.T) {
	if m := nonNilInt64Map(nil); len(m) != 0 {
		t.Errorf("nonNilInt64Map(nil) = %v; want empty", m)
	}
	if m := nonNilInt64Map(map[string]int64{"a": 1}); m["a"] != 1 {
		t.Errorf("nonNilInt64Map(copy) = %v", m)
	}
}

func TestTimestampProto_NilForZero(t *testing.T) {
	if ts := timestampProto(time.Time{}); ts != nil {
		t.Errorf("timestampProto(zero) = %v; want nil", ts)
	}
	ts := timestampProto(time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
	if ts == nil || ts.AsTime().Year() != 2026 {
		t.Errorf("timestampProto(2026-07-01) = %v", ts)
	}
}

func TestHostingModeMappers_AllBranches(t *testing.T) {
	// Valid host modes round-trip.
	for _, tok := range []string{"PLATFORM_HOSTED", "WHITE_LABEL", "FRANCHISE", "SELF_HOST"} {
		proto, err := hostingModeToProto(tok)
		if err != nil {
			t.Fatalf("hostingModeToProto(%q): %v", tok, err)
		}
		if got := hostingModeToWire(proto); got != tok {
			t.Errorf("round-trip %q → wire %q", tok, got)
		}
	}
	// Empty → UNSPECIFIED (server applies default).
	if proto, err := hostingModeToProto(""); err != nil || proto != tenancyv1.HostingMode_HOSTING_MODE_UNSPECIFIED {
		t.Errorf("hostingModeToProto() = (%v, %v); want (UNSPECIFIED, nil)", proto, err)
	}
	// Unknown → error.
	if _, err := hostingModeToProto("k8s-bare-metal"); err == nil {
		t.Error("unknown hosting mode: expected error")
	}
	// UNSPECIFIED → empty wire string.
	if got := hostingModeToWire(tenancyv1.HostingMode_HOSTING_MODE_UNSPECIFIED); got != "" {
		t.Errorf("hostingModeToWire(UNSPECIFIED) = %q; want empty", got)
	}
}

func TestTenantStateToWire_AllBranches(t *testing.T) {
	cases := map[tenancyv1.TenantState]string{
		tenancyv1.TenantState_TENANT_STATE_UNSPECIFIED: "",
		tenancyv1.TenantState_TENANT_STATE_ACTIVE:      "ACTIVE",
		tenancyv1.TenantState_TENANT_STATE_SUSPENDED:   "SUSPENDED",
	}
	for st, want := range cases {
		if got := tenantStateToWire(st); got != want {
			t.Errorf("tenantStateToWire(%v) = %q; want %q", st, got, want)
		}
	}
}

// TestPurchaseStateString_Branches exercises the payments client's state
// stringifier across its enum branches.
func TestPurchaseStateString_Branches(t *testing.T) {
	cases := map[paymentsv1.PurchaseState]string{
		paymentsv1.PurchaseState_PURCHASE_STATE_CHECKOUT_STARTED: "checkout_started",
		paymentsv1.PurchaseState_PURCHASE_STATE_PAYMENT_CAPTURED: "payment_captured",
		paymentsv1.PurchaseState_PURCHASE_STATE_PAYMENT_FAILED:   "payment_failed",
		paymentsv1.PurchaseState_PURCHASE_STATE_REFUNDED:         "refunded",
		paymentsv1.PurchaseState_PURCHASE_STATE_EXPIRED:          "expired",
		paymentsv1.PurchaseState_PURCHASE_STATE_UNSPECIFIED:      "unspecified",
	}
	for st, want := range cases {
		if got := purchaseStateString(st); got != want {
			t.Errorf("purchaseStateString(%v) = %q; want %q", st, got, want)
		}
	}
}
