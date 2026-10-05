// tenancy_admin_addons_test.go: E1 (UX refactor wave W2), the gateway half of
// granting chosen add-ons at creation.
//
// The gateway validates SHAPE and chora-tenancy owns membership in the
// catalogue. That split is deliberate. The catalogue is data, seeded by
// migration 0035 and reconciled at tenancy boot; a hard-coded list here would
// be inline config that drifts the first time somebody adds an add-on, and the
// existing `validHostingModes` map is not a precedent for it, because a hosting
// mode is a closed proto enum while an add-on is a row. So this layer rejects
// what it can see is wrong (blank, over-long, absurdly many) and lets an
// unrecognised code come back as the 400 chora-tenancy already produces.
//
// The operator gate is asserted again on the new field, not because the field
// changes it, but because a new field on an operator-only route is exactly
// where a fresh path in gets introduced by accident.
package httpadapter

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
)

// bodyWithAddOns builds a valid create body plus the add_on_codes fragment.
func bodyWithAddOns(fragment string) string {
	base := `{"parent_tenant_id":"` + stParent + `","display_name":"Northwind Academy","owner_gcid":"` + stOwner + `"`
	if fragment == "" {
		return base + `}`
	}
	return base + `,` + fragment + `}`
}

func TestSubTenant_AddOnCodes_ForwardedToTheClient(t *testing.T) {
	fake := &fakeSubTenantCreator{resp: clients.TenantDTO{TenantID: stChild}}
	rec := stServeWith(t, fake, stOperatorClaims(), http.MethodPost, subTenantsTarget,
		bodyWithAddOns(`"add_on_codes":["tms","cms"]`))

	if rec.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := strings.Join(fake.lastParams.AddOnCodes, ","), "tms,cms"; got != want {
		t.Fatalf("codes reaching the client = %q, want %q; a dropped field leaves the whole "+
			"feature looking implemented and doing nothing", got, want)
	}
}

func TestSubTenant_AddOnCodes_AreOptional(t *testing.T) {
	// The regression fence: every existing caller omits the field.
	fake := &fakeSubTenantCreator{resp: clients.TenantDTO{TenantID: stChild}}
	rec := stServeWith(t, fake, stOperatorClaims(), http.MethodPost, subTenantsTarget, bodyWithAddOns(""))
	if rec.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if len(fake.lastParams.AddOnCodes) != 0 {
		t.Fatalf("AddOnCodes = %v, want none", fake.lastParams.AddOnCodes)
	}
}

func TestSubTenant_AddOnCodes_MalformedIsRefusedBeforeTheCall(t *testing.T) {
	for _, tc := range []struct{ name, fragment string }{
		{"blank entry", `"add_on_codes":["tms",""]`},
		{"whitespace entry", `"add_on_codes":["   "]`},
		{"over-long entry", `"add_on_codes":["` + strings.Repeat("x", 65) + `"]`},
		{"absurdly many", `"add_on_codes":[` + strings.TrimSuffix(strings.Repeat(`"x",`, 65), ",") + `]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSubTenantCreator{}
			rec := stServeWith(t, fake, stOperatorClaims(), http.MethodPost, subTenantsTarget,
				bodyWithAddOns(tc.fragment))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if fake.called {
				t.Errorf("a malformed body must be refused BEFORE the outbound gRPC call")
			}
		})
	}
}

func TestSubTenant_AddOnCodes_DoNotBypassTheOperatorGate(t *testing.T) {
	fake := &fakeSubTenantCreator{}
	rec := stServeWith(t, fake, stTenantAdminClaims(), http.MethodPost, subTenantsTarget,
		bodyWithAddOns(`"add_on_codes":["tms"]`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if fake.called {
		t.Errorf("a non-operator must never reach the outbound gRPC call")
	}
}

// The unknown-code refusal comes back from chora-tenancy as INVALID_ARGUMENT
// and must reach the operator as a 400 naming the code, not as the 409 the
// parent-invariant rejection uses. Asserted here because writeSubTenantErr is
// where the two would be conflated.
func TestSubTenant_UnknownAddOnCode_SurfacesAs400NamingTheCode(t *testing.T) {
	fake := &fakeSubTenantCreator{
		err: status.Error(codes.InvalidArgument, `unknown add-on code "not-a-real-add-on"`),
	}
	rec := stServeWith(t, fake, stOperatorClaims(), http.MethodPost, subTenantsTarget,
		bodyWithAddOns(`"add_on_codes":["not-a-real-add-on"]`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	// The gateway envelope nests under `error` (handler.go errEnvelope), which
	// a flat decode reads as an empty message and would pass silently.
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if !strings.Contains(body.Error.Message, "not-a-real-add-on") {
		t.Errorf("message %q must name the offending code, or the operator cannot tell which "+
			"checkbox was wrong", body.Error.Message)
	}
}
