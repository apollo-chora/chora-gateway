package companionbridge_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/companionbridge"
)

// D6. The H+ pod-catalogue editor saves through chora-tenancy's FULL-entry
// upsert, so it must read the full entry and hand it straight back. These two
// routes are that round-trip.

// The admin read must NOT go through catalogFixup. That fixup exists for the
// LEARNER-facing catalogue: it renames items→skus and converts priceCents into
// priceMicros. An editor that read priceMicros would have to convert back before
// saving, and a unit slip there writes a wrong price to a real SKU. The admin
// read stays a faithful camelCase mirror of the row.
func TestAdminEggEntry_IsAFaithfulMirror_NotTheLearnerCatalogueShape(t *testing.T) {
	var gotPath string
	ten := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"sku":"egg.standard.v1","display_name":"Standard","description":"d",
			"price_cents":999,"currency":"SGD","suggested_focal_atom_id":"",
			"breed_distribution":{"owl":60,"penguin":40},"purchasable":true,
			"is_trial":false,"soft_expiry_days":30,"hard_expiry_days":60}`))
	})
	b := companionbridge.New(companionbridge.Config{TenancyURL: ten.URL, PerCallTimeout: time.Second})
	resp, _ := b.AdminEggEntry(context.Background(), basicAuth(), "egg.standard.v1")
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Status, resp.Body)
	}
	if gotPath != "/api/admin/familiar-eggs/catalog/egg.standard.v1" {
		t.Errorf("upstream path = %q; want the tenancy admin read", gotPath)
	}
	d := dataMap(t, resp.Body)
	if _, ok := d["priceCents"]; !ok {
		t.Errorf("priceCents missing — the editor round-trips it back as price_cents: %v", d)
	}
	if _, leak := d["priceMicros"]; leak {
		t.Errorf("priceMicros present — the learner catalogue fixup must NOT apply here")
	}
	if _, ok := d["breedDistribution"]; !ok {
		t.Errorf("breedDistribution missing: %v", d)
	}
	if _, ok := d["softExpiryDays"]; !ok {
		t.Errorf("softExpiryDays missing — a save would wipe it: %v", d)
	}
}

// The FE speaks camelCase; chora-tenancy's upsert reads snake_case. The bridge
// owns that translation, exactly as it already does for checkout.
func TestAdminEggUpsert_TranslatesCamelToSnakeAndPostsToTheAdminPath(t *testing.T) {
	var gotPath, gotMethod string
	// newStub drains and closes r.Body before this handler runs, so read the
	// forwarded body from the stub's own capture rather than from the request.
	ten := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_, _ = w.Write([]byte(`{"sku":"egg.standard.v1","display_name":"Standard","price_cents":999}`))
	})
	b := companionbridge.New(companionbridge.Config{TenancyURL: ten.URL, PerCallTimeout: time.Second})
	resp, _ := b.AdminEggUpsert(context.Background(), basicAuth(),
		[]byte(`{"sku":"egg.standard.v1","displayName":"Standard","priceCents":999,"softExpiryDays":30,"breedDistribution":{"owl":60,"penguin":40}}`))
	if resp.Status != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Status, resp.Body)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s; want POST", gotMethod)
	}
	if gotPath != "/api/admin/familiar-eggs/catalog" {
		t.Errorf("upstream path = %q; want the tenancy admin upsert", gotPath)
	}
	var sent map[string]any
	if err := json.Unmarshal(ten.lastBody, &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v (%s)", err, ten.lastBody)
	}
	for _, k := range []string{"display_name", "price_cents", "soft_expiry_days", "breed_distribution"} {
		if _, ok := sent[k]; !ok {
			t.Errorf("upstream body missing %q — tenancy reads snake_case: %v", k, sent)
		}
	}
	if _, leak := sent["displayName"]; leak {
		t.Errorf("camelCase key leaked upstream: %v", sent)
	}
}

// A refusal must reach the FE intact rather than as a generic gateway error:
// the editor has to be able to say WHY a distribution was rejected.
func TestAdminEggUpsert_PassesTheValidationRefusalThrough(t *testing.T) {
	ten := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"validation_failed","message":"unknown species: turtle"}}`))
	})
	b := companionbridge.New(companionbridge.Config{TenancyURL: ten.URL, PerCallTimeout: time.Second})
	resp, _ := b.AdminEggUpsert(context.Background(), basicAuth(), []byte(`{"sku":"x"}`))
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 passed through", resp.Status)
	}
	env := decodeJSON(t, resp.Body)
	errObj, ok := env["error"].(map[string]any)
	if !ok {
		t.Fatalf("error envelope missing or double-wrapped: %v", env)
	}
	if msg, _ := errObj["message"].(string); msg != "unknown species: turtle" {
		t.Errorf("refusal message lost: %v", errObj)
	}
}
