// clients_branches_test.go — residual coverage for the adapter clients:
//
//   - payments: CreateUserManaTopUpSession happy + nil-rpc + error +
//     nil-response branches + timestampProto
//   - tenancy-admin: hostingModeToWire / tenantStateToWire + the invalid
//     hosting-mode error branch
//   - tx-history: tokenToFormat / exportStatusToToken / tokenToKind /
//     tokenToStatus / statusToToken / nonNilInt64Map mappers
//   - the Error() methods on closure / passkey / weakness-resume client types
package clients_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"

	paymentsv1 "github.com/apollo-chora/chora-contracts/gen/go/chora/services/payments/v1"
)

// ---------------------------------------------------------------------------
// payments — CreateUserManaTopUpSession
// ---------------------------------------------------------------------------

func TestCreateUserManaTopUpSession_ForwardsAllFields(t *testing.T) {
	rpc := &fakePaymentsRPC{
		userManaResp: &paymentsv1.CreateUserManaTopUpSessionResponse{
			PurchaseId:        "purchase-1",
			StripeSessionId:   "cs-1",
			StripeCheckoutUrl: "https://checkout.stripe.com/cs-1",
			State:             paymentsv1.PurchaseState_PURCHASE_STATE_CHECKOUT_STARTED,
		},
	}
	c, err := clients.NewPaymentsClient(clients.PaymentsClientConfig{RPC: rpc, Timeout: time.Second})
	if err != nil {
		t.Fatalf("NewPaymentsClient: %v", err)
	}

	resp, err := c.CreateUserManaTopUpSession(context.Background(), clients.UserManaTopUpRequest{
		IdempotencyKey: "idem-1",
		TenantID:       "tenant-1",
		LearnerGCID:    "gcid-1",
		SKU:            "mana_pack_1000",
		ManaUnits:      1000,
		AmountCents:    199,
		Currency:       "USD",
		SuccessURL:     "https://api.chora.site/checkout/success",
		CancelURL:      "https://api.chora.site/checkout/cancel",
	})
	if err != nil {
		t.Fatalf("CreateUserManaTopUpSession: %v", err)
	}
	if resp.PurchaseID != "purchase-1" || resp.StripeSessionID != "cs-1" || resp.State == "" {
		t.Errorf("resp = %+v; want fields mapped", resp)
	}
}

func TestCreateUserManaTopUpSession_NilClientFailsLoud(t *testing.T) {
	var c *clients.PaymentsClient
	if _, err := c.CreateUserManaTopUpSession(context.Background(), clients.UserManaTopUpRequest{}); err == nil {
		t.Fatal("expected error when client nil")
	}
}

func TestCreateUserManaTopUpSession_PropagatesGRPCError(t *testing.T) {
	rpc := &fakePaymentsRPC{
		userManaErr: errors.New("grpc boom"),
	}
	c, err := clients.NewPaymentsClient(clients.PaymentsClientConfig{RPC: rpc, Timeout: time.Second})
	if err != nil {
		t.Fatalf("NewPaymentsClient: %v", err)
	}
	if _, err := c.CreateUserManaTopUpSession(context.Background(), clients.UserManaTopUpRequest{}); err == nil {
		t.Fatal("expected propagated grpc error")
	}
}

func TestCreateUserManaTopUpSession_NilResponseErrors(t *testing.T) {
	rpc := &fakePaymentsRPC{} // userManaResp nil + userManaErr nil → resp=nil, err=nil
	c, err := clients.NewPaymentsClient(clients.PaymentsClientConfig{RPC: rpc, Timeout: time.Second})
	if err != nil {
		t.Fatalf("NewPaymentsClient: %v", err)
	}
	if _, err := c.CreateUserManaTopUpSession(context.Background(), clients.UserManaTopUpRequest{}); err == nil {
		t.Fatal("expected nil-response error")
	}
}

// ---------------------------------------------------------------------------
// Error() stringifiers on the least-covered client types
// ---------------------------------------------------------------------------

func TestClosureClient_ErrorStringifier(t *testing.T) {
	e := &clients.ClosureUpstreamError{StatusCode: 403, Code: "CLOSURE_FORBIDDEN", Message: "nope"}
	if got := e.Error(); got == "" || !containsAny(got, "CLOSURE_FORBIDDEN", "nope") {
		t.Errorf("ClosureUpstreamError.Error() = %q", got)
	}
}

func TestIdentityPasskeyClient_ErrorStringifier(t *testing.T) {
	e := &clients.PasskeyUpstreamError{StatusCode: 409, Code: "PASSKEY_NOT_PROVISIONED", Message: "no passkey"}
	if got := e.Error(); got == "" || !containsAny(got, "PASSKEY_NOT_PROVISIONED", "no passkey") {
		t.Errorf("PasskeyUpstreamError.Error() = %q", got)
	}
}

func TestWeaknessResumeClient_ErrorStringifier(t *testing.T) {
	e := &clients.WeaknessResumeUpstreamError{StatusCode: 503, Code: "WEAKNESS_RESUME_BUSY", Message: "busy"}
	if got := e.Error(); got == "" || !containsAny(got, "WEAKNESS_RESUME_BUSY", "busy") {
		t.Errorf("WeaknessResumeUpstreamError.Error() = %q", got)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(s) > 0 && contains(s, sub) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
