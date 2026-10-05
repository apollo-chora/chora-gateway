// handlers_oplus_egress_test.go — O+ external-egress audit read (CHO-2245).
//
// GET /bff/oplus/governance/egress-audit surfaces the external-web egress
// audit slice for O+ (IMDA D2 transparency + D1 accountability). It maps
// upstream status honestly: a governance DENY (4xx) surfaces as the same 4xx
// (never a masked 5xx — "a policy DENY must be 4xx"); an unavailable upstream
// as 503. The /bff/oplus/* AuditorGate (auditor/admin/owner) is enforced by
// the mount in main.go; the role-gated test below proves the new path is
// covered by that prefix gate.
package httpadapter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
	"github.com/apollo-chora/chora-gateway/internal/middleware"
)

// newEgressGovMock serves GET /api/audit with the given status/body.
func newEgressGovMock(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/audit" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newEgressOPlusHandler(t *testing.T, gov *httptest.Server) *httpadapter.OPlusHandler {
	t.Helper()
	govClient := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: gov.URL, PerCallTimeout: 2 * time.Second},
	})
	return httpadapter.NewOPlusHandler(govClient, nil, nil, nil)
}

const egressHappyBody = `{"items":[
	{"event_id":"e1","tenant_id":"t1","action":"external_egress","decision":"denied",
	 "reason":"external egress denied; denial_reason=model_armor_pre_block",
	 "subject_type":"agent","subject_id":"companion_seeker",
	 "created_at":"2026-07-17T10:00:00Z",
	 "after":{"agentId":"companion_seeker","denialReason":"model_armor_pre_block"}}
],"total":1}`

func TestOPlusHandler_EgressAudit_Happy(t *testing.T) {
	t.Parallel()
	h := newEgressOPlusHandler(t, newEgressGovMock(t, http.StatusOK, egressHappyBody))

	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/governance/egress-audit", nil)
	rr := httptest.NewRecorder()
	h.EgressAudit(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var body struct {
		Items []map[string]any `json:"items"`
		Count int              `json:"count"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Count != 1 || len(body.Items) != 1 {
		t.Fatalf("count=%d items=%d; want 1/1", body.Count, len(body.Items))
	}
	if body.Items[0]["action"] != "external_egress" {
		t.Errorf("item action = %v; want external_egress", body.Items[0]["action"])
	}
}

func TestOPlusHandler_EgressAudit_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	h := newEgressOPlusHandler(t, newEgressGovMock(t, http.StatusOK, egressHappyBody))
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/governance/egress-audit", nil)
	rr := httptest.NewRecorder()
	h.EgressAudit(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d; want 405", rr.Code)
	}
}

// A governance DENY (upstream 403) must surface as a 4xx, NEVER a masked 5xx.
func TestOPlusHandler_EgressAudit_UpstreamDeny_Is4xxNot502(t *testing.T) {
	t.Parallel()
	h := newEgressOPlusHandler(t, newEgressGovMock(t, http.StatusForbidden, ""))
	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/governance/egress-audit", nil)
	rr := httptest.NewRecorder()
	h.EgressAudit(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 (deny must pass through as 4xx, not 502/503/200)", rr.Code)
	}
}

func TestOPlusHandler_EgressAudit_UpstreamUnavailable_503(t *testing.T) {
	t.Parallel()
	h := newEgressOPlusHandler(t, newEgressGovMock(t, http.StatusInternalServerError, ""))
	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/governance/egress-audit", nil)
	rr := httptest.NewRecorder()
	h.EgressAudit(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 (governance 5xx → upstream-unavailable)", rr.Code)
	}
}

// egressRolesKey + egressRoleResolver mirror the auditor_gate_test fixture so
// the role-gated test drives the real AuditorGate over the new O+ route.
type egressRolesKey struct{}

func egressRoleResolver(r *http.Request) []string {
	v, _ := r.Context().Value(egressRolesKey{}).([]string)
	return v
}

// The new route is served under /bff/oplus/, so it inherits the AuditorGate
// (auditor/admin/owner). Prove a role-less caller is 403'd and an auditor
// passes through.
func TestOPlusHandler_EgressAudit_RoleGated(t *testing.T) {
	t.Parallel()
	h := newEgressOPlusHandler(t, newEgressGovMock(t, http.StatusOK, egressHappyBody))
	gated := middleware.AuditorGate(egressRoleResolver, httpadapter.NewOPlusMux(h))

	// No roles → 403.
	rrDenied := httptest.NewRecorder()
	gated.ServeHTTP(rrDenied, httptest.NewRequest(http.MethodGet, "/bff/oplus/governance/egress-audit", nil))
	if rrDenied.Code != http.StatusForbidden {
		t.Fatalf("role-less status = %d; want 403", rrDenied.Code)
	}

	// Auditor role → gate admits; handler answers 200.
	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/governance/egress-audit", nil)
	req = req.WithContext(context.WithValue(req.Context(), egressRolesKey{}, []string{"auditor"}))
	rrOK := httptest.NewRecorder()
	gated.ServeHTTP(rrOK, req)
	if rrOK.Code == http.StatusForbidden {
		t.Fatalf("auditor was 403'd; the gate must admit auditor on the new route")
	}
	if rrOK.Code != http.StatusOK {
		t.Fatalf("auditor status = %d; want 200", rrOK.Code)
	}
}
