package readiness

// entitlements_wire_test.go pins the features row to the wire the GRANT
// endpoint actually sends, and to the discrimination between a row that
// carries no code and a response that carries no code FIELD.
//
// Why this file exists. From E2 part 1 (ebe5bf734, 2026-09-02) until the sha
// that adds it, entitlementsWire decoded `add_on_code`. That spelling belongs
// to the setup-wizard INTENT endpoint (`GET /api/v1/tenants/me/addons`,
// services/chora-tenancy/internal/adapter/http/me_addons_handler.go), which
// also uses a different envelope (`subscriptions`, not `items`). The GRANT
// endpoint this row reads emits `addon_code`
// (services/chora-tenancy/internal/adapter/http/handlers.go, entitlementDTO,
// whose own comment calls the name a contract pin for the frontend; chora-web
// pins the same spelling in core/services/feature-flags.model.ts).
//
// Nothing caught it because every stub in readiness_test.go carried the same
// wrong spelling, so the tests agreed with the code and neither agreed with
// the wire. The consequence was not a false green but a permanently false
// row: every item decoded to an empty code, isBaselineCode treated empty as
// the baseline, and the features row reported "Only the baseline plan" with
// count 0 for every tenant on /h/ready and /h/go-live, whatever was granted.
//
// The guard against a repeat is the fixture below: every entitlements stub in
// this package builds from entitlementRow, and TestEntitlementFixtureMirrors-
// TheTenancyDTO pins its key set against the DTO's, so a stub can no longer
// drift away from the wire on its own.

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// entitlementDTOKeys is the key set chora-tenancy's entitlementDTO emits for
// one ACTIVE row of GET /api/tenants/{id}/entitlements. Source:
// services/chora-tenancy/internal/adapter/http/handlers.go, func entitlementDTO.
//
// `cancelled_at` is deliberately absent: the DTO adds it only when the row
// carries a cancellation, and this fixture is an active grant.
var entitlementDTOKeys = []string{
	"activated_at",
	"addon_code",
	"addon_id",
	"id",
	"monthly_price_cents_snapshot",
	"status",
	"tenant_id",
	"updated_at",
}

// entitlementRow renders one active grant in the exact shape the endpoint
// sends. A caller passing "" gets a row whose addon_code is PRESENT and empty,
// which is a legacy row predating tenancy migration 0021, not a wire error.
func entitlementRow(code string) map[string]any {
	return map[string]any{
		"id":                           "01990000-0000-7000-8000-00000000000a",
		"tenant_id":                    "t-1",
		"addon_id":                     "01990000-0000-7000-8000-00000000000b",
		"addon_code":                   code,
		"monthly_price_cents_snapshot": 0,
		"status":                       "active",
		"activated_at":                 "2026-09-01T10:00:00Z",
		"updated_at":                   "2026-09-01T10:00:00Z",
	}
}

// entitlementsBody renders the grant endpoint's full envelope for the given
// codes. Use it for EVERY entitlements stub in this package.
func entitlementsBody(t *testing.T, codes ...string) string {
	t.Helper()
	rows := make([]map[string]any, 0, len(codes))
	for _, c := range codes {
		rows = append(rows, entitlementRow(c))
	}
	b, err := json.Marshal(map[string]any{"items": rows, "total": len(rows)})
	if err != nil {
		t.Fatalf("marshal entitlements fixture: %v", err)
	}
	return string(b)
}

func TestEntitlementFixtureMirrorsTheTenancyDTO(t *testing.T) {
	// The fixture is only a guard while it matches the DTO it claims to
	// mirror. Editing entitlementRow without editing entitlementDTOKeys (or
	// the reverse) fails here rather than silently reintroducing the drift
	// this file was written for.
	got := make([]string, 0, len(entitlementDTOKeys))
	for k := range entitlementRow("tms") {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(entitlementDTOKeys, ",") {
		t.Fatalf("fixture keys = %v, want the entitlementDTO key set %v "+
			"(source: chora-tenancy internal/adapter/http/handlers.go entitlementDTO)",
			got, entitlementDTOKeys)
	}
}

func TestReadiness_FeaturesReadsTheGrantEndpointsFieldName(t *testing.T) {
	// The row reads GET /api/tenants/{id}/entitlements, whose rows carry
	// `addon_code`. Reading any other spelling makes every code decode empty,
	// which this row cannot distinguish from a baseline-only tenant.
	f := healthy(t)
	f["/api/tenants/t-1/entitlements"] = stubResp{200, entitlementsBody(t, "core", "tms")}
	row := rowByKey(t, build(t, f), "features")
	if row.Status != StatusOK {
		t.Fatalf("features = %q (reason %q), want ok: the grant carries a real add-on beyond the baseline",
			row.Status, row.Reason)
	}
	if row.Count == nil || *row.Count != 1 {
		t.Errorf("count = %v, want 1 (the baseline excluded)", row.Count)
	}
}

func TestReadiness_FeaturesRefusesTheIntentEndpointsEnvelope(t *testing.T) {
	// The setup-wizard selection endpoint sends `add_on_code` rows. If those
	// ever reach this row, no item carries the field this endpoint is
	// contracted to send, and the honest answer is that the row cannot say
	// what the plan includes. Reporting "only the baseline plan" instead is
	// what the pre-fix code did, and it is a statement of fact the response
	// does not support.
	f := healthy(t)
	f["/api/tenants/t-1/entitlements"] = stubResp{
		200, `{"items":[{"add_on_code":"tms"},{"add_on_code":"cms"}]}`,
	}
	dto := build(t, f)
	row := rowByKey(t, dto, "features")
	if row.Status != StatusUnknown {
		t.Fatalf("features = %q, want unknown for a response carrying no addon_code field", row.Status)
	}
	if !strings.Contains(row.Reason, "addon_code") {
		t.Errorf("the reason must name the field that was missing, got %q", row.Reason)
	}
	if row.Count != nil {
		t.Errorf("count = %v, want absent: a row that cannot read the codes must not count them", *row.Count)
	}
	// A wire shape we do not recognise is an ERROR, not an empty result, so
	// it lands in part_errors and the response reports itself partial.
	if dto.PartErrors["features"] == "" {
		t.Error("an unexpected envelope must be recorded in part_errors")
	}
	if !dto.Partial {
		t.Error("part_errors is non-empty, so the response must report partial")
	}
}

func TestReadiness_FeaturesUnknownWhenAGrantIsMissingItsCatalogueCode(t *testing.T) {
	// Distinct from the case above and deliberately so. `addon_code: ""` is a
	// documented data condition, not a wire error: chora-web's contract note
	// records that legacy add_ons rows predating tenancy migration 0021 have
	// no code. The response is well formed, so nothing failed and nothing goes
	// in part_errors, but the row still cannot name what is granted, and
	// counting the row as the baseline would under-claim a real add-on.
	f := healthy(t)
	f["/api/tenants/t-1/entitlements"] = stubResp{200, entitlementsBody(t, "core", "")}
	dto := build(t, f)
	row := rowByKey(t, dto, "features")
	if row.Status != StatusUnknown {
		t.Fatalf("features = %q, want unknown when a granted row carries no catalogue code", row.Status)
	}
	if strings.Contains(strings.ToLower(row.Reason), "baseline") {
		t.Errorf("an uncoded grant is not the baseline; reason must not claim it is, got %q", row.Reason)
	}
	if dto.PartErrors["features"] != "" {
		t.Errorf("a well-formed response is not a part error, got %q", dto.PartErrors["features"])
	}
}

func TestReadiness_FeaturesStillCountsWhenNoRowIsUncoded(t *testing.T) {
	// Positive control for the two unknown branches above: they must fire on
	// their own condition only. A clean grant with two real add-ons still
	// counts both and still reports ok.
	f := healthy(t)
	f["/api/tenants/t-1/entitlements"] = stubResp{200, entitlementsBody(t, "base", "tms", "cms")}
	dto := build(t, f)
	row := rowByKey(t, dto, "features")
	if row.Status != StatusOK {
		t.Fatalf("features = %q (reason %q), want ok", row.Status, row.Reason)
	}
	if row.Count == nil || *row.Count != 2 {
		t.Errorf("count = %v, want 2", row.Count)
	}
	if dto.PartErrors["features"] != "" {
		t.Errorf("nothing failed, got part error %q", dto.PartErrors["features"])
	}
}
