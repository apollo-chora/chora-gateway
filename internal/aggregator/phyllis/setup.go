// setup.go — Setup Wizard Phase C BFF aggregator wrapper (CHO-1682).
//
// Surfaces `POST /api/v1/tenants/setup` for the H+ Setup-Tenant wizard's
// step 4 Apply. THIS IS THE FIRST MULTI-DOWNSTREAM PHYLLIS AGGREGATOR —
// every other route in this package proxies to ONE downstream service;
// SetupTenant fans out to TWO and sequences them so a failed first call
// short-circuits before the second.
//
// Fan-out contract:
//
//  1. chora-identity POST /api/v1/tenants/me/idp-providers
//     (request body = the wizard's `identity` slice unwrapped)
//  2. on identity 2xx, chora-tenancy PATCH /api/v1/tenants/me/finish-setup
//     (no body — set-once stamp keyed by JWT-stamped X-Tenant-Id)
//
// Identity-failure rule: when identity returns 4xx/5xx, tenancy MUST NOT
// be called. A failed identity write must NEVER leave a tenant marked as
// wizard-complete.
//
// Tenancy-failure-after-identity-success rule: when identity succeeded but
// tenancy fails, surface a 502 that names tenancy as the failing leg. The
// caller may safely retry the whole call — both downstream endpoints are
// idempotent (identity on (tenant_id, provider_type), tenancy set-once).
//
// Sibling stories:
//   - CHO-1405 — parent Setup Wizard
//   - CHO-1655 — Phase A, branding (single-downstream proxy template:
//     branding.go)
//   - CHO-1664 — Phase B, add-ons (single-downstream template: addons.go)
//   - CHO-1682 — Phase C, this file (Apply / finish-setup fan-out)
package phyllis

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// setupRequestEnvelope mirrors bff-gateway.yaml::SetupTenantRequest. The
// aggregator only consumes the `identity` slice — branding + add-ons
// were already persisted by their own wizard steps and the FE keeps
// re-sending them for diagnostic parity (`additionalProperties: true`).
type setupRequestEnvelope struct {
	Identity json.RawMessage `json:"identity"`
}

// setupResponseEnvelope mirrors bff-gateway.yaml::SetupTenantResponse —
// the two downstream payloads stitched into one shape.
type setupResponseEnvelope struct {
	IdpProvider json.RawMessage `json:"idp_provider"`
	FinishSetup json.RawMessage `json:"finish_setup"`
}

// SetupTenant fans out the Setup Wizard step 4 Apply request:
//
//  1. POST chora-identity /api/v1/tenants/me/idp-providers with the
//     unwrapped identity slice.
//  2. PATCH chora-tenancy /api/v1/tenants/me/finish-setup (no body) IFF
//     identity returned 2xx.
//
// Both downstream calls pass through phyllis.call() so AuthCtx headers
// (Authorization, gcid, X-Tenant-Id, traceparent) are stamped uniformly.
func (a *Aggregator) SetupTenant(ctx context.Context, auth AuthCtx, body []byte) (Response, error) {
	return a.withBudget(ctx, func(c context.Context) Response {
		// Decode the FE envelope.
		var env setupRequestEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			return errResp(http.StatusBadRequest, "GATEWAY_INVALID_BODY",
				"invalid setup request: "+err.Error())
		}
		if len(env.Identity) == 0 || string(env.Identity) == "null" {
			return errResp(http.StatusBadRequest, "GATEWAY_MISSING_IDENTITY_SLICE",
				"setup request `identity` slice is required")
		}

		// 1) Identity — hard-coded canonical path (per feedback_bff_aggregator_path_test).
		identityResp := classify(a.call(c, http.MethodPost,
			a.cfg.IdentityURL+"/api/v1/tenants/me/idp-providers",
			env.Identity, auth))
		if identityResp.Status >= 400 {
			// Pass identity's status + body straight through — the FE
			// surfaces the precise error code (unknown_provider_type,
			// invalid_input, etc) from the response.
			return identityResp
		}

		// 2) Tenancy — only on identity success. Canonical path hard-coded.
		tenancyResp := classify(a.call(c, http.MethodPatch,
			a.cfg.TenancyURL+"/api/v1/tenants/me/finish-setup",
			nil, auth))
		if tenancyResp.Status >= 400 {
			// Identity succeeded but tenancy failed. The identity row IS
			// already persisted; surface a 502 that names tenancy so the
			// FE can decide whether to retry (safe — both downstreams
			// are idempotent).
			return errResp(http.StatusBadGateway, "GATEWAY_UPSTREAM_TENANCY",
				fmt.Sprintf("identity succeeded; tenancy returned %d (retry-safe — both endpoints idempotent)", tenancyResp.Status))
		}

		// 3) Combine the two payloads.
		merged := setupResponseEnvelope{
			IdpProvider: identityResp.Body,
			FinishSetup: tenancyResp.Body,
		}
		body, err := json.Marshal(merged)
		if err != nil {
			return errResp(http.StatusInternalServerError, "GATEWAY_RESPONSE_MARSHAL",
				"failed to marshal combined response: "+err.Error())
		}
		return Response{
			Status: http.StatusOK,
			Body:   body,
			Headers: http.Header{
				"Content-Type": []string{"application/json; charset=utf-8"},
			},
		}
	}), nil
}
