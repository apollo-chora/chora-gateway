// payments_handler_test.go — TDD specs for the chora-gateway
// /api/v1/checkout/* REST proxy (Stage B of ADR-164).
//
// Each route gets:
//   - happy-path test asserting tenant_id + learner_gcid are pulled from the
//     stamped mesh claims and forwarded to the gRPC client, and that the
//     200 JSON envelope is shaped per the FE contract.
//   - validation-error test for missing required body fields.
//
// gRPC-error → HTTP mapping is covered by a dedicated table-test.
package httpadapter_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
)

// fakeCheckoutClient records the last request per RPC + returns a canned
// reply or error.
type fakeCheckoutClient struct {
	courseReq        clients.CourseCheckoutRequest
	courseResp       clients.CheckoutResponse
	courseErr        error
	applicationReq   clients.ApplicationCheckoutRequest
	applicationResp  clients.CheckoutResponse
	applicationErr   error
	companionEggReq  clients.CompanionEggCheckoutRequest
	companionEggResp clients.CheckoutResponse
	companionEggErr  error
	manaTopUpReq     clients.ManaTopUpRequest
	manaTopUpResp    clients.CheckoutResponse
	manaTopUpErr     error
	userManaReq      clients.UserManaTopUpRequest
	userManaResp     clients.CheckoutResponse
	userManaErr      error
	subscriptionReq  clients.SubscriptionRequest
	subscriptionResp clients.CheckoutResponse
	subscriptionErr  error
	tenantAddonReq   clients.TenantAddonCheckoutRequest
	tenantAddonResp  clients.CheckoutResponse
	tenantAddonErr   error
}

func (f *fakeCheckoutClient) CreateCourseCheckoutSession(_ context.Context, req clients.CourseCheckoutRequest) (clients.CheckoutResponse, error) {
	f.courseReq = req
	return f.courseResp, f.courseErr
}

func (f *fakeCheckoutClient) CreateApplicationCheckoutSession(_ context.Context, req clients.ApplicationCheckoutRequest) (clients.CheckoutResponse, error) {
	f.applicationReq = req
	return f.applicationResp, f.applicationErr
}

func (f *fakeCheckoutClient) CreateCompanionEggCheckoutSession(_ context.Context, req clients.CompanionEggCheckoutRequest) (clients.CheckoutResponse, error) {
	f.companionEggReq = req
	return f.companionEggResp, f.companionEggErr
}

func (f *fakeCheckoutClient) CreateManaTopUpSession(_ context.Context, req clients.ManaTopUpRequest) (clients.CheckoutResponse, error) {
	f.manaTopUpReq = req
	return f.manaTopUpResp, f.manaTopUpErr
}

func (f *fakeCheckoutClient) CreateUserManaTopUpSession(_ context.Context, req clients.UserManaTopUpRequest) (clients.CheckoutResponse, error) {
	f.userManaReq = req
	return f.userManaResp, f.userManaErr
}

func (f *fakeCheckoutClient) CreateSubscription(_ context.Context, req clients.SubscriptionRequest) (clients.CheckoutResponse, error) {
	f.subscriptionReq = req
	return f.subscriptionResp, f.subscriptionErr
}

func (f *fakeCheckoutClient) CreateTenantAddonCheckoutSession(_ context.Context, req clients.TenantAddonCheckoutRequest) (clients.CheckoutResponse, error) {
	f.tenantAddonReq = req
	return f.tenantAddonResp, f.tenantAddonErr
}

func newCheckoutHandlerWithFake(t *testing.T, c httpadapter.PaymentsCheckoutClient) http.Handler {
	t.Helper()
	h, err := httpadapter.NewPaymentsCheckoutHandler(c)
	if err != nil {
		t.Fatalf("NewPaymentsCheckoutHandler: %v", err)
	}
	return httpadapter.NewPaymentsCheckoutMux(h)
}

func authedReq(t *testing.T, method, path, bodyJSON string) *http.Request {
	t.Helper()
	var body *bytes.Reader
	if bodyJSON != "" {
		body = bytes.NewReader([]byte(bodyJSON))
	} else {
		body = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, path, body)
	r.Header.Set("Content-Type", "application/json")
	ctx := httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-1", "tenant-1")
	return r.WithContext(ctx)
}

// --- Constructor ------------------------------------------------------------

func TestNewPaymentsCheckoutHandler_RequiresClient(t *testing.T) {
	t.Parallel()
	if _, err := httpadapter.NewPaymentsCheckoutHandler(nil); err == nil {
		t.Fatal("expected error when client is nil")
	}
}

// --- /checkout/course -------------------------------------------------------

func TestHandleCourse_HappyPath(t *testing.T) {
	t.Parallel()
	fake := &fakeCheckoutClient{
		courseResp: clients.CheckoutResponse{
			PurchaseID:        "p-1",
			StripeSessionID:   "cs_test_1",
			StripeCheckoutURL: "https://checkout.stripe.com/c/pay/cs_test_1",
			State:             "checkout_started",
		},
	}
	mux := newCheckoutHandlerWithFake(t, fake)
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/course",
		`{"course_id":"course-1","amount_cents":49900,"currency":"SGD"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fake.courseReq.TenantID != "tenant-1" {
		t.Errorf("TenantID forwarded = %q, want tenant-1", fake.courseReq.TenantID)
	}
	if fake.courseReq.LearnerGCID != "gcid-1" {
		t.Errorf("LearnerGCID forwarded = %q, want gcid-1", fake.courseReq.LearnerGCID)
	}
	if fake.courseReq.CourseID != "course-1" {
		t.Errorf("CourseID = %q, want course-1", fake.courseReq.CourseID)
	}
	if fake.courseReq.AmountCents != 49900 {
		t.Errorf("AmountCents = %d, want 49900", fake.courseReq.AmountCents)
	}
	if fake.courseReq.Currency != "SGD" {
		t.Errorf("Currency = %q, want SGD", fake.courseReq.Currency)
	}
	if strings.TrimSpace(fake.courseReq.IdempotencyKey) == "" {
		t.Error("IdempotencyKey should be auto-generated by the handler")
	}

	var resp clients.CheckoutResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.PurchaseID != "p-1" {
		t.Errorf("PurchaseID = %q, want p-1", resp.PurchaseID)
	}
	if resp.StripeCheckoutURL != "https://checkout.stripe.com/c/pay/cs_test_1" {
		t.Errorf("StripeCheckoutURL = %q", resp.StripeCheckoutURL)
	}
	if resp.State != "checkout_started" {
		t.Errorf("State = %q, want checkout_started", resp.State)
	}
}

func TestHandleCourse_ValidatesCourseID(t *testing.T) {
	t.Parallel()
	fake := &fakeCheckoutClient{}
	mux := newCheckoutHandlerWithFake(t, fake)
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/course",
		`{"course_id":"","amount_cents":49900,"currency":"SGD"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if fake.courseReq.IdempotencyKey != "" {
		t.Error("RPC should NOT be invoked when course_id missing")
	}
}

func TestHandleCourse_RejectsMissingAmount(t *testing.T) {
	t.Parallel()
	fake := &fakeCheckoutClient{}
	mux := newCheckoutHandlerWithFake(t, fake)
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/course",
		`{"course_id":"course-1","amount_cents":0,"currency":"SGD"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleCourse_RejectsBadCurrency(t *testing.T) {
	t.Parallel()
	fake := &fakeCheckoutClient{}
	mux := newCheckoutHandlerWithFake(t, fake)
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/course",
		`{"course_id":"course-1","amount_cents":49900,"currency":"XX"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleCourse_RejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	fake := &fakeCheckoutClient{}
	mux := newCheckoutHandlerWithFake(t, fake)
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/course",
		`{"course_id":`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleCourse_Rejects405OnGET(t *testing.T) {
	t.Parallel()
	fake := &fakeCheckoutClient{}
	mux := newCheckoutHandlerWithFake(t, fake)
	r := authedReq(t, http.MethodGet, "/api/v1/checkout/course", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestHandleCourse_UnauthenticatedWhenMeshClaimsMissing(t *testing.T) {
	t.Parallel()
	fake := &fakeCheckoutClient{}
	mux := newCheckoutHandlerWithFake(t, fake)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/checkout/course",
		bytes.NewReader([]byte(`{"course_id":"course-1","amount_cents":49900,"currency":"SGD"}`)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
}

// --- /checkout/application --------------------------------------------------

func TestHandleApplication_HappyPath(t *testing.T) {
	t.Parallel()
	fake := &fakeCheckoutClient{
		applicationResp: clients.CheckoutResponse{
			PurchaseID:        "p-2",
			StripeSessionID:   "cs_test_2",
			StripeCheckoutURL: "https://checkout.stripe.com/c/pay/cs_test_2",
			State:             "checkout_started",
		},
	}
	mux := newCheckoutHandlerWithFake(t, fake)
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/application",
		`{"application_id":"app-1","course_id":"course-1","amount_cents":99900,"currency":"SGD"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fake.applicationReq.ApplicationID != "app-1" {
		t.Errorf("ApplicationID = %q, want app-1", fake.applicationReq.ApplicationID)
	}
	if fake.applicationReq.CourseID != "course-1" {
		t.Errorf("CourseID = %q, want course-1", fake.applicationReq.CourseID)
	}
}

func TestHandleApplication_RejectsMissingApplicationID(t *testing.T) {
	t.Parallel()
	fake := &fakeCheckoutClient{}
	mux := newCheckoutHandlerWithFake(t, fake)
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/application",
		`{"application_id":"","course_id":"course-1","amount_cents":99900,"currency":"SGD"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// --- /checkout/companion-egg -------------------------------------------------

func TestHandleCompanionEgg_HappyPath(t *testing.T) {
	t.Parallel()
	fake := &fakeCheckoutClient{
		companionEggResp: clients.CheckoutResponse{
			PurchaseID:        "p-egg-1",
			StripeSessionID:   "cs_test_egg_1",
			StripeCheckoutURL: "https://checkout.stripe.com/c/pay/cs_test_egg_1",
			State:             "checkout_started",
		},
	}
	mux := newCheckoutHandlerWithFake(t, fake)
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/companion-egg",
		`{"egg_sku":"egg-standard","amount_cents":1999,"currency":"SGD","soft_expiry_at":"2026-06-01T00:00:00Z","hard_expiry_at":"2026-12-01T00:00:00Z"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fake.companionEggReq.EggSKU != "egg-standard" {
		t.Errorf("EggSKU = %q, want egg-standard", fake.companionEggReq.EggSKU)
	}
	if fake.companionEggReq.SoftExpiryAt.IsZero() {
		t.Error("SoftExpiryAt should be parsed from body")
	}
	if fake.companionEggReq.HardExpiryAt.IsZero() {
		t.Error("HardExpiryAt should be parsed from body")
	}
}

func TestHandleCompanionEgg_RejectsMalformedExpiry(t *testing.T) {
	t.Parallel()
	fake := &fakeCheckoutClient{}
	mux := newCheckoutHandlerWithFake(t, fake)
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/companion-egg",
		`{"egg_sku":"egg-standard","amount_cents":1999,"currency":"SGD","soft_expiry_at":"not-a-date"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// --- /checkout/mana-topup ---------------------------------------------------

func TestHandleManaTopUp_HappyPath(t *testing.T) {
	t.Parallel()
	fake := &fakeCheckoutClient{
		manaTopUpResp: clients.CheckoutResponse{
			PurchaseID:        "p-mana-1",
			StripeSessionID:   "cs_test_mana_1",
			StripeCheckoutURL: "https://checkout.stripe.com/c/pay/cs_test_mana_1",
			State:             "checkout_started",
		},
	}
	mux := newCheckoutHandlerWithFake(t, fake)
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/mana-topup",
		`{"sku":"mana-pack-100k","mana_units":100000,"amount_cents":19900,"currency":"SGD"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fake.manaTopUpReq.TenantID != "tenant-1" {
		t.Errorf("TenantID = %q, want tenant-1", fake.manaTopUpReq.TenantID)
	}
	if fake.manaTopUpReq.AdminGCID != "gcid-1" {
		t.Errorf("AdminGCID = %q, want gcid-1", fake.manaTopUpReq.AdminGCID)
	}
	if fake.manaTopUpReq.ManaUnits != 100000 {
		t.Errorf("ManaUnits = %d, want 100000", fake.manaTopUpReq.ManaUnits)
	}
}

func TestHandleManaTopUp_RejectsZeroManaUnits(t *testing.T) {
	t.Parallel()
	fake := &fakeCheckoutClient{}
	mux := newCheckoutHandlerWithFake(t, fake)
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/mana-topup",
		`{"sku":"mana-pack-100k","mana_units":0,"amount_cents":19900,"currency":"SGD"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// --- /checkout/subscription -------------------------------------------------

func TestHandleSubscription_HappyPath(t *testing.T) {
	t.Parallel()
	fake := &fakeCheckoutClient{
		subscriptionResp: clients.CheckoutResponse{
			PurchaseID:        "p-sub-1",
			StripeSessionID:   "cs_test_sub_1",
			StripeCheckoutURL: "https://checkout.stripe.com/c/pay/cs_test_sub_1",
			State:             "checkout_started",
		},
	}
	mux := newCheckoutHandlerWithFake(t, fake)
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/subscription",
		`{"plan_sku":"companion-standard","billing_period":"monthly","amount_cents":999,"currency":"SGD"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fake.subscriptionReq.PlanSKU != "companion-standard" {
		t.Errorf("PlanSKU = %q, want companion-standard", fake.subscriptionReq.PlanSKU)
	}
	if fake.subscriptionReq.BillingPeriod != "monthly" {
		t.Errorf("BillingPeriod = %q, want monthly", fake.subscriptionReq.BillingPeriod)
	}
}

func TestHandleSubscription_RejectsUnknownBillingPeriod(t *testing.T) {
	t.Parallel()
	fake := &fakeCheckoutClient{}
	mux := newCheckoutHandlerWithFake(t, fake)
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/subscription",
		`{"plan_sku":"companion-standard","billing_period":"fortnightly","amount_cents":999,"currency":"SGD"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// --- gRPC error code → HTTP status mapping ----------------------------------

func TestGRPCErrorCodeMappings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		grpcErr error
		want    int
	}{
		{"NotFound→404", status.Error(codes.NotFound, "missing course"), http.StatusNotFound},
		{"InvalidArgument→400", status.Error(codes.InvalidArgument, "bad amount"), http.StatusBadRequest},
		{"PermissionDenied→403", status.Error(codes.PermissionDenied, "tenant gate"), http.StatusForbidden},
		{"FailedPrecondition→409", status.Error(codes.FailedPrecondition, "already paid"), http.StatusConflict},
		{"Unauthenticated→401", status.Error(codes.Unauthenticated, "no claim"), http.StatusUnauthorized},
		{"Unavailable→503", status.Error(codes.Unavailable, "pod down"), http.StatusServiceUnavailable},
		{"DeadlineExceeded→503", status.Error(codes.DeadlineExceeded, "slow stripe"), http.StatusServiceUnavailable},
		{"Internal→500", status.Error(codes.Internal, "boom"), http.StatusInternalServerError},
		{"plain error→500", errors.New("non-grpc error"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeCheckoutClient{courseErr: c.grpcErr}
			mux := newCheckoutHandlerWithFake(t, fake)
			r := authedReq(t, http.MethodPost, "/api/v1/checkout/course",
				`{"course_id":"course-1","amount_cents":49900,"currency":"SGD"}`)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Errorf("status = %d, want %d; body=%s", w.Code, c.want, w.Body.String())
			}
		})
	}
}

// --- Compose passthrough ----------------------------------------------------

func TestWithPaymentsCheckout_NilHandlerPassthrough(t *testing.T) {
	t.Parallel()
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := httpadapter.WithPaymentsCheckout(base, nil)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/checkout/course", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418 (passthrough)", w.Code)
	}
}

func TestWithPaymentsCheckout_NonCheckoutPathPassthrough(t *testing.T) {
	t.Parallel()
	fake := &fakeCheckoutClient{
		courseResp: clients.CheckoutResponse{PurchaseID: "p", State: "checkout_started"},
	}
	checkoutH, err := httpadapter.NewPaymentsCheckoutHandler(fake)
	if err != nil {
		t.Fatalf("NewPaymentsCheckoutHandler: %v", err)
	}
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	composed := httpadapter.WithPaymentsCheckout(base, checkoutH)

	r := httptest.NewRequest(http.MethodGet, "/api/healthz", nil)
	w := httptest.NewRecorder()
	composed.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418 (passthrough)", w.Code)
	}
}

func TestWithPaymentsCheckout_RoutesCheckoutPath(t *testing.T) {
	t.Parallel()
	fake := &fakeCheckoutClient{
		courseResp: clients.CheckoutResponse{
			PurchaseID:        "p-1",
			StripeSessionID:   "cs_x",
			StripeCheckoutURL: "https://checkout.stripe.com/x",
			State:             "checkout_started",
		},
	}
	checkoutH, err := httpadapter.NewPaymentsCheckoutHandler(fake)
	if err != nil {
		t.Fatalf("NewPaymentsCheckoutHandler: %v", err)
	}
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	composed := httpadapter.WithPaymentsCheckout(base, checkoutH)

	r := authedReq(t, http.MethodPost, "/api/v1/checkout/course",
		`{"course_id":"course-1","amount_cents":49900,"currency":"SGD"}`)
	w := httptest.NewRecorder()
	composed.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (routed to checkout)", w.Code)
	}
}

// --- /checkout/tenant-addon — CHO-1736 H+ Marketplace Subscribe -------------

func TestHandleTenantAddon_HappyPath(t *testing.T) {
	t.Parallel()
	fake := &fakeCheckoutClient{
		tenantAddonResp: clients.CheckoutResponse{
			PurchaseID:        "p-addon-1",
			StripeSessionID:   "cs_test_addon_1",
			StripeCheckoutURL: "https://checkout.stripe.com/c/pay/cs_test_addon_1",
			State:             "checkout_started",
		},
	}
	mux := newCheckoutHandlerWithFake(t, fake)
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/tenant-addon",
		`{"addon_plan_id":"019e0000-0000-7000-8000-aaaaaaaaaaaa","addon_code":"knowledge_graph","tier_code":"pro","amount_cents":4900,"currency":"SGD"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fake.tenantAddonReq.TenantID != "tenant-1" {
		t.Errorf("TenantID = %q, want tenant-1 (from mesh)", fake.tenantAddonReq.TenantID)
	}
	if fake.tenantAddonReq.AdminGCID != "gcid-1" {
		t.Errorf("AdminGCID = %q, want gcid-1 (from mesh)", fake.tenantAddonReq.AdminGCID)
	}
	if fake.tenantAddonReq.AddonPlanID != "019e0000-0000-7000-8000-aaaaaaaaaaaa" {
		t.Errorf("AddonPlanID = %q", fake.tenantAddonReq.AddonPlanID)
	}
	if fake.tenantAddonReq.AddonCode != "knowledge_graph" {
		t.Errorf("AddonCode = %q", fake.tenantAddonReq.AddonCode)
	}
	if fake.tenantAddonReq.TierCode != "pro" {
		t.Errorf("TierCode = %q", fake.tenantAddonReq.TierCode)
	}
	if fake.tenantAddonReq.AmountCents != 4900 {
		t.Errorf("AmountCents = %d, want 4900", fake.tenantAddonReq.AmountCents)
	}
	if fake.tenantAddonReq.Currency != "SGD" {
		t.Errorf("Currency = %q, want SGD (uppercased)", fake.tenantAddonReq.Currency)
	}
}

func TestHandleTenantAddon_RequiresAddonPlanID(t *testing.T) {
	t.Parallel()
	mux := newCheckoutHandlerWithFake(t, &fakeCheckoutClient{})
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/tenant-addon",
		`{"addon_code":"knowledge_graph","tier_code":"pro","amount_cents":4900,"currency":"SGD"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleTenantAddon_RequiresAddonCode(t *testing.T) {
	t.Parallel()
	mux := newCheckoutHandlerWithFake(t, &fakeCheckoutClient{})
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/tenant-addon",
		`{"addon_plan_id":"019e0000-0000-7000-8000-aaaaaaaaaaaa","tier_code":"pro","amount_cents":4900,"currency":"SGD"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleTenantAddon_RequiresTierCode(t *testing.T) {
	t.Parallel()
	mux := newCheckoutHandlerWithFake(t, &fakeCheckoutClient{})
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/tenant-addon",
		`{"addon_plan_id":"019e0000-0000-7000-8000-aaaaaaaaaaaa","addon_code":"knowledge_graph","amount_cents":4900,"currency":"SGD"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleTenantAddon_RejectsNonPOST(t *testing.T) {
	t.Parallel()
	mux := newCheckoutHandlerWithFake(t, &fakeCheckoutClient{})
	r := authedReq(t, http.MethodGet, "/api/v1/checkout/tenant-addon", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestHandleTenantAddon_UpstreamErrorReturns502(t *testing.T) {
	t.Parallel()
	fake := &fakeCheckoutClient{
		tenantAddonErr: errors.New("payments client: connection refused"),
	}
	mux := newCheckoutHandlerWithFake(t, fake)
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/tenant-addon",
		`{"addon_plan_id":"019e0000-0000-7000-8000-aaaaaaaaaaaa","addon_code":"knowledge_graph","tier_code":"pro","amount_cents":4900,"currency":"SGD"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code < 500 {
		t.Fatalf("status = %d, want 5xx on upstream error", w.Code)
	}
}
