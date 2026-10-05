// payments_legacy_egg_method.go: the W4-to-W5 overlap shim for the Companion
// egg checkout, and NOTHING else.
//
// # Why this exists
//
// ADR-254 D9 renamed the payments RPC CreateFamiliarEggCheckoutSession to
// CreateCompanionEggCheckoutSession. The two services roll in DIFFERENT
// windows: chora-gateway at W4, chora-payments at W5. Between those rolls the
// live PaymentService still serves the Familiar-spelled method, and the
// payments Istio allowlist (chora-infra/k8s/services/chora-payments/
// authz-allow-mesh.yaml) admits only that path, so a gateway calling the new
// name would get UNIMPLEMENTED or an RBAC denial and every A+ egg checkout
// would fail for the whole window.
//
// The rename is wire-safe in the direction that matters: the request and
// response FIELD NUMBERS did not move, so the Companion-typed message
// marshals byte-identically for the Familiar-named method. Only the method
// STRING has to stay behind, which is why this shim overrides one method and
// delegates every other RPC to the generated client untouched.
//
// # Deleting it (W5, not later)
//
// This file, its test, and the loader's call to
// NewPaymentServiceClientWithLegacyEggMethod go out in the W5 payments commit,
// replaced by paymentsv1.NewPaymentServiceClient(conn).
//
// ⚠ W5 SEQUENCING, because this shim only covers ONE side: when chora-payments
// rolls with the renamed method, every caller still on the old path breaks.
// chora-tenancy is such a caller (internal/adapter/payments/grpc_client.go).
// So the W5 payments roll needs a legacy method alias on the SERVER, the same
// pattern chora-consumption used for FamiliarGrowth, plus both paths in the
// payments allowlist, dropped one roll later. Flipping this shim without that
// alias just moves the outage from the gateway to tenancy.
package clients

import (
	"context"

	"google.golang.org/grpc"

	paymentsv1 "github.com/apollo-chora/chora-contracts/gen/go/chora/services/payments/v1"
)

// LegacyCompanionEggMethod is the fully-qualified method chora-payments serves
// until its W5 roll. Exported so the loader can log which path it dialled and
// so the overlap contract is pinned by a test rather than by a comment.
const LegacyCompanionEggMethod = "/chora.services.payments.v1.PaymentService/CreateFamiliarEggCheckoutSession"

// legacyEggMethodClient is the generated PaymentService client with exactly one
// method re-pointed at the pre-rename path.
//
// Embedding the generated client rather than reimplementing the interface is
// deliberate: an RPC added to PaymentService later is carried automatically and
// keeps its own generated path, so this shim can never become a stale, partial
// copy of the contract.
type legacyEggMethodClient struct {
	paymentsv1.PaymentServiceClient
	cc grpc.ClientConnInterface
}

// NewPaymentServiceClientWithLegacyEggMethod wraps cc in the generated client
// and re-points the Companion egg checkout at LegacyCompanionEggMethod.
func NewPaymentServiceClientWithLegacyEggMethod(cc grpc.ClientConnInterface) PaymentServiceGRPCClient {
	return &legacyEggMethodClient{
		PaymentServiceClient: paymentsv1.NewPaymentServiceClient(cc),
		cc:                   cc,
	}
}

// CreateCompanionEggCheckoutSession sends the Companion-typed request to the
// Familiar-named method. No field copying: the message goes through as it
// arrived, so there is no second definition of the request to drift.
//
// There is no probe-and-fallback here on purpose. Trying the new method first
// and falling back on UNIMPLEMENTED would add a failed round trip to every
// checkout and, worse, would hide which side of the rename the estate is on:
// after W5 the fallback would silently keep working and nobody would learn the
// shim was still in the binary.
func (c *legacyEggMethodClient) CreateCompanionEggCheckoutSession(
	ctx context.Context,
	in *paymentsv1.CreateCompanionEggCheckoutSessionRequest,
	opts ...grpc.CallOption,
) (*paymentsv1.CreateCompanionEggCheckoutSessionResponse, error) {
	out := new(paymentsv1.CreateCompanionEggCheckoutSessionResponse)
	if err := c.cc.Invoke(ctx, LegacyCompanionEggMethod, in, out, opts...); err != nil {
		return nil, err
	}
	return out, nil
}
