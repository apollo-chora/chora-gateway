// Package clients owns chora-gateway's outbound client adapters for internal
// gRPC + HTTP services.
//
// payments_client.go — gRPC client for chora-payments PaymentService
// (chora-contracts/proto/services/payments/v1/payments.proto), per
// ADR-164 PROPOSED 2026-05-24.
//
// Stage B scope (`/api/v1/checkout/*` REST proxy) — chora-gateway exposes a
// thin REST surface that proxies each of the 5 Purchase aggregates' Create*
// RPCs to chora-payments. The HTTP handler (payments_handler.go) injects
// the validated ChoraSession mesh claims (tenant_id + learner_gcid) into the
// outbound gRPC request and translates gRPC status codes back into HTTP.
//
// Trust model: this client is mesh-internal (chora-gateway → chora-payments
// across Cloud Service Mesh). Cloud Service Mesh handles mTLS at L4 so the
// gRPC client speaks plain HTTP/2 to its sidecar — `insecure.NewCredentials()`
// is the canonical chora pattern (mirrors chora-delivery's
// `clients/question_client.go` + chora-identity/main.go gRPC dial against
// SVC_CREATION_GRPC_URL / chora-tenancy mana).
//
// Per `feedback_no_inline_config`: address comes from env
// (`CHORA_PAYMENTS_GRPC_ADDR`); the constructor fails-loud on empty input.
//
// Per `feedback_no_stubs_real_wiring`: there is NO in-process stub fallback
// or fake mode. Missing env → service refuses to start (boot env gate). gRPC
// dial failure → wraps + returns the error to the caller; HTTP handler maps
// to a 5xx.
package clients

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	paymentsv1 "github.com/apollo-chora/chora-contracts/gen/go/chora/services/payments/v1"
)

// Default per-call timeout. The Stripe Checkout Session mint involves an
// outbound HTTPS call from chora-payments to Stripe Checkout (canonical p95
// ~ 300-600 ms; p99 occasionally 1-2 s). 8 s allows headroom while keeping
// the gateway-side WriteTimeout (45 s) comfortable.
const defaultPaymentsCallTimeout = 8 * time.Second

// PaymentServiceGRPCClient is the minimal slice of
// `paymentsv1.PaymentServiceClient` that chora-gateway calls. Tests inject
// a fake; production wires the real `paymentsv1.NewPaymentServiceClient(conn)`
// against `CHORA_PAYMENTS_GRPC_ADDR`.
type PaymentServiceGRPCClient interface {
	CreateCourseCheckoutSession(ctx context.Context, in *paymentsv1.CreateCourseCheckoutSessionRequest, opts ...grpc.CallOption) (*paymentsv1.CreateCourseCheckoutSessionResponse, error)
	CreateApplicationCheckoutSession(ctx context.Context, in *paymentsv1.CreateApplicationCheckoutSessionRequest, opts ...grpc.CallOption) (*paymentsv1.CreateApplicationCheckoutSessionResponse, error)
	CreateCompanionEggCheckoutSession(ctx context.Context, in *paymentsv1.CreateCompanionEggCheckoutSessionRequest, opts ...grpc.CallOption) (*paymentsv1.CreateCompanionEggCheckoutSessionResponse, error)
	CreateManaTopUpSession(ctx context.Context, in *paymentsv1.CreateManaTopUpSessionRequest, opts ...grpc.CallOption) (*paymentsv1.CreateManaTopUpSessionResponse, error)
	CreateUserManaTopUpSession(ctx context.Context, in *paymentsv1.CreateUserManaTopUpSessionRequest, opts ...grpc.CallOption) (*paymentsv1.CreateUserManaTopUpSessionResponse, error)
	CreateSubscription(ctx context.Context, in *paymentsv1.CreateSubscriptionRequest, opts ...grpc.CallOption) (*paymentsv1.CreateSubscriptionResponse, error)
	// CHO-1736 — 8th aggregate, H+ Marketplace Subscribe.
	CreateTenantAddonCheckoutSession(ctx context.Context, in *paymentsv1.CreateTenantAddonCheckoutSessionRequest, opts ...grpc.CallOption) (*paymentsv1.CreateTenantAddonCheckoutSessionResponse, error)
}

// PaymentsClient adapts the chora-payments PaymentService gRPC to the
// HTTP-handler-facing API used by `payments_handler.go`.
type PaymentsClient struct {
	rpc     PaymentServiceGRPCClient
	timeout time.Duration
}

// PaymentsClientConfig is the constructor input.
type PaymentsClientConfig struct {
	// RPC is the gRPC client (production: `paymentsv1.NewPaymentServiceClient(conn)`).
	// REQUIRED — constructor fails-loud on nil.
	RPC PaymentServiceGRPCClient

	// Timeout is the per-RPC deadline. Defaults to 8 s.
	Timeout time.Duration
}

// NewPaymentsClient constructs a PaymentsClient. Returns an error when RPC
// is nil — boot must fail loud per `feedback_no_stubs_real_wiring`.
func NewPaymentsClient(cfg PaymentsClientConfig) (*PaymentsClient, error) {
	if cfg.RPC == nil {
		return nil, errors.New("clients.payments: RPC client required")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultPaymentsCallTimeout
	}
	return &PaymentsClient{rpc: cfg.RPC, timeout: timeout}, nil
}

// CheckoutResponse is the normalised reply shape the HTTP handler emits to
// chora-web. All 5 Create* RPCs converge on this shape — the HTTP handler
// returns the same JSON envelope regardless of which aggregate is being
// checked out.
//
// The `State` field is the canonical lowercase enum name (e.g.
// "checkout_started") — the proto enum is converted to snake-case via
// `purchaseStateString` for FE consumption.
type CheckoutResponse struct {
	PurchaseID        string `json:"purchase_id"`
	StripeSessionID   string `json:"stripe_session_id"`
	StripeCheckoutURL string `json:"stripe_checkout_url"`
	State             string `json:"state"`
}

// CourseCheckoutRequest is the validated input for CreateCourseCheckoutSession.
type CourseCheckoutRequest struct {
	IdempotencyKey string
	TenantID       string
	LearnerGCID    string
	CourseID       string
	AmountCents    int64
	Currency       string
	SuccessURL     string
	CancelURL      string
}

// ApplicationCheckoutRequest is the validated input for CreateApplicationCheckoutSession.
type ApplicationCheckoutRequest struct {
	IdempotencyKey string
	TenantID       string
	LearnerGCID    string
	ApplicationID  string
	CourseID       string
	AmountCents    int64
	Currency       string
	SuccessURL     string
	CancelURL      string
}

// CompanionEggCheckoutRequest is the validated input for CreateCompanionEggCheckoutSession.
type CompanionEggCheckoutRequest struct {
	IdempotencyKey       string
	TenantID             string
	LearnerGCID          string
	EggSKU               string
	SuggestedFocalAtomID string
	AmountCents          int64
	Currency             string
	SoftExpiryAt         time.Time
	HardExpiryAt         time.Time
	SuccessURL           string
	CancelURL            string
}

// ManaTopUpRequest is the validated input for CreateManaTopUpSession.
type ManaTopUpRequest struct {
	IdempotencyKey string
	TenantID       string
	AdminGCID      string
	SKU            string
	ManaUnits      int64
	AmountCents    int64
	Currency       string
	SuccessURL     string
	CancelURL      string
}

// UserManaTopUpRequest is the validated input for CreateUserManaTopUpSession
// (per-USER wallet top-up; distinct from ManaTopUpRequest which is the TENANT
// pool top-up keyed on AdminGCID). LearnerGCID is the buyer whose user_mana
// wallet is credited on payment capture.
type UserManaTopUpRequest struct {
	IdempotencyKey string
	TenantID       string
	LearnerGCID    string
	SKU            string
	ManaUnits      int64
	AmountCents    int64
	Currency       string
	SuccessURL     string
	CancelURL      string
}

// SubscriptionRequest is the validated input for CreateSubscription.
type SubscriptionRequest struct {
	IdempotencyKey string
	TenantID       string
	LearnerGCID    string
	PlanSKU        string
	BillingPeriod  string // "monthly" | "annually"
	AmountCents    int64
	Currency       string
	SuccessURL     string
	CancelURL      string
}

// TenantAddonCheckoutRequest is the validated input for
// CreateTenantAddonCheckoutSession (CHO-1736 H+ Marketplace Subscribe).
// Tenant-scoped purchase initiated by a tenant admin.
type TenantAddonCheckoutRequest struct {
	IdempotencyKey string
	TenantID       string
	AdminGCID      string
	AddonPlanID    string
	AddonCode      string
	TierCode       string
	AmountCents    int64
	Currency       string
	SuccessURL     string
	CancelURL      string
}

// CreateCourseCheckoutSession invokes the gRPC RPC and normalises the reply.
func (c *PaymentsClient) CreateCourseCheckoutSession(ctx context.Context, req CourseCheckoutRequest) (CheckoutResponse, error) {
	if c == nil || c.rpc == nil {
		return CheckoutResponse{}, errors.New("payments client: rpc client nil")
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, err := c.rpc.CreateCourseCheckoutSession(callCtx, &paymentsv1.CreateCourseCheckoutSessionRequest{
		IdempotencyKey: req.IdempotencyKey,
		TenantId:       req.TenantID,
		LearnerGcid:    req.LearnerGCID,
		CourseId:       req.CourseID,
		AmountCents:    req.AmountCents,
		Currency:       req.Currency,
		SuccessUrl:     req.SuccessURL,
		CancelUrl:      req.CancelURL,
	})
	if err != nil {
		return CheckoutResponse{}, fmt.Errorf("payments client: CreateCourseCheckoutSession: %w", err)
	}
	if resp == nil {
		return CheckoutResponse{}, errors.New("payments client: nil response")
	}
	return CheckoutResponse{
		PurchaseID:        resp.GetPurchaseId(),
		StripeSessionID:   resp.GetStripeSessionId(),
		StripeCheckoutURL: resp.GetStripeCheckoutUrl(),
		State:             purchaseStateString(resp.GetState()),
	}, nil
}

// CreateApplicationCheckoutSession invokes the gRPC RPC.
func (c *PaymentsClient) CreateApplicationCheckoutSession(ctx context.Context, req ApplicationCheckoutRequest) (CheckoutResponse, error) {
	if c == nil || c.rpc == nil {
		return CheckoutResponse{}, errors.New("payments client: rpc client nil")
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, err := c.rpc.CreateApplicationCheckoutSession(callCtx, &paymentsv1.CreateApplicationCheckoutSessionRequest{
		IdempotencyKey: req.IdempotencyKey,
		TenantId:       req.TenantID,
		LearnerGcid:    req.LearnerGCID,
		ApplicationId:  req.ApplicationID,
		CourseId:       req.CourseID,
		AmountCents:    req.AmountCents,
		Currency:       req.Currency,
		SuccessUrl:     req.SuccessURL,
		CancelUrl:      req.CancelURL,
	})
	if err != nil {
		return CheckoutResponse{}, fmt.Errorf("payments client: CreateApplicationCheckoutSession: %w", err)
	}
	if resp == nil {
		return CheckoutResponse{}, errors.New("payments client: nil response")
	}
	return CheckoutResponse{
		PurchaseID:        resp.GetPurchaseId(),
		StripeSessionID:   resp.GetStripeSessionId(),
		StripeCheckoutURL: resp.GetStripeCheckoutUrl(),
		State:             purchaseStateString(resp.GetState()),
	}, nil
}

// CreateCompanionEggCheckoutSession invokes the gRPC RPC.
func (c *PaymentsClient) CreateCompanionEggCheckoutSession(ctx context.Context, req CompanionEggCheckoutRequest) (CheckoutResponse, error) {
	if c == nil || c.rpc == nil {
		return CheckoutResponse{}, errors.New("payments client: rpc client nil")
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	pbReq := &paymentsv1.CreateCompanionEggCheckoutSessionRequest{
		IdempotencyKey:       req.IdempotencyKey,
		TenantId:             req.TenantID,
		LearnerGcid:          req.LearnerGCID,
		EggSku:               req.EggSKU,
		SuggestedFocalAtomId: req.SuggestedFocalAtomID,
		AmountCents:          req.AmountCents,
		Currency:             req.Currency,
		SuccessUrl:           req.SuccessURL,
		CancelUrl:            req.CancelURL,
	}
	if !req.SoftExpiryAt.IsZero() {
		pbReq.SoftExpiryAt = timestampProto(req.SoftExpiryAt)
	}
	if !req.HardExpiryAt.IsZero() {
		pbReq.HardExpiryAt = timestampProto(req.HardExpiryAt)
	}
	resp, err := c.rpc.CreateCompanionEggCheckoutSession(callCtx, pbReq)
	if err != nil {
		return CheckoutResponse{}, fmt.Errorf("payments client: CreateCompanionEggCheckoutSession: %w", err)
	}
	if resp == nil {
		return CheckoutResponse{}, errors.New("payments client: nil response")
	}
	return CheckoutResponse{
		PurchaseID:        resp.GetPurchaseId(),
		StripeSessionID:   resp.GetStripeSessionId(),
		StripeCheckoutURL: resp.GetStripeCheckoutUrl(),
		State:             purchaseStateString(resp.GetState()),
	}, nil
}

// CreateManaTopUpSession invokes the gRPC RPC.
func (c *PaymentsClient) CreateManaTopUpSession(ctx context.Context, req ManaTopUpRequest) (CheckoutResponse, error) {
	if c == nil || c.rpc == nil {
		return CheckoutResponse{}, errors.New("payments client: rpc client nil")
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, err := c.rpc.CreateManaTopUpSession(callCtx, &paymentsv1.CreateManaTopUpSessionRequest{
		IdempotencyKey: req.IdempotencyKey,
		TenantId:       req.TenantID,
		AdminGcid:      req.AdminGCID,
		Sku:            req.SKU,
		ManaUnits:      req.ManaUnits,
		AmountCents:    req.AmountCents,
		Currency:       req.Currency,
		SuccessUrl:     req.SuccessURL,
		CancelUrl:      req.CancelURL,
	})
	if err != nil {
		return CheckoutResponse{}, fmt.Errorf("payments client: CreateManaTopUpSession: %w", err)
	}
	if resp == nil {
		return CheckoutResponse{}, errors.New("payments client: nil response")
	}
	return CheckoutResponse{
		PurchaseID:        resp.GetPurchaseId(),
		StripeSessionID:   resp.GetStripeSessionId(),
		StripeCheckoutURL: resp.GetStripeCheckoutUrl(),
		State:             purchaseStateString(resp.GetState()),
	}, nil
}

// CreateUserManaTopUpSession invokes the gRPC RPC for the per-USER wallet
// top-up (distinct from CreateManaTopUpSession, which is the TENANT pool).
func (c *PaymentsClient) CreateUserManaTopUpSession(ctx context.Context, req UserManaTopUpRequest) (CheckoutResponse, error) {
	if c == nil || c.rpc == nil {
		return CheckoutResponse{}, errors.New("payments client: rpc client nil")
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, err := c.rpc.CreateUserManaTopUpSession(callCtx, &paymentsv1.CreateUserManaTopUpSessionRequest{
		IdempotencyKey: req.IdempotencyKey,
		TenantId:       req.TenantID,
		LearnerGcid:    req.LearnerGCID,
		Sku:            req.SKU,
		ManaUnits:      req.ManaUnits,
		AmountCents:    req.AmountCents,
		Currency:       req.Currency,
		SuccessUrl:     req.SuccessURL,
		CancelUrl:      req.CancelURL,
	})
	if err != nil {
		return CheckoutResponse{}, fmt.Errorf("payments client: CreateUserManaTopUpSession: %w", err)
	}
	if resp == nil {
		return CheckoutResponse{}, errors.New("payments client: nil response")
	}
	return CheckoutResponse{
		PurchaseID:        resp.GetPurchaseId(),
		StripeSessionID:   resp.GetStripeSessionId(),
		StripeCheckoutURL: resp.GetStripeCheckoutUrl(),
		State:             purchaseStateString(resp.GetState()),
	}, nil
}

// CreateSubscription invokes the gRPC RPC.
func (c *PaymentsClient) CreateSubscription(ctx context.Context, req SubscriptionRequest) (CheckoutResponse, error) {
	if c == nil || c.rpc == nil {
		return CheckoutResponse{}, errors.New("payments client: rpc client nil")
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, err := c.rpc.CreateSubscription(callCtx, &paymentsv1.CreateSubscriptionRequest{
		IdempotencyKey: req.IdempotencyKey,
		TenantId:       req.TenantID,
		LearnerGcid:    req.LearnerGCID,
		PlanSku:        req.PlanSKU,
		BillingPeriod:  billingPeriodFromString(req.BillingPeriod),
		AmountCents:    req.AmountCents,
		Currency:       req.Currency,
		SuccessUrl:     req.SuccessURL,
		CancelUrl:      req.CancelURL,
	})
	if err != nil {
		return CheckoutResponse{}, fmt.Errorf("payments client: CreateSubscription: %w", err)
	}
	if resp == nil {
		return CheckoutResponse{}, errors.New("payments client: nil response")
	}
	return CheckoutResponse{
		PurchaseID:        resp.GetPurchaseId(),
		StripeSessionID:   resp.GetStripeSessionId(),
		StripeCheckoutURL: resp.GetStripeCheckoutUrl(),
		State:             purchaseStateString(resp.GetState()),
	}, nil
}

// purchaseStateString converts a proto PurchaseState enum to the canonical
// snake_case JSON value the FE expects. The proto enum names are
// `PURCHASE_STATE_CHECKOUT_STARTED` etc.; we strip the prefix + lower-case.
func purchaseStateString(s paymentsv1.PurchaseState) string {
	switch s {
	case paymentsv1.PurchaseState_PURCHASE_STATE_CHECKOUT_STARTED:
		return "checkout_started"
	case paymentsv1.PurchaseState_PURCHASE_STATE_PAYMENT_CAPTURED:
		return "payment_captured"
	case paymentsv1.PurchaseState_PURCHASE_STATE_PAYMENT_FAILED:
		return "payment_failed"
	case paymentsv1.PurchaseState_PURCHASE_STATE_REFUNDED:
		return "refunded"
	case paymentsv1.PurchaseState_PURCHASE_STATE_EXPIRED:
		return "expired"
	default:
		return "unspecified"
	}
}

// timestampProto converts a time.Time to *timestamppb.Timestamp. Returns nil
// when t is zero so unset fields skip the wire (proto3 nullable semantics).
func timestampProto(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t.UTC())
}

// billingPeriodFromString maps the FE's lowercase string to the proto enum.
// "monthly" / "month" → MONTHLY; "annually" / "annual" / "yearly" / "year" →
// ANNUALLY; anything else (including empty) → UNSPECIFIED, which the
// downstream rejects with InvalidArgument.
func billingPeriodFromString(s string) paymentsv1.BillingPeriod {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "monthly", "month":
		return paymentsv1.BillingPeriod_BILLING_PERIOD_MONTHLY
	case "annually", "annual", "yearly", "year":
		return paymentsv1.BillingPeriod_BILLING_PERIOD_ANNUALLY
	default:
		return paymentsv1.BillingPeriod_BILLING_PERIOD_UNSPECIFIED
	}
}

// CreateTenantAddonCheckoutSession invokes the gRPC RPC. CHO-1736 — 8th
// aggregate, tenant-admin-initiated H+ Marketplace add-on subscription.
func (c *PaymentsClient) CreateTenantAddonCheckoutSession(ctx context.Context, req TenantAddonCheckoutRequest) (CheckoutResponse, error) {
	if c == nil || c.rpc == nil {
		return CheckoutResponse{}, errors.New("payments client: rpc client nil")
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, err := c.rpc.CreateTenantAddonCheckoutSession(callCtx, &paymentsv1.CreateTenantAddonCheckoutSessionRequest{
		IdempotencyKey: req.IdempotencyKey,
		TenantId:       req.TenantID,
		AdminGcid:      req.AdminGCID,
		AddonPlanId:    req.AddonPlanID,
		AddonCode:      req.AddonCode,
		TierCode:       req.TierCode,
		AmountCents:    req.AmountCents,
		Currency:       req.Currency,
		SuccessUrl:     req.SuccessURL,
		CancelUrl:      req.CancelURL,
	})
	if err != nil {
		return CheckoutResponse{}, fmt.Errorf("payments client: CreateTenantAddonCheckoutSession: %w", err)
	}
	if resp == nil {
		return CheckoutResponse{}, errors.New("payments client: nil response")
	}
	return CheckoutResponse{
		PurchaseID:        resp.GetPurchaseId(),
		StripeSessionID:   resp.GetStripeSessionId(),
		StripeCheckoutURL: resp.GetStripeCheckoutUrl(),
		State:             purchaseStateString(resp.GetState()),
	}, nil
}
