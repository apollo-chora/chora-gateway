// payments_client_test.go — TDD specs for the chora-gateway → chora-payments
// gRPC client wrapper. Per ADR-164 Stage B.
//
// Strict TDD: tests assert the wire-level proto translation, gRPC error
// propagation, idempotency-key forwarding, and the snake_case state mapping.
package clients_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	paymentsv1 "github.com/apollo-chora/chora-contracts/gen/go/chora/services/payments/v1"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
)

// fakePaymentsRPC records the most recent request per RPC + returns a
// configurable canned response. One fake per test keeps the assertions
// isolated.
type fakePaymentsRPC struct {
	courseReq        *paymentsv1.CreateCourseCheckoutSessionRequest
	courseResp       *paymentsv1.CreateCourseCheckoutSessionResponse
	courseErr        error
	applicationReq   *paymentsv1.CreateApplicationCheckoutSessionRequest
	applicationResp  *paymentsv1.CreateApplicationCheckoutSessionResponse
	applicationErr   error
	companionEggReq  *paymentsv1.CreateCompanionEggCheckoutSessionRequest
	companionEggResp *paymentsv1.CreateCompanionEggCheckoutSessionResponse
	companionEggErr  error
	manaTopUpReq     *paymentsv1.CreateManaTopUpSessionRequest
	manaTopUpResp    *paymentsv1.CreateManaTopUpSessionResponse
	manaTopUpErr     error
	userManaReq      *paymentsv1.CreateUserManaTopUpSessionRequest
	userManaResp     *paymentsv1.CreateUserManaTopUpSessionResponse
	userManaErr      error
	subscriptionReq  *paymentsv1.CreateSubscriptionRequest
	subscriptionResp *paymentsv1.CreateSubscriptionResponse
	subscriptionErr  error
	tenantAddonReq   *paymentsv1.CreateTenantAddonCheckoutSessionRequest
	tenantAddonResp  *paymentsv1.CreateTenantAddonCheckoutSessionResponse
	tenantAddonErr   error
}

func (f *fakePaymentsRPC) CreateCourseCheckoutSession(_ context.Context, in *paymentsv1.CreateCourseCheckoutSessionRequest, _ ...grpc.CallOption) (*paymentsv1.CreateCourseCheckoutSessionResponse, error) {
	f.courseReq = in
	return f.courseResp, f.courseErr
}

func (f *fakePaymentsRPC) CreateApplicationCheckoutSession(_ context.Context, in *paymentsv1.CreateApplicationCheckoutSessionRequest, _ ...grpc.CallOption) (*paymentsv1.CreateApplicationCheckoutSessionResponse, error) {
	f.applicationReq = in
	return f.applicationResp, f.applicationErr
}

func (f *fakePaymentsRPC) CreateCompanionEggCheckoutSession(_ context.Context, in *paymentsv1.CreateCompanionEggCheckoutSessionRequest, _ ...grpc.CallOption) (*paymentsv1.CreateCompanionEggCheckoutSessionResponse, error) {
	f.companionEggReq = in
	return f.companionEggResp, f.companionEggErr
}

func (f *fakePaymentsRPC) CreateManaTopUpSession(_ context.Context, in *paymentsv1.CreateManaTopUpSessionRequest, _ ...grpc.CallOption) (*paymentsv1.CreateManaTopUpSessionResponse, error) {
	f.manaTopUpReq = in
	return f.manaTopUpResp, f.manaTopUpErr
}

func (f *fakePaymentsRPC) CreateUserManaTopUpSession(_ context.Context, in *paymentsv1.CreateUserManaTopUpSessionRequest, _ ...grpc.CallOption) (*paymentsv1.CreateUserManaTopUpSessionResponse, error) {
	f.userManaReq = in
	return f.userManaResp, f.userManaErr
}

func (f *fakePaymentsRPC) CreateSubscription(_ context.Context, in *paymentsv1.CreateSubscriptionRequest, _ ...grpc.CallOption) (*paymentsv1.CreateSubscriptionResponse, error) {
	f.subscriptionReq = in
	return f.subscriptionResp, f.subscriptionErr
}

func (f *fakePaymentsRPC) CreateTenantAddonCheckoutSession(_ context.Context, in *paymentsv1.CreateTenantAddonCheckoutSessionRequest, _ ...grpc.CallOption) (*paymentsv1.CreateTenantAddonCheckoutSessionResponse, error) {
	f.tenantAddonReq = in
	return f.tenantAddonResp, f.tenantAddonErr
}

// --- Constructor ------------------------------------------------------------

func TestNewPaymentsClient_RequiresRPC(t *testing.T) {
	t.Parallel()
	_, err := clients.NewPaymentsClient(clients.PaymentsClientConfig{})
	if err == nil {
		t.Fatal("expected error when RPC is nil")
	}
}

func TestNewPaymentsClient_AcceptsValidRPC(t *testing.T) {
	t.Parallel()
	c, err := clients.NewPaymentsClient(clients.PaymentsClientConfig{RPC: &fakePaymentsRPC{}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

// --- CreateCourseCheckoutSession --------------------------------------------

func TestCreateCourseCheckoutSession_ForwardsAllFields(t *testing.T) {
	t.Parallel()
	rpc := &fakePaymentsRPC{
		courseResp: &paymentsv1.CreateCourseCheckoutSessionResponse{
			PurchaseId:        "p-1",
			StripeSessionId:   "cs_test_1",
			StripeCheckoutUrl: "https://checkout.stripe.com/c/pay/cs_test_1",
			State:             paymentsv1.PurchaseState_PURCHASE_STATE_CHECKOUT_STARTED,
		},
	}
	c, _ := clients.NewPaymentsClient(clients.PaymentsClientConfig{RPC: rpc})

	got, err := c.CreateCourseCheckoutSession(context.Background(), clients.CourseCheckoutRequest{
		IdempotencyKey: "idem-1",
		TenantID:       "tenant-1",
		LearnerGCID:    "gcid-1",
		CourseID:       "course-1",
		AmountCents:    49900,
		Currency:       "SGD",
		SuccessURL:     "https://chora.site/c/courses/{COURSE_ID}/success",
		CancelURL:      "https://chora.site/c/courses/{COURSE_ID}",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rpc.courseReq == nil {
		t.Fatal("RPC was not invoked")
	}
	if rpc.courseReq.GetIdempotencyKey() != "idem-1" {
		t.Errorf("IdempotencyKey = %q, want idem-1", rpc.courseReq.GetIdempotencyKey())
	}
	if rpc.courseReq.GetTenantId() != "tenant-1" {
		t.Errorf("TenantId = %q, want tenant-1", rpc.courseReq.GetTenantId())
	}
	if rpc.courseReq.GetLearnerGcid() != "gcid-1" {
		t.Errorf("LearnerGcid = %q, want gcid-1", rpc.courseReq.GetLearnerGcid())
	}
	if rpc.courseReq.GetCourseId() != "course-1" {
		t.Errorf("CourseId = %q, want course-1", rpc.courseReq.GetCourseId())
	}
	if rpc.courseReq.GetAmountCents() != 49900 {
		t.Errorf("AmountCents = %d, want 49900", rpc.courseReq.GetAmountCents())
	}
	if rpc.courseReq.GetCurrency() != "SGD" {
		t.Errorf("Currency = %q, want SGD", rpc.courseReq.GetCurrency())
	}
	if got.PurchaseID != "p-1" {
		t.Errorf("PurchaseID = %q, want p-1", got.PurchaseID)
	}
	if got.StripeCheckoutURL != "https://checkout.stripe.com/c/pay/cs_test_1" {
		t.Errorf("StripeCheckoutURL = %q", got.StripeCheckoutURL)
	}
	if got.State != "checkout_started" {
		t.Errorf("State = %q, want checkout_started", got.State)
	}
}

func TestCreateCourseCheckoutSession_PropagatesGRPCError(t *testing.T) {
	t.Parallel()
	rpc := &fakePaymentsRPC{
		courseErr: status.Error(codes.InvalidArgument, "missing course_id"),
	}
	c, _ := clients.NewPaymentsClient(clients.PaymentsClientConfig{RPC: rpc})

	_, err := c.CreateCourseCheckoutSession(context.Background(), clients.CourseCheckoutRequest{
		IdempotencyKey: "idem-2",
		TenantID:       "tenant-1",
		LearnerGCID:    "gcid-1",
		CourseID:       "",
		AmountCents:    49900,
		Currency:       "SGD",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	st, ok := status.FromError(errors.Unwrap(err))
	if !ok {
		t.Fatalf("expected wrapped gRPC status; got %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", st.Code())
	}
}

// --- CreateApplicationCheckoutSession ---------------------------------------

func TestCreateApplicationCheckoutSession_ForwardsAllFields(t *testing.T) {
	t.Parallel()
	rpc := &fakePaymentsRPC{
		applicationResp: &paymentsv1.CreateApplicationCheckoutSessionResponse{
			PurchaseId:        "p-2",
			StripeSessionId:   "cs_test_2",
			StripeCheckoutUrl: "https://checkout.stripe.com/c/pay/cs_test_2",
			State:             paymentsv1.PurchaseState_PURCHASE_STATE_CHECKOUT_STARTED,
		},
	}
	c, _ := clients.NewPaymentsClient(clients.PaymentsClientConfig{RPC: rpc})

	got, err := c.CreateApplicationCheckoutSession(context.Background(), clients.ApplicationCheckoutRequest{
		IdempotencyKey: "idem-app-1",
		TenantID:       "tenant-1",
		LearnerGCID:    "gcid-1",
		ApplicationID:  "app-1",
		CourseID:       "course-1",
		AmountCents:    99900,
		Currency:       "SGD",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rpc.applicationReq.GetApplicationId() != "app-1" {
		t.Errorf("ApplicationId = %q, want app-1", rpc.applicationReq.GetApplicationId())
	}
	if rpc.applicationReq.GetCourseId() != "course-1" {
		t.Errorf("CourseId = %q, want course-1", rpc.applicationReq.GetCourseId())
	}
	if got.PurchaseID != "p-2" {
		t.Errorf("PurchaseID = %q, want p-2", got.PurchaseID)
	}
}

// --- CreateCompanionEggCheckoutSession ---------------------------------------

func TestCreateCompanionEggCheckoutSession_ForwardsExpiryTimestamps(t *testing.T) {
	t.Parallel()
	soft := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	hard := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	rpc := &fakePaymentsRPC{
		companionEggResp: &paymentsv1.CreateCompanionEggCheckoutSessionResponse{
			PurchaseId:        "p-egg-1",
			StripeSessionId:   "cs_test_egg_1",
			StripeCheckoutUrl: "https://checkout.stripe.com/c/pay/cs_test_egg_1",
			State:             paymentsv1.PurchaseState_PURCHASE_STATE_CHECKOUT_STARTED,
		},
	}
	c, _ := clients.NewPaymentsClient(clients.PaymentsClientConfig{RPC: rpc})

	got, err := c.CreateCompanionEggCheckoutSession(context.Background(), clients.CompanionEggCheckoutRequest{
		IdempotencyKey:       "idem-egg-1",
		TenantID:             "tenant-1",
		LearnerGCID:          "gcid-1",
		EggSKU:               "egg-standard",
		SuggestedFocalAtomID: "atom-1",
		AmountCents:          1999,
		Currency:             "SGD",
		SoftExpiryAt:         soft,
		HardExpiryAt:         hard,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rpc.companionEggReq.GetEggSku() != "egg-standard" {
		t.Errorf("EggSku = %q, want egg-standard", rpc.companionEggReq.GetEggSku())
	}
	if rpc.companionEggReq.GetSoftExpiryAt() == nil {
		t.Fatal("SoftExpiryAt nil; expected forwarded timestamp")
	}
	if !rpc.companionEggReq.GetSoftExpiryAt().AsTime().Equal(soft) {
		t.Errorf("SoftExpiryAt = %v, want %v", rpc.companionEggReq.GetSoftExpiryAt().AsTime(), soft)
	}
	if rpc.companionEggReq.GetHardExpiryAt() == nil {
		t.Fatal("HardExpiryAt nil; expected forwarded timestamp")
	}
	if got.PurchaseID != "p-egg-1" {
		t.Errorf("PurchaseID = %q, want p-egg-1", got.PurchaseID)
	}
}

func TestCreateCompanionEggCheckoutSession_OmitsZeroExpiry(t *testing.T) {
	t.Parallel()
	rpc := &fakePaymentsRPC{
		companionEggResp: &paymentsv1.CreateCompanionEggCheckoutSessionResponse{
			PurchaseId: "p-egg-2",
			State:      paymentsv1.PurchaseState_PURCHASE_STATE_CHECKOUT_STARTED,
		},
	}
	c, _ := clients.NewPaymentsClient(clients.PaymentsClientConfig{RPC: rpc})

	_, err := c.CreateCompanionEggCheckoutSession(context.Background(), clients.CompanionEggCheckoutRequest{
		IdempotencyKey: "idem-egg-2",
		TenantID:       "tenant-1",
		LearnerGCID:    "gcid-1",
		EggSKU:         "egg-standard",
		AmountCents:    1999,
		Currency:       "SGD",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rpc.companionEggReq.GetSoftExpiryAt() != nil {
		t.Error("SoftExpiryAt should be nil when caller passed zero time")
	}
	if rpc.companionEggReq.GetHardExpiryAt() != nil {
		t.Error("HardExpiryAt should be nil when caller passed zero time")
	}
}

// --- CreateManaTopUpSession -------------------------------------------------

func TestCreateManaTopUpSession_ForwardsAdminGCIDAndManaUnits(t *testing.T) {
	t.Parallel()
	rpc := &fakePaymentsRPC{
		manaTopUpResp: &paymentsv1.CreateManaTopUpSessionResponse{
			PurchaseId:        "p-mana-1",
			StripeSessionId:   "cs_test_mana_1",
			StripeCheckoutUrl: "https://checkout.stripe.com/c/pay/cs_test_mana_1",
			State:             paymentsv1.PurchaseState_PURCHASE_STATE_CHECKOUT_STARTED,
		},
	}
	c, _ := clients.NewPaymentsClient(clients.PaymentsClientConfig{RPC: rpc})

	got, err := c.CreateManaTopUpSession(context.Background(), clients.ManaTopUpRequest{
		IdempotencyKey: "idem-mana-1",
		TenantID:       "tenant-1",
		AdminGCID:      "admin-gcid-1",
		SKU:            "mana-pack-100k",
		ManaUnits:      100000,
		AmountCents:    19900,
		Currency:       "SGD",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rpc.manaTopUpReq.GetAdminGcid() != "admin-gcid-1" {
		t.Errorf("AdminGcid = %q, want admin-gcid-1", rpc.manaTopUpReq.GetAdminGcid())
	}
	if rpc.manaTopUpReq.GetManaUnits() != 100000 {
		t.Errorf("ManaUnits = %d, want 100000", rpc.manaTopUpReq.GetManaUnits())
	}
	if got.StripeSessionID != "cs_test_mana_1" {
		t.Errorf("StripeSessionID = %q, want cs_test_mana_1", got.StripeSessionID)
	}
}

// --- CreateSubscription -----------------------------------------------------

func TestCreateSubscription_MapsBillingPeriodMonthly(t *testing.T) {
	t.Parallel()
	rpc := &fakePaymentsRPC{
		subscriptionResp: &paymentsv1.CreateSubscriptionResponse{
			PurchaseId: "p-sub-1",
			State:      paymentsv1.PurchaseState_PURCHASE_STATE_CHECKOUT_STARTED,
		},
	}
	c, _ := clients.NewPaymentsClient(clients.PaymentsClientConfig{RPC: rpc})

	_, err := c.CreateSubscription(context.Background(), clients.SubscriptionRequest{
		IdempotencyKey: "idem-sub-1",
		TenantID:       "tenant-1",
		LearnerGCID:    "gcid-1",
		PlanSKU:        "companion-standard",
		BillingPeriod:  "monthly",
		AmountCents:    999,
		Currency:       "SGD",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rpc.subscriptionReq.GetBillingPeriod() != paymentsv1.BillingPeriod_BILLING_PERIOD_MONTHLY {
		t.Errorf("BillingPeriod = %v, want MONTHLY", rpc.subscriptionReq.GetBillingPeriod())
	}
}

func TestCreateSubscription_MapsBillingPeriodAnnually(t *testing.T) {
	t.Parallel()
	rpc := &fakePaymentsRPC{
		subscriptionResp: &paymentsv1.CreateSubscriptionResponse{
			PurchaseId: "p-sub-2",
			State:      paymentsv1.PurchaseState_PURCHASE_STATE_CHECKOUT_STARTED,
		},
	}
	c, _ := clients.NewPaymentsClient(clients.PaymentsClientConfig{RPC: rpc})

	_, err := c.CreateSubscription(context.Background(), clients.SubscriptionRequest{
		IdempotencyKey: "idem-sub-2",
		TenantID:       "tenant-1",
		LearnerGCID:    "gcid-1",
		PlanSKU:        "companion-premium",
		BillingPeriod:  "annually",
		AmountCents:    9999,
		Currency:       "SGD",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rpc.subscriptionReq.GetBillingPeriod() != paymentsv1.BillingPeriod_BILLING_PERIOD_ANNUALLY {
		t.Errorf("BillingPeriod = %v, want ANNUALLY", rpc.subscriptionReq.GetBillingPeriod())
	}
}

func TestCreateSubscription_MapsBillingPeriodUnspecifiedOnUnknown(t *testing.T) {
	t.Parallel()
	rpc := &fakePaymentsRPC{
		subscriptionResp: &paymentsv1.CreateSubscriptionResponse{
			PurchaseId: "p-sub-3",
			State:      paymentsv1.PurchaseState_PURCHASE_STATE_CHECKOUT_STARTED,
		},
	}
	c, _ := clients.NewPaymentsClient(clients.PaymentsClientConfig{RPC: rpc})

	_, err := c.CreateSubscription(context.Background(), clients.SubscriptionRequest{
		IdempotencyKey: "idem-sub-3",
		TenantID:       "tenant-1",
		LearnerGCID:    "gcid-1",
		PlanSKU:        "companion-premium",
		BillingPeriod:  "fortnightly",
		AmountCents:    9999,
		Currency:       "SGD",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rpc.subscriptionReq.GetBillingPeriod() != paymentsv1.BillingPeriod_BILLING_PERIOD_UNSPECIFIED {
		t.Errorf("BillingPeriod = %v, want UNSPECIFIED for unknown input", rpc.subscriptionReq.GetBillingPeriod())
	}
}

// --- State mapping ----------------------------------------------------------

func TestPurchaseStateMappings_ConvergeOnSnakeCase(t *testing.T) {
	t.Parallel()
	cases := []struct {
		state paymentsv1.PurchaseState
		want  string
	}{
		{paymentsv1.PurchaseState_PURCHASE_STATE_CHECKOUT_STARTED, "checkout_started"},
		{paymentsv1.PurchaseState_PURCHASE_STATE_PAYMENT_CAPTURED, "payment_captured"},
		{paymentsv1.PurchaseState_PURCHASE_STATE_PAYMENT_FAILED, "payment_failed"},
		{paymentsv1.PurchaseState_PURCHASE_STATE_REFUNDED, "refunded"},
		{paymentsv1.PurchaseState_PURCHASE_STATE_EXPIRED, "expired"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.want, func(t *testing.T) {
			t.Parallel()
			rpc := &fakePaymentsRPC{
				courseResp: &paymentsv1.CreateCourseCheckoutSessionResponse{
					PurchaseId: "p", State: c.state,
				},
			}
			pc, _ := clients.NewPaymentsClient(clients.PaymentsClientConfig{RPC: rpc})
			got, err := pc.CreateCourseCheckoutSession(context.Background(), clients.CourseCheckoutRequest{
				IdempotencyKey: "k",
				TenantID:       "t",
				LearnerGCID:    "g",
				CourseID:       "c",
				AmountCents:    1,
				Currency:       "SGD",
			})
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got.State != c.want {
				t.Errorf("State = %q, want %q", got.State, c.want)
			}
		})
	}
}

// --- Wrapped-error preserves codes ------------------------------------------

func TestWrappedGRPCError_RetainsCode(t *testing.T) {
	t.Parallel()
	rpc := &fakePaymentsRPC{
		manaTopUpErr: status.Error(codes.PermissionDenied, "tenant admin only"),
	}
	c, _ := clients.NewPaymentsClient(clients.PaymentsClientConfig{RPC: rpc})

	_, err := c.CreateManaTopUpSession(context.Background(), clients.ManaTopUpRequest{
		IdempotencyKey: "k", TenantID: "t", AdminGCID: "g",
		SKU: "s", ManaUnits: 1, AmountCents: 1, Currency: "SGD",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "PermissionDenied") {
		t.Errorf("err = %q, want it to contain PermissionDenied", err)
	}
}

// --- CreateTenantAddonCheckoutSession — CHO-1736 H+ Marketplace Subscribe ----

func TestCreateTenantAddonCheckoutSession_ForwardsAddonFieldsAndAdminGCID(t *testing.T) {
	t.Parallel()
	rpc := &fakePaymentsRPC{
		tenantAddonResp: &paymentsv1.CreateTenantAddonCheckoutSessionResponse{
			PurchaseId:        "p-addon-1",
			StripeSessionId:   "cs_test_addon_1",
			StripeCheckoutUrl: "https://checkout.stripe.com/c/pay/cs_test_addon_1",
			State:             paymentsv1.PurchaseState_PURCHASE_STATE_CHECKOUT_STARTED,
		},
	}
	c, _ := clients.NewPaymentsClient(clients.PaymentsClientConfig{RPC: rpc})

	got, err := c.CreateTenantAddonCheckoutSession(context.Background(), clients.TenantAddonCheckoutRequest{
		IdempotencyKey: "idem-addon-1",
		TenantID:       "tenant-1",
		AdminGCID:      "admin-gcid-1",
		AddonPlanID:    "019e0000-0000-7000-8000-aaaaaaaaaaaa",
		AddonCode:      "knowledge_graph",
		TierCode:       "pro",
		AmountCents:    4900,
		Currency:       "SGD",
		SuccessURL:     "https://chora.site/h/marketplace/x?checkout=success",
		CancelURL:      "https://chora.site/h/marketplace/x?checkout=cancel",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rpc.tenantAddonReq == nil {
		t.Fatal("RPC was not invoked")
	}
	if rpc.tenantAddonReq.GetAdminGcid() != "admin-gcid-1" {
		t.Errorf("AdminGcid = %q", rpc.tenantAddonReq.GetAdminGcid())
	}
	if rpc.tenantAddonReq.GetAddonPlanId() != "019e0000-0000-7000-8000-aaaaaaaaaaaa" {
		t.Errorf("AddonPlanId = %q", rpc.tenantAddonReq.GetAddonPlanId())
	}
	if rpc.tenantAddonReq.GetAddonCode() != "knowledge_graph" {
		t.Errorf("AddonCode = %q", rpc.tenantAddonReq.GetAddonCode())
	}
	if rpc.tenantAddonReq.GetTierCode() != "pro" {
		t.Errorf("TierCode = %q", rpc.tenantAddonReq.GetTierCode())
	}
	if rpc.tenantAddonReq.GetAmountCents() != 4900 {
		t.Errorf("AmountCents = %d", rpc.tenantAddonReq.GetAmountCents())
	}
	if rpc.tenantAddonReq.GetCurrency() != "SGD" {
		t.Errorf("Currency = %q", rpc.tenantAddonReq.GetCurrency())
	}
	if got.StripeSessionID != "cs_test_addon_1" {
		t.Errorf("StripeSessionID = %q", got.StripeSessionID)
	}
	if got.StripeCheckoutURL != "https://checkout.stripe.com/c/pay/cs_test_addon_1" {
		t.Errorf("StripeCheckoutURL = %q", got.StripeCheckoutURL)
	}
	if got.State != "checkout_started" {
		t.Errorf("State = %q, want checkout_started", got.State)
	}
}

func TestCreateTenantAddonCheckoutSession_PropagatesError(t *testing.T) {
	t.Parallel()
	rpc := &fakePaymentsRPC{tenantAddonErr: errors.New("upstream payments down")}
	c, _ := clients.NewPaymentsClient(clients.PaymentsClientConfig{RPC: rpc})

	_, err := c.CreateTenantAddonCheckoutSession(context.Background(), clients.TenantAddonCheckoutRequest{
		IdempotencyKey: "idem-addon-2",
		TenantID:       "tenant-1",
		AdminGCID:      "admin-gcid-1",
		AddonPlanID:    "019e0000-0000-7000-8000-bbbbbbbbbbbb",
		AddonCode:      "companion",
		TierCode:       "starter",
		AmountCents:    3900,
		Currency:       "SGD",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
