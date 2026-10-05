package httpadapter_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
)

func testManaPacks(t *testing.T) httpadapter.ManaPackCatalogue {
	t.Helper()
	cat, err := httpadapter.ParseManaPackCatalogue(
		`[{"sku":"mana_pack_5000","mana_units":5000,"amount_cents":799,"currency":"usd"}]`)
	if err != nil {
		t.Fatalf("ParseManaPackCatalogue: %v", err)
	}
	return cat
}

func newUserManaHandler(t *testing.T, fake httpadapter.PaymentsCheckoutClient, packs httpadapter.ManaPackCatalogue) http.Handler {
	t.Helper()
	h, err := httpadapter.NewPaymentsCheckoutHandler(fake)
	if err != nil {
		t.Fatalf("NewPaymentsCheckoutHandler: %v", err)
	}
	h = h.WithManaPacks(packs)
	return httpadapter.NewPaymentsCheckoutMux(h)
}

func TestUserManaCheckout_HappyPath_ServerResolvedPrice(t *testing.T) {
	fake := &fakeCheckoutClient{userManaResp: clients.CheckoutResponse{
		PurchaseID: "umt-1", StripeCheckoutURL: "https://stripe.test/cs_1", State: "checkout_started",
	}}
	h := newUserManaHandler(t, fake, testManaPacks(t))

	r := authedReq(t, http.MethodPost, "/api/v1/checkout/user-mana", `{"sku":"mana_pack_5000"}`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// Price + units MUST come from the server catalogue, not the request.
	if fake.userManaReq.ManaUnits != 5000 || fake.userManaReq.AmountCents != 799 || fake.userManaReq.Currency != "USD" {
		t.Fatalf("server-resolved price wrong: units=%d cents=%d cur=%s",
			fake.userManaReq.ManaUnits, fake.userManaReq.AmountCents, fake.userManaReq.Currency)
	}
	// Buyer = mesh learner gcid (NOT admin_gcid).
	if fake.userManaReq.LearnerGCID != "gcid-1" || fake.userManaReq.TenantID != "tenant-1" {
		t.Fatalf("identity wrong: gcid=%s tenant=%s", fake.userManaReq.LearnerGCID, fake.userManaReq.TenantID)
	}
	if fake.userManaReq.IdempotencyKey == "" {
		t.Fatal("idempotency key not minted")
	}
}

func TestUserManaCheckout_UnknownSku_400(t *testing.T) {
	fake := &fakeCheckoutClient{}
	h := newUserManaHandler(t, fake, testManaPacks(t))
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/user-mana", `{"sku":"mana_pack_nope"}`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown sku", w.Code)
	}
}

func TestUserManaCheckout_NoCatalogue_503(t *testing.T) {
	fake := &fakeCheckoutClient{}
	h := newUserManaHandler(t, fake, nil) // catalogue unset
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/user-mana", `{"sku":"mana_pack_5000"}`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when catalogue unconfigured", w.Code)
	}
}

// A client attempting to set its own price (amount_cents) must be rejected —
// the body type has no such field and DisallowUnknownFields is on. Proves the
// umbrella-currency abuse vector is closed at the BFF.
func TestUserManaCheckout_ClientPriceRejected_400(t *testing.T) {
	fake := &fakeCheckoutClient{}
	h := newUserManaHandler(t, fake, testManaPacks(t))
	r := authedReq(t, http.MethodPost, "/api/v1/checkout/user-mana",
		`{"sku":"mana_pack_5000","amount_cents":1,"mana_units":999999}`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown fields rejected)", w.Code)
	}
	if fake.userManaReq.ManaUnits != 0 {
		t.Fatalf("upstream must NOT be called with a client-supplied price")
	}
}
