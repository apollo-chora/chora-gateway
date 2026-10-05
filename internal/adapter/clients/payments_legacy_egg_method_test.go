// payments_legacy_egg_method_test.go: pins the W4-to-W5 overlap shim.
//
// chora-gateway rolls at W4 built against the RENAMED contracts, which only
// declare CreateCompanionEggCheckoutSession. chora-payments rolls at W5, so
// through the whole overlap the live PaymentService serves
// CreateFamiliarEggCheckoutSession and its Istio allowlist admits only that
// path. Calling the new name would be UNIMPLEMENTED (or mesh-denied) and the
// A+ egg checkout would 502 for every learner in the window.
//
// The method string is therefore an OVERLAP CONTRACT, not an implementation
// detail, and it is pinned here so a well-meant "finish the rename" cannot
// quietly break checkout between the two rolls.
package clients

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"

	paymentsv1 "github.com/apollo-chora/chora-contracts/gen/go/chora/services/payments/v1"
)

// recordingConn is a grpc.ClientConnInterface that records the method string
// and the request it was handed.
type recordingConn struct {
	method string
	args   any
	err    error
}

func (c *recordingConn) Invoke(_ context.Context, method string, args, _ any, _ ...grpc.CallOption) error {
	c.method = method
	c.args = args
	return c.err
}

func (c *recordingConn) NewStream(_ context.Context, _ *grpc.StreamDesc, _ string, _ ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, errors.New("streaming not used by PaymentService")
}

func TestLegacyEggMethod_IsTheFamiliarSpelledPath(t *testing.T) {
	const want = "/chora.services.payments.v1.PaymentService/CreateFamiliarEggCheckoutSession"
	if LegacyCompanionEggMethod != want {
		t.Fatalf("LegacyCompanionEggMethod = %q; want %q (the method chora-payments serves until its W5 roll)",
			LegacyCompanionEggMethod, want)
	}
}

func TestPaymentServiceClientWithLegacyEggMethod_DialsTheLegacyPath(t *testing.T) {
	conn := &recordingConn{}
	rpc := NewPaymentServiceClientWithLegacyEggMethod(conn)

	req := &paymentsv1.CreateCompanionEggCheckoutSessionRequest{
		IdempotencyKey: "idem-1",
		TenantId:       "01957c8c-2222-7000-aaaa-222222222222",
		LearnerGcid:    "01957c8c-3333-7000-aaaa-333333333333",
		EggSku:         "egg_standard_v1",
		AmountCents:    1900,
		Currency:       "SGD",
	}
	if _, err := rpc.CreateCompanionEggCheckoutSession(context.Background(), req); err != nil {
		t.Fatalf("CreateCompanionEggCheckoutSession: %v", err)
	}

	if conn.method != LegacyCompanionEggMethod {
		t.Errorf("invoked %q; want the legacy %q (chora-payments does not serve the Companion name until W5)",
			conn.method, LegacyCompanionEggMethod)
	}
	// Field numbers survived the rename, so the Companion-typed request
	// marshals byte-identically for the Familiar-named method: no translation
	// struct, no field-by-field copy that could drift.
	if conn.args != any(req) {
		t.Errorf("the request was rewritten before the call; want the caller's message passed straight through")
	}
}

func TestPaymentServiceClientWithLegacyEggMethod_LeavesEveryOtherRPCAlone(t *testing.T) {
	conn := &recordingConn{}
	rpc := NewPaymentServiceClientWithLegacyEggMethod(conn)

	if _, err := rpc.CreateCourseCheckoutSession(context.Background(),
		&paymentsv1.CreateCourseCheckoutSessionRequest{IdempotencyKey: "idem-2"}); err != nil {
		t.Fatalf("CreateCourseCheckoutSession: %v", err)
	}
	const want = "/chora.services.payments.v1.PaymentService/CreateCourseCheckoutSession"
	if conn.method != want {
		t.Errorf("CreateCourseCheckoutSession invoked %q; want %q (the shim must touch the egg RPC only)",
			conn.method, want)
	}
}

func TestPaymentServiceClientWithLegacyEggMethod_PropagatesTheError(t *testing.T) {
	conn := &recordingConn{err: errors.New("mesh denied")}
	rpc := NewPaymentServiceClientWithLegacyEggMethod(conn)

	if _, err := rpc.CreateCompanionEggCheckoutSession(context.Background(),
		&paymentsv1.CreateCompanionEggCheckoutSessionRequest{IdempotencyKey: "idem-3"}); err == nil {
		t.Fatal("a transport error was swallowed; the checkout must fail loud")
	}
}

// The shim must satisfy the interface the PaymentsClient consumes, or the
// loader would silently keep the typed client.
var _ PaymentServiceGRPCClient = NewPaymentServiceClientWithLegacyEggMethod(&recordingConn{})
