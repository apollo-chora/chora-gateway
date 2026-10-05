// payments_handler.go — Stage B of ADR-164 (chora-payments REST proxy).
//
// Exposes 5 REST routes under /api/v1/checkout/* that translate browser-side
// chora-web requests into chora-payments gRPC RPCs:
//
//	POST /api/v1/checkout/course        → PaymentService.CreateCourseCheckoutSession
//	POST /api/v1/checkout/application   → PaymentService.CreateApplicationCheckoutSession
//	POST /api/v1/checkout/companion-egg  → PaymentService.CreateCompanionEggCheckoutSession
//	POST /api/v1/checkout/mana-topup    → PaymentService.CreateManaTopUpSession
//	POST /api/v1/checkout/subscription  → PaymentService.CreateSubscription
//
// Auth: every route lives behind the canonical Chora session JWT gate
// (DefaultJWTGatedPrefixes). RequireChoraSessionJWT stamps MeshClaims
// (tenant_id + learner_gcid) onto the context BEFORE this handler runs.
// Anonymous callers are 401'd upstream of this file.
//
// The CJ2-STRIPE-CORS FE ask (per `docs/m13/e2e-fe-coord-directive-2026-05-16.md`
// §3) is satisfied by the existing gateway-wide CORSMiddleware, which already
// echoes the canonical chora-web origins. /api/v1/checkout/* is therefore
// CORS-clean for free — no additional CORS wiring needed here.
//
// gRPC-error → HTTP mapping (per ADR-164 + the Chora gRPC convention shared
// with other clients/*):
//
//	codes.NotFound           → 404 GATEWAY_NOT_FOUND
//	codes.InvalidArgument    → 400 GATEWAY_INVALID_REQUEST
//	codes.PermissionDenied   → 403 GATEWAY_FORBIDDEN
//	codes.FailedPrecondition → 409 GATEWAY_CONFLICT
//	default                  → 500 GATEWAY_UPSTREAM_ERROR
//
// Per `feedback_no_inline_config`: the gRPC dial address is injected via the
// loader (CHORA_PAYMENTS_GRPC_ADDR). Per `feedback_no_stubs_real_wiring` the
// handler fails-loud when the payments client is nil — production deployments
// MUST configure the env var.
package httpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/google/uuid"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
)

// PaymentsCheckoutClient is the minimal slice of clients.PaymentsClient that
// the handler depends on. Tests inject a fake; production wires the real
// *clients.PaymentsClient.
type PaymentsCheckoutClient interface {
	CreateCourseCheckoutSession(ctx context.Context, req clients.CourseCheckoutRequest) (clients.CheckoutResponse, error)
	CreateApplicationCheckoutSession(ctx context.Context, req clients.ApplicationCheckoutRequest) (clients.CheckoutResponse, error)
	CreateCompanionEggCheckoutSession(ctx context.Context, req clients.CompanionEggCheckoutRequest) (clients.CheckoutResponse, error)
	CreateManaTopUpSession(ctx context.Context, req clients.ManaTopUpRequest) (clients.CheckoutResponse, error)
	CreateUserManaTopUpSession(ctx context.Context, req clients.UserManaTopUpRequest) (clients.CheckoutResponse, error)
	CreateSubscription(ctx context.Context, req clients.SubscriptionRequest) (clients.CheckoutResponse, error)
	// CHO-1736 — 8th aggregate, H+ Marketplace Subscribe.
	CreateTenantAddonCheckoutSession(ctx context.Context, req clients.TenantAddonCheckoutRequest) (clients.CheckoutResponse, error)
}

// PaymentsCheckoutPathPrefix is the BFF path prefix served by this handler.
// Exposed for inclusion in DefaultJWTGatedPrefixes so RequireChoraSessionJWT
// stamps validated mesh claims onto the request context before the handler
// runs.
const PaymentsCheckoutPathPrefix = "/api/v1/checkout"

// Per-aggregate route leaves (full path = PaymentsCheckoutPathPrefix + leaf).
const (
	pathCheckoutCourse       = "/api/v1/checkout/course"
	pathCheckoutApplication  = "/api/v1/checkout/application"
	pathCheckoutCompanionEgg = "/api/v1/checkout/companion-egg"
	// ADR-254 D9 alias, drop after the SPA cut (WP-X go)
	pathCheckoutCompanionEggAlias = "/api/v1/checkout/familiar-egg"
	pathCheckoutManaTopUp         = "/api/v1/checkout/mana-topup"
	pathCheckoutUserMana          = "/api/v1/checkout/user-mana"
	pathCheckoutSubscription      = "/api/v1/checkout/subscription"
	// CHO-1736 — H+ Marketplace Subscribe (8th aggregate, tenant-scoped).
	pathCheckoutTenantAddon = "/api/v1/checkout/tenant-addon"
)

// PaymentsCheckoutHandler binds the chora-payments gRPC client to the BFF
// /api/v1/checkout/* HTTP surface.
type PaymentsCheckoutHandler struct {
	client    PaymentsCheckoutClient
	manaPacks ManaPackCatalogue
}

// NewPaymentsCheckoutHandler constructs the handler. Returns an error when
// the client is nil — production callers MUST provide a wired gRPC client.
func NewPaymentsCheckoutHandler(client PaymentsCheckoutClient) (*PaymentsCheckoutHandler, error) {
	if client == nil {
		return nil, errors.New("payments-checkout: client required")
	}
	return &PaymentsCheckoutHandler{client: client}, nil
}

// WithManaPacks attaches the server-side mana-pack price catalogue used by the
// per-user mana checkout route (WS-2.4). Returns the same handler for fluent
// wiring. When unset/empty, /api/v1/checkout/user-mana fails loud (503).
func (h *PaymentsCheckoutHandler) WithManaPacks(c ManaPackCatalogue) *PaymentsCheckoutHandler {
	h.manaPacks = c
	return h
}

// NewPaymentsCheckoutMux returns a mux that serves all 5 routes. Combine
// with WithPaymentsCheckout to compose with a base handler that owns
// non-checkout paths.
func NewPaymentsCheckoutMux(h *PaymentsCheckoutHandler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(pathCheckoutCourse, h.handleCourse)
	mux.HandleFunc(pathCheckoutApplication, h.handleApplication)
	mux.HandleFunc(pathCheckoutCompanionEgg, h.handleCompanionEgg)
	// ADR-254 D9 alias, drop after the SPA cut (WP-X go): same handler, same RPC.
	mux.HandleFunc(pathCheckoutCompanionEggAlias, h.handleCompanionEgg)
	mux.HandleFunc(pathCheckoutManaTopUp, h.handleManaTopUp)
	mux.HandleFunc(pathCheckoutUserMana, h.handleUserManaTopUp)
	mux.HandleFunc(pathCheckoutSubscription, h.handleSubscription)
	mux.HandleFunc(pathCheckoutTenantAddon, h.handleTenantAddon)
	return mux
}

// WithPaymentsCheckout composes a PaymentsCheckoutMux with a base handler:
// paths under /api/v1/checkout/* are served by the checkout mux; everything
// else falls through to `base`. Passes through when `h` is nil so
// cmd/server/main.go can opt out during env-driven dev mode.
func WithPaymentsCheckout(base http.Handler, h *PaymentsCheckoutHandler) http.Handler {
	if h == nil {
		return base
	}
	checkoutMux := NewPaymentsCheckoutMux(h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PaymentsCheckoutPathPrefix ||
			strings.HasPrefix(r.URL.Path, PaymentsCheckoutPathPrefix+"/") {
			checkoutMux.ServeHTTP(w, r)
			return
		}
		base.ServeHTTP(w, r)
	})
}

// -----------------------------------------------------------------------------
// Request shapes (decoded from JSON body)
// -----------------------------------------------------------------------------

type courseCheckoutBody struct {
	CourseID    string `json:"course_id"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	SuccessURL  string `json:"success_url"`
	CancelURL   string `json:"cancel_url"`
}

type applicationCheckoutBody struct {
	ApplicationID string `json:"application_id"`
	CourseID      string `json:"course_id"`
	AmountCents   int64  `json:"amount_cents"`
	Currency      string `json:"currency"`
	SuccessURL    string `json:"success_url"`
	CancelURL     string `json:"cancel_url"`
}

type companionEggCheckoutBody struct {
	EggSKU               string `json:"egg_sku"`
	SuggestedFocalAtomID string `json:"suggested_focal_atom_id"`
	AmountCents          int64  `json:"amount_cents"`
	Currency             string `json:"currency"`
	SoftExpiryAt         string `json:"soft_expiry_at"`
	HardExpiryAt         string `json:"hard_expiry_at"`
	SuccessURL           string `json:"success_url"`
	CancelURL            string `json:"cancel_url"`
}

type manaTopUpCheckoutBody struct {
	SKU         string `json:"sku"`
	ManaUnits   int64  `json:"mana_units"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	SuccessURL  string `json:"success_url"`
	CancelURL   string `json:"cancel_url"`
}

// userManaTopUpCheckoutBody is the per-user mana top-up request. Unlike the
// other checkout bodies it carries NO amount_cents/mana_units — the price is
// resolved SERVER-SIDE from the SKU catalogue (WS-2.4) so a client cannot mint
// arbitrary mana for an arbitrary price.
type userManaTopUpCheckoutBody struct {
	SKU        string `json:"sku"`
	SuccessURL string `json:"success_url"`
	CancelURL  string `json:"cancel_url"`
}

type subscriptionCheckoutBody struct {
	PlanSKU       string `json:"plan_sku"`
	BillingPeriod string `json:"billing_period"`
	AmountCents   int64  `json:"amount_cents"`
	Currency      string `json:"currency"`
	SuccessURL    string `json:"success_url"`
	CancelURL     string `json:"cancel_url"`
}

// CHO-1736 — H+ Marketplace Subscribe body shape.
type tenantAddonCheckoutBody struct {
	AddonPlanID string `json:"addon_plan_id"`
	AddonCode   string `json:"addon_code"`
	TierCode    string `json:"tier_code"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	SuccessURL  string `json:"success_url"`
	CancelURL   string `json:"cancel_url"`
}

// -----------------------------------------------------------------------------
// Route handlers
// -----------------------------------------------------------------------------

func (h *PaymentsCheckoutHandler) handleCourse(w http.ResponseWriter, r *http.Request) {
	if !requireCheckoutPOST(w, r) {
		return
	}
	tenantID, gcid, ok := requireMeshIdentity(w, r)
	if !ok {
		return
	}
	var body courseCheckoutBody
	if !decodeCheckoutBody(w, r, &body) {
		return
	}
	if !requireAmountAndCurrency(w, body.AmountCents, body.Currency) {
		return
	}
	if strings.TrimSpace(body.CourseID) == "" {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "course_id required")
		return
	}
	idempotency := newIdempotencyKey()
	resp, err := h.client.CreateCourseCheckoutSession(r.Context(), clients.CourseCheckoutRequest{
		IdempotencyKey: idempotency,
		TenantID:       tenantID,
		LearnerGCID:    gcid,
		CourseID:       strings.TrimSpace(body.CourseID),
		AmountCents:    body.AmountCents,
		Currency:       strings.ToUpper(strings.TrimSpace(body.Currency)),
		SuccessURL:     strings.TrimSpace(body.SuccessURL),
		CancelURL:      strings.TrimSpace(body.CancelURL),
	})
	writeCheckoutResp(w, resp, err)
}

func (h *PaymentsCheckoutHandler) handleApplication(w http.ResponseWriter, r *http.Request) {
	if !requireCheckoutPOST(w, r) {
		return
	}
	tenantID, gcid, ok := requireMeshIdentity(w, r)
	if !ok {
		return
	}
	var body applicationCheckoutBody
	if !decodeCheckoutBody(w, r, &body) {
		return
	}
	if !requireAmountAndCurrency(w, body.AmountCents, body.Currency) {
		return
	}
	if strings.TrimSpace(body.ApplicationID) == "" {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "application_id required")
		return
	}
	if strings.TrimSpace(body.CourseID) == "" {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "course_id required")
		return
	}
	idempotency := newIdempotencyKey()
	resp, err := h.client.CreateApplicationCheckoutSession(r.Context(), clients.ApplicationCheckoutRequest{
		IdempotencyKey: idempotency,
		TenantID:       tenantID,
		LearnerGCID:    gcid,
		ApplicationID:  strings.TrimSpace(body.ApplicationID),
		CourseID:       strings.TrimSpace(body.CourseID),
		AmountCents:    body.AmountCents,
		Currency:       strings.ToUpper(strings.TrimSpace(body.Currency)),
		SuccessURL:     strings.TrimSpace(body.SuccessURL),
		CancelURL:      strings.TrimSpace(body.CancelURL),
	})
	writeCheckoutResp(w, resp, err)
}

func (h *PaymentsCheckoutHandler) handleCompanionEgg(w http.ResponseWriter, r *http.Request) {
	if !requireCheckoutPOST(w, r) {
		return
	}
	tenantID, gcid, ok := requireMeshIdentity(w, r)
	if !ok {
		return
	}
	var body companionEggCheckoutBody
	if !decodeCheckoutBody(w, r, &body) {
		return
	}
	if !requireAmountAndCurrency(w, body.AmountCents, body.Currency) {
		return
	}
	if strings.TrimSpace(body.EggSKU) == "" {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "egg_sku required")
		return
	}
	soft, err := parseOptionalRFC3339(body.SoftExpiryAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "soft_expiry_at must be RFC3339")
		return
	}
	hard, err := parseOptionalRFC3339(body.HardExpiryAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "hard_expiry_at must be RFC3339")
		return
	}
	idempotency := newIdempotencyKey()
	resp, rpcErr := h.client.CreateCompanionEggCheckoutSession(r.Context(), clients.CompanionEggCheckoutRequest{
		IdempotencyKey:       idempotency,
		TenantID:             tenantID,
		LearnerGCID:          gcid,
		EggSKU:               strings.TrimSpace(body.EggSKU),
		SuggestedFocalAtomID: strings.TrimSpace(body.SuggestedFocalAtomID),
		AmountCents:          body.AmountCents,
		Currency:             strings.ToUpper(strings.TrimSpace(body.Currency)),
		SoftExpiryAt:         soft,
		HardExpiryAt:         hard,
		SuccessURL:           strings.TrimSpace(body.SuccessURL),
		CancelURL:            strings.TrimSpace(body.CancelURL),
	})
	writeCheckoutResp(w, resp, rpcErr)
}

func (h *PaymentsCheckoutHandler) handleManaTopUp(w http.ResponseWriter, r *http.Request) {
	if !requireCheckoutPOST(w, r) {
		return
	}
	tenantID, gcid, ok := requireMeshIdentity(w, r)
	if !ok {
		return
	}
	var body manaTopUpCheckoutBody
	if !decodeCheckoutBody(w, r, &body) {
		return
	}
	if !requireAmountAndCurrency(w, body.AmountCents, body.Currency) {
		return
	}
	if strings.TrimSpace(body.SKU) == "" {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "sku required")
		return
	}
	if body.ManaUnits <= 0 {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "mana_units must be > 0")
		return
	}
	idempotency := newIdempotencyKey()
	// gcid is the tenant-admin who initiated the top-up; chora-payments
	// persists it as admin_gcid on the TenantManaTopUp row.
	resp, err := h.client.CreateManaTopUpSession(r.Context(), clients.ManaTopUpRequest{
		IdempotencyKey: idempotency,
		TenantID:       tenantID,
		AdminGCID:      gcid,
		SKU:            strings.TrimSpace(body.SKU),
		ManaUnits:      body.ManaUnits,
		AmountCents:    body.AmountCents,
		Currency:       strings.ToUpper(strings.TrimSpace(body.Currency)),
		SuccessURL:     strings.TrimSpace(body.SuccessURL),
		CancelURL:      strings.TrimSpace(body.CancelURL),
	})
	writeCheckoutResp(w, resp, err)
}

// handleUserManaTopUp mints a Stripe Checkout Session for a per-USER mana
// top-up. The buyer is the authenticated learner (LearnerGCID = mesh gcid);
// on payment capture chora-payments publishes user_mana_topup.payment_captured.v1
// → chora-identity credits the user_mana wallet (WS-2.1). The PRICE is resolved
// server-side from the SKU catalogue (WS-2.4) — the request body carries only a
// sku, never an amount.
func (h *PaymentsCheckoutHandler) handleUserManaTopUp(w http.ResponseWriter, r *http.Request) {
	if !requireCheckoutPOST(w, r) {
		return
	}
	tenantID, gcid, ok := requireMeshIdentity(w, r)
	if !ok {
		return
	}
	if len(h.manaPacks) == 0 {
		// Fail loud rather than fall back to a client-priced mint
		// (feedback_no_stubs_real_wiring + the umbrella-currency abuse vector).
		writeError(w, http.StatusServiceUnavailable, "GATEWAY_UPSTREAM_UNAVAILABLE",
			"mana packs not configured (CHORA_MANA_PACKS unset)")
		return
	}
	var body userManaTopUpCheckoutBody
	if !decodeCheckoutBody(w, r, &body) {
		return
	}
	pack, ok := h.manaPacks.Lookup(body.SKU)
	if !ok {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST",
			"unknown mana pack sku: "+strings.TrimSpace(body.SKU))
		return
	}
	idempotency := newIdempotencyKey()
	resp, err := h.client.CreateUserManaTopUpSession(r.Context(), clients.UserManaTopUpRequest{
		IdempotencyKey: idempotency,
		TenantID:       tenantID,
		LearnerGCID:    gcid,
		SKU:            pack.SKU,
		ManaUnits:      pack.ManaUnits,   // server-resolved
		AmountCents:    pack.AmountCents, // server-resolved
		Currency:       pack.Currency,    // server-resolved
		SuccessURL:     strings.TrimSpace(body.SuccessURL),
		CancelURL:      strings.TrimSpace(body.CancelURL),
	})
	writeCheckoutResp(w, resp, err)
}

func (h *PaymentsCheckoutHandler) handleSubscription(w http.ResponseWriter, r *http.Request) {
	if !requireCheckoutPOST(w, r) {
		return
	}
	tenantID, gcid, ok := requireMeshIdentity(w, r)
	if !ok {
		return
	}
	var body subscriptionCheckoutBody
	if !decodeCheckoutBody(w, r, &body) {
		return
	}
	if !requireAmountAndCurrency(w, body.AmountCents, body.Currency) {
		return
	}
	if strings.TrimSpace(body.PlanSKU) == "" {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "plan_sku required")
		return
	}
	if !isKnownBillingPeriod(body.BillingPeriod) {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST",
			"billing_period must be monthly or annually")
		return
	}
	idempotency := newIdempotencyKey()
	resp, err := h.client.CreateSubscription(r.Context(), clients.SubscriptionRequest{
		IdempotencyKey: idempotency,
		TenantID:       tenantID,
		LearnerGCID:    gcid,
		PlanSKU:        strings.TrimSpace(body.PlanSKU),
		BillingPeriod:  strings.ToLower(strings.TrimSpace(body.BillingPeriod)),
		AmountCents:    body.AmountCents,
		Currency:       strings.ToUpper(strings.TrimSpace(body.Currency)),
		SuccessURL:     strings.TrimSpace(body.SuccessURL),
		CancelURL:      strings.TrimSpace(body.CancelURL),
	})
	writeCheckoutResp(w, resp, err)
}

// handleTenantAddon — CHO-1736 H+ Marketplace Subscribe. Tenant-admin
// initiates a paid add-on subscription; the BFF proxies to chora-payments
// which mints a Stripe Checkout Session + persists a CHECKOUT_STARTED
// TenantAddonPurchase aggregate. On checkout.session.completed Stripe
// webhook, chora-payments emits
// `chora.payments.tenant_addon_purchase.payment_captured.v1` which the
// chora-tenancy Pub/Sub subscriber (Sub 4 / CHO-1740) consumes to flip
// the AddonSubscription aggregate to ACTIVE.
func (h *PaymentsCheckoutHandler) handleTenantAddon(w http.ResponseWriter, r *http.Request) {
	if !requireCheckoutPOST(w, r) {
		return
	}
	tenantID, gcid, ok := requireMeshIdentity(w, r)
	if !ok {
		return
	}
	var body tenantAddonCheckoutBody
	if !decodeCheckoutBody(w, r, &body) {
		return
	}
	if !requireAmountAndCurrency(w, body.AmountCents, body.Currency) {
		return
	}
	if strings.TrimSpace(body.AddonPlanID) == "" {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "addon_plan_id required")
		return
	}
	if strings.TrimSpace(body.AddonCode) == "" {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "addon_code required")
		return
	}
	if strings.TrimSpace(body.TierCode) == "" {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "tier_code required")
		return
	}
	idempotency := newIdempotencyKey()
	// gcid is the tenant-admin who initiated the subscription;
	// chora-payments persists it as admin_gcid on the
	// TenantAddonPurchase row.
	resp, err := h.client.CreateTenantAddonCheckoutSession(r.Context(), clients.TenantAddonCheckoutRequest{
		IdempotencyKey: idempotency,
		TenantID:       tenantID,
		AdminGCID:      gcid,
		AddonPlanID:    strings.TrimSpace(body.AddonPlanID),
		AddonCode:      strings.TrimSpace(body.AddonCode),
		TierCode:       strings.ToLower(strings.TrimSpace(body.TierCode)),
		AmountCents:    body.AmountCents,
		Currency:       strings.ToUpper(strings.TrimSpace(body.Currency)),
		SuccessURL:     strings.TrimSpace(body.SuccessURL),
		CancelURL:      strings.TrimSpace(body.CancelURL),
	})
	writeCheckoutResp(w, resp, err)
}

// -----------------------------------------------------------------------------
// Shared helpers
// -----------------------------------------------------------------------------

// requireCheckoutPOST enforces POST. Returns false on rejection (handler
// already wrote a 405 envelope).
func requireCheckoutPOST(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "POST only")
		return false
	}
	return true
}

// requireMeshIdentity returns (tenant_id, learner_gcid, ok). RequireChoraSessionJWT
// stamps the mesh claims onto the context before this handler runs; if either
// claim is missing the gate is misconfigured and we 401 — this is the
// defence-in-depth case the JWT middleware doc references.
func requireMeshIdentity(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	mc, ok := MeshClaimsFromContext(r.Context())
	if !ok || mc == nil {
		// Fall back to the raw ChoraSession claims (the JWT middleware was
		// stubbed in tests via InjectChoraSessionClaimsForTest).
		if cs, csOK := ChoraSessionClaimsFromContext(r.Context()); csOK && cs != nil {
			tid := strings.TrimSpace(cs.TenantID)
			gcid := strings.TrimSpace(cs.GCID)
			if tid != "" && gcid != "" {
				return tid, gcid, true
			}
		}
		writeError(w, http.StatusUnauthorized, "GATEWAY_UNAUTHENTICATED", "mesh claims missing")
		return "", "", false
	}
	tid := strings.TrimSpace(mc.TenantID)
	gcid := strings.TrimSpace(mc.GCID)
	if tid == "" || gcid == "" {
		writeError(w, http.StatusUnauthorized, "GATEWAY_UNAUTHENTICATED", "tenant_id or gcid missing from session")
		return "", "", false
	}
	return tid, gcid, true
}

// decodeCheckoutBody decodes a 1 MiB-capped JSON body. On any decode failure
// writes a 400 envelope and returns false.
func decodeCheckoutBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body == nil {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "empty body")
		return false
	}
	const maxBytes = 1 << 20
	limited := io.LimitReader(r.Body, maxBytes)
	dec := json.NewDecoder(limited)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "malformed json: "+err.Error())
		return false
	}
	return true
}

// requireAmountAndCurrency enforces the two universal body fields. Returns
// false on rejection (handler already wrote a 400 envelope).
func requireAmountAndCurrency(w http.ResponseWriter, amountCents int64, currency string) bool {
	if amountCents <= 0 {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "amount_cents must be > 0")
		return false
	}
	if c := strings.TrimSpace(currency); len(c) != 3 {
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", "currency must be a 3-letter ISO code")
		return false
	}
	return true
}

// parseOptionalRFC3339 parses an RFC3339 timestamp. Empty input returns the
// zero time + nil error so the caller skips the field on the wire.
func parseOptionalRFC3339(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// isKnownBillingPeriod gates the FE-supplied period before forwarding to gRPC.
// The downstream rejects UNSPECIFIED with InvalidArgument; we surface 400
// at the BFF for a clearer FE error.
func isKnownBillingPeriod(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "monthly", "month", "annually", "annual", "yearly", "year":
		return true
	}
	return false
}

// newIdempotencyKey mints a fresh UUIDv7 idempotency key for each inbound
// request. This is intentional — chora-payments uses the key to dedupe
// in-flight Stripe Checkout Session mints; per browser request we want a
// fresh key so re-submits proceed (browsers can't replay the same
// /checkout/* POST without a user click, by design).
//
// uuid.NewV7 errors only when the entropy source does, never on a clock
// backstep, and uuid.NewString panics on that same failure, so there is no
// v4 fallback that would let the request proceed.
func newIdempotencyKey() string {
	return uuid.Must(uuid.NewV7()).String()
}

// writeCheckoutResp emits the response envelope on success, or maps the
// gRPC error to the canonical HTTP status code on failure.
func writeCheckoutResp(w http.ResponseWriter, resp clients.CheckoutResponse, err error) {
	if err != nil {
		writeCheckoutErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// writeCheckoutErr maps a gRPC status code to the canonical HTTP status +
// error envelope. Unknown / non-grpc errors collapse to 500.
func writeCheckoutErr(w http.ResponseWriter, err error) {
	st, ok := status.FromError(unwrap(err))
	if !ok {
		writeError(w, http.StatusInternalServerError, "GATEWAY_UPSTREAM_ERROR", err.Error())
		return
	}
	switch st.Code() {
	case codes.NotFound:
		writeError(w, http.StatusNotFound, "GATEWAY_NOT_FOUND", st.Message())
	case codes.InvalidArgument:
		writeError(w, http.StatusBadRequest, "GATEWAY_INVALID_REQUEST", st.Message())
	case codes.PermissionDenied:
		writeError(w, http.StatusForbidden, "GATEWAY_FORBIDDEN", st.Message())
	case codes.FailedPrecondition:
		writeError(w, http.StatusConflict, "GATEWAY_CONFLICT", st.Message())
	case codes.Unauthenticated:
		writeError(w, http.StatusUnauthorized, "GATEWAY_UNAUTHENTICATED", st.Message())
	case codes.Unavailable, codes.DeadlineExceeded:
		writeError(w, http.StatusServiceUnavailable, "GATEWAY_UPSTREAM_UNAVAILABLE", st.Message())
	default:
		writeError(w, http.StatusInternalServerError, "GATEWAY_UPSTREAM_ERROR", st.Message())
	}
}

// unwrap pulls the deepest wrapped error so status.FromError(...) can see a
// gRPC status that was wrapped by fmt.Errorf("...: %w", err) in the client.
func unwrap(err error) error {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err
		}
		err = next
	}
}
