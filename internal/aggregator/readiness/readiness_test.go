package readiness

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubDownstream serves canned JSON per path and records what was asked.
type stubDownstream struct {
	byPath map[string]stubResp
	seen   []string
}

type stubResp struct {
	status int
	body   string
}

func newStub(t *testing.T, byPath map[string]stubResp) (*httptest.Server, *stubDownstream) {
	t.Helper()
	d := &stubDownstream{byPath: byPath}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.seen = append(d.seen, r.URL.Path)
		resp, ok := d.byPath[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	}))
	t.Cleanup(srv.Close)
	return srv, d
}

// healthy is the everything-is-fine fixture.
// healthy takes the testing handle because the entitlements fixture is
// rendered from entitlementRow (entitlements_wire_test.go) rather than
// hand-written, so a stub can no longer drift away from the wire.
func healthy(t *testing.T) map[string]stubResp {
	t.Helper()
	return map[string]stubResp{
		"/api/v1/tenancy/tenants/current/hierarchy": {200, `{"is_parent":true,"children":[{"tenant_id":"t-1","tenant_name":"Northwind","user_count":3,"atom_count":24}]}`},
		"/v1/tenants/me":                {200, `{"tenant_id":"t-1","branding":{"primary_color_hex":"#0f766e"},"wizard_completed_at":"2026-09-01T10:00:00Z"}`},
		"/v1/tenants/me/idp-providers":  {200, `{"items":[{"provider_type":"oidc"}]}`},
		"/api/tenants/t-1/entitlements": {200, entitlementsBody(t, "core", "tms")},
		"/api/v1/admin/tenant-members":  {200, `{"items":[{"gcid":"g-1","roles":["admin"]},{"gcid":"g-2","roles":["learner"]}]}`},
	}
}

func build(t *testing.T, byPath map[string]stubResp) readinessDTO {
	t.Helper()
	srv, _ := newStub(t, byPath)
	agg := New(Config{TenancyURL: srv.URL, IdentityURL: srv.URL})
	if agg == nil {
		t.Fatal("New returned nil for a configured aggregator")
	}
	resp, err := agg.GetReadiness(context.Background(), AuthCtx{TenantID: "t-1", GCID: "g-1"})
	if err != nil {
		t.Fatalf("GetReadiness: %v", err)
	}
	var dto readinessDTO
	if uerr := json.Unmarshal(resp.Body, &dto); uerr != nil {
		t.Fatalf("decode: %v (body %s)", uerr, resp.Body)
	}
	return dto
}

func rowByKey(t *testing.T, dto readinessDTO, key string) readinessRowDTO {
	t.Helper()
	for _, r := range dto.Rows {
		if r.Key == key {
			return r
		}
	}
	t.Fatalf("no row %q in %+v", key, dto.Rows)
	return readinessRowDTO{}
}

func TestReadiness_EveryRowIsPresentEvenWhenHealthy(t *testing.T) {
	// The screen renders a fixed set of checks. A row that vanishes when it
	// passes would make the list length mean something, and a reader could not
	// tell "checked and fine" from "not checked at all".
	dto := build(t, healthy(t))
	for _, key := range []string{"organisation", "branding", "features", "signIn", "administrators", "content", "setup", "billing"} {
		rowByKey(t, dto, key)
	}
	if len(dto.Rows) != 8 {
		t.Fatalf("got %d rows, want 8: %+v", len(dto.Rows), dto.Rows)
	}
}

func TestReadiness_HealthyRowsReportOk(t *testing.T) {
	dto := build(t, healthy(t))
	for _, key := range []string{"organisation", "branding", "signIn", "administrators", "content", "setup"} {
		if got := rowByKey(t, dto, key).Status; got != StatusOK {
			t.Errorf("row %s = %q, want ok", key, got)
		}
	}
}

func TestReadiness_BillingIsAlwaysUnknownWithAReason(t *testing.T) {
	// The one row that CANNOT be checked. Invoices and payment methods are
	// in-memory repos in production, so any answer would be invented. Green
	// would be a lie and red would be a false alarm; unknown with a reason is
	// the only honest third option, and it must be a SERVER fact so the
	// screen renders it rather than guessing.
	row := rowByKey(t, build(t, healthy(t)), "billing")
	if row.Status != StatusUnknown {
		t.Fatalf("billing = %q, want unknown", row.Status)
	}
	if strings.TrimSpace(row.Reason) == "" {
		t.Fatal("an unknown row must carry a reason; a bare unknown tells an operator nothing")
	}
	if !strings.Contains(strings.ToLower(row.Reason), "memory") {
		t.Errorf("the reason must name WHY it cannot be checked, got %q", row.Reason)
	}
}

func TestReadiness_FeaturesIsOkOnlyBeyondTheBaseline(t *testing.T) {
	// The baseline plan is granted to EVERY tenant at creation, so its
	// presence alone proves nothing about the chosen add-ons. Counting it
	// would make every organisation look entitled the moment it exists.
	row := rowByKey(t, build(t, healthy(t)), "features")
	if row.Status != StatusOK {
		t.Fatalf("features = %q, want ok when a non-baseline entitlement exists", row.Status)
	}
	if row.Count == nil || *row.Count != 1 {
		t.Errorf("count = %v, want 1 (core excluded)", row.Count)
	}
}

func TestReadiness_FeaturesBaselineOnlyIsDeclaredNotYetEntitled(t *testing.T) {
	// The failure direction is deliberate: absent evidence must UNDER-claim.
	// Telling an operator a plan is granted when it is not is discovered by a
	// learner who cannot use the feature; the reverse costs a minute.
	for _, body := range []string{
		entitlementsBody(t, "core"), // baseline under its stored name
		entitlementsBody(t, "base"), // and under the catalogue name
		entitlementsBody(t),         // nothing at all
	} {
		f := healthy(t)
		f["/api/tenants/t-1/entitlements"] = stubResp{200, body}
		row := rowByKey(t, build(t, f), "features")
		if row.Status != StatusAttention {
			t.Errorf("body %s: features = %q, want attention", body, row.Status)
		}
		if !strings.Contains(strings.ToLower(row.Reason), "declared") {
			t.Errorf("body %s: reason must say declared, not yet entitled; got %q", body, row.Reason)
		}
	}
}

func TestReadiness_FeaturesDeclaresItsSourceAsAKnownGap(t *testing.T) {
	// The row must say which source answered rather than imply it read
	// add_on_subscriptions directly.
	//
	// AMENDED 2026-09-02. chora-tenancy dfab160a3 NARROWED this gap: the read
	// now falls through to the durable table when the registry has never seen
	// the tenant. It did not close it. A tenant the registry hydrated at boot
	// is still answered from that snapshot, so a grant made by another pod
	// afterwards stays invisible. The gap string has to carry both halves,
	// because the old blanket wording is now false and dropping it would
	// claim a durability this read still does not have for every tenant.
	dto := build(t, healthy(t))
	if dto.KnownGaps["features"] == "" {
		t.Fatal("the features row must declare its source in known_gaps")
	}
	gap := dto.KnownGaps["features"]
	if !strings.Contains(gap, "registry") {
		t.Errorf("the gap must name the registry, got %q", gap)
	}
	if !strings.Contains(gap, "add_on_subscriptions") {
		t.Errorf("the gap must name the durable table it now falls through to, got %q", gap)
	}
}

func TestReadiness_FeaturesReadsATenantTheRegistryNeverHydrated(t *testing.T) {
	// Positive control for the dfab160a3 fall-through path, which reaches this
	// row with a DIFFERENT baseline spelling than the registry path does.
	//
	// The bootstrap saga writes the legacy "core" into add_on_subscriptions,
	// and the boot hydrator maps it to the canonical "base" on the way INTO
	// the registry. The durable table never had that applied, so the
	// fall-through translates on the way out and this row can be handed
	// either name for the same always-on plan depending on whether the
	// serving pod had hydrated the tenant.
	//
	// So the invariant under test is that the baseline is excluded under BOTH
	// names and only the real add-on is counted. Narrowing isBaselineCode to
	// "core" alone would still pass every other features test here and would
	// make a never-hydrated tenant report its baseline as a granted add-on,
	// which is the over-claim direction the row exists to avoid.
	f := healthy(t)
	f["/api/tenants/t-1/entitlements"] = stubResp{
		200, entitlementsBody(t, "base", "tms"),
	}
	row := rowByKey(t, build(t, f), "features")
	if row.Status != StatusOK {
		t.Fatalf("features = %q, want ok for a never-hydrated tenant holding a real add-on", row.Status)
	}
	if row.Count == nil || *row.Count != 1 {
		t.Errorf("count = %v, want 1 (the translated baseline excluded)", row.Count)
	}
}

func TestReadiness_EmptyInstanceFlagsTheRowsThatNeedAction(t *testing.T) {
	f := healthy(t)
	f["/api/v1/tenancy/tenants/current/hierarchy"] = stubResp{200, `{"is_parent":false,"children":[]}`}
	f["/v1/tenants/me/idp-providers"] = stubResp{200, `{"items":[]}`}
	f["/api/v1/admin/tenant-members"] = stubResp{200, `{"items":[]}`}
	dto := build(t, f)

	for _, key := range []string{"organisation", "signIn", "administrators", "content"} {
		if got := rowByKey(t, dto, key).Status; got != StatusAttention {
			t.Errorf("row %s on an empty instance = %q, want attention", key, got)
		}
	}
}

func TestReadiness_ADownSourceIsUnknownNotAWholeScreen500(t *testing.T) {
	// Fail-soft per row. One dead source must never blank the screen: the
	// operator loses every other answer for no reason, and an outage in a
	// service they were not asking about looks like a broken page.
	f := healthy(t)
	f["/v1/tenants/me/idp-providers"] = stubResp{500, `{"error":"boom"}`}
	dto := build(t, f)

	if got := rowByKey(t, dto, "signIn").Status; got != StatusUnknown {
		t.Errorf("a 5xx source must render the row unknown, got %q", got)
	}
	if !dto.Partial {
		t.Error("a failed part must set partial")
	}
	if dto.PartErrors["signIn"] == "" {
		t.Error("the failed part must be named in part_errors")
	}
	// The positive control: every other row still answered.
	if got := rowByKey(t, dto, "administrators").Status; got != StatusOK {
		t.Errorf("an unrelated row = %q, want ok; one dead source must not blank the rest", got)
	}
}

func TestReadiness_UnconfiguredSourceIsAKnownGapNotAFailure(t *testing.T) {
	// A downstream URL that is not configured is a DEPLOYMENT fact, not a
	// per-request failure, and the two need different fixes. Conflating them
	// sends an operator hunting an outage that is really a missing env var.
	agg := New(Config{TenancyURL: "", IdentityURL: ""})
	if agg != nil {
		t.Fatal("New must return nil when NO downstream is configured, so the route can be skipped")
	}
}

func TestReadiness_PartialIsFalseWhenEverythingAnswered(t *testing.T) {
	dto := build(t, healthy(t))
	if dto.Partial {
		t.Errorf("partial = true with no failures; part_errors = %v", dto.PartErrors)
	}
	if len(dto.PartErrors) != 0 {
		t.Errorf("part_errors = %v, want empty", dto.PartErrors)
	}
}

func TestReadiness_NextActionNamesTheFirstThingToDo(t *testing.T) {
	// The screen's whole job is to say what to do next. On an empty instance
	// that is creating an organisation, not "six checks failed".
	f := healthy(t)
	f["/api/v1/tenancy/tenants/current/hierarchy"] = stubResp{200, `{"is_parent":false,"children":[]}`}
	dto := build(t, f)
	if strings.TrimSpace(dto.NextAction) == "" {
		t.Fatal("an instance with work to do must name a next action")
	}
	if !strings.Contains(strings.ToLower(dto.NextAction), "organisation") {
		t.Errorf("next action = %q, want the organisation row (the first blocker)", dto.NextAction)
	}
}

func TestReadiness_NextActionIsEmptyWhenNothingNeedsAttention(t *testing.T) {
	// Billing is unknown even on a fully healthy instance, and unknown is NOT
	// a call to action: an operator cannot act on a check we cannot run. This
	// is the assertion that keeps `unknown` from silently behaving like a
	// third kind of red.
	dto := build(t, healthy(t))
	if dto.NextAction != "" {
		t.Errorf("next action = %q, want empty; unknown must not be treated as actionable", dto.NextAction)
	}
}

func TestReadiness_CountsRideOnTheRowsThatHaveThem(t *testing.T) {
	dto := build(t, healthy(t))
	// Count is a POINTER so a row that counts nothing omits the key entirely
	// and a zero always means zero.
	content := rowByKey(t, dto, "content").Count
	if content == nil || *content != 24 {
		t.Errorf("content count = %v, want 24 atoms from the hierarchy projection", content)
	}
	admins := rowByKey(t, dto, "administrators").Count
	if admins == nil || *admins != 1 {
		t.Errorf("administrators count = %v, want 1 (only the admin role counts)", admins)
	}
	// The billing row counts nothing, so it must carry no count at all.
	if c := rowByKey(t, dto, "billing").Count; c != nil {
		t.Errorf("billing count = %v, want omitted", c)
	}
}
