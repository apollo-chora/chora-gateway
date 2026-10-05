// handlers_oplus_hitl_test.go — N13 black-box coverage for the BFF HITL
// self-claim + release passthrough routes:
//
//	POST /bff/oplus/governance/hitl/{id}/claim
//	POST /bff/oplus/governance/hitl/{id}/release
//
// Status mapping (per FE contract):
//
//	200 — claim/release succeeded; body is the updated HITLDecisionItem
//	404 — decision_id not found upstream
//	409 — already claimed (claim) / terminal (claim/release)
//	403 — release attempted by non-assignee (auditor role gate handled
//	      by the auditor_gate middleware, NOT this handler)
//	422 — invalid body (blank operator_gcid)
//	503 — upstream chora-governance unreachable
//
// Each test stands up a stub chora-governance HTTP server, points the
// GovernanceClient at it, and asserts the BFF handler maps the upstream
// status correctly.
package httpadapter_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

// newHITLOPlusHandler builds an OPlusHandler whose Governance client points
// at the supplied stub server. Observability + A2A wiring intentionally nil
// — the HITL passthrough does not consult either.
func newHITLOPlusHandler(t *testing.T, govURL string) *httpadapter.OPlusHandler {
	t.Helper()
	govClient := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: govURL, PerCallTimeout: 1 * time.Second},
	})
	return httpadapter.NewOPlusHandler(govClient, nil, nil, nil)
}

// withAuditorGCID stamps the auditor's GCID into the upstream auth context
// so the handler can find it when assembling the request body.
func withAuditorGCID(req *http.Request, gcid string) *http.Request {
	ctx := upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{GCID: gcid})
	return req.WithContext(ctx)
}

func postJSONBody(t *testing.T, v map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// -----------------------------------------------------------------------------
// Claim — happy / 404 / 409 / 422 / 503 paths
// -----------------------------------------------------------------------------

func TestOPlusHITLClaim_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hitl/decisions/d-1/claim" {
			t.Errorf("path = %s", r.URL.Path)
		}
		// X-Tenant-Id propagation is tested at the upstream client layer
		// (governance_client_test.go) where the request flow is direct
		// rather than via the BFF middleware chain. The BFF handler test
		// here verifies only the path + body + status-mapping contract.
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["operator_gcid"] != "auditor-gcid-1" {
			t.Errorf("operator_gcid = %v; want auditor-gcid-1", body["operator_gcid"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision_id":"d-1","run_id":"r-1","assignee_gcid":"auditor-gcid-1","decision":"pending","autonomy_level":"hitl_l1","lifecycle_stage":"runtime"}`))
	}))
	defer srv.Close()

	h := newHITLOPlusHandler(t, srv.URL)
	body := postJSONBody(t, map[string]any{"operator_gcid": "auditor-gcid-1"})
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/governance/hitl/d-1/claim", bytes.NewReader(body))
	req = withAuditorGCID(req, "auditor-gcid-1")
	// Stamp tenant via upstream auth ctx so the handler forwards X-Tenant-Id.
	ctx := upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{TenantID: "tenant-1", GCID: "auditor-gcid-1"})
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()

	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200, body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["decision_id"] != "d-1" {
		t.Errorf("decision_id = %v", resp["decision_id"])
	}
	if resp["assignee_gcid"] != "auditor-gcid-1" {
		t.Errorf("assignee_gcid = %v; want auditor-gcid-1", resp["assignee_gcid"])
	}
}

func TestOPlusHITLClaim_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	h := newHITLOPlusHandler(t, srv.URL)
	body := postJSONBody(t, map[string]any{"operator_gcid": "auditor-1"})
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/governance/hitl/d-missing/claim", bytes.NewReader(body))
	req = req.WithContext(upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{TenantID: "t1", GCID: "auditor-1"}))
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404, body=%s", rr.Code, rr.Body.String())
	}
}

func TestOPlusHITLClaim_Conflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()
	h := newHITLOPlusHandler(t, srv.URL)
	body := postJSONBody(t, map[string]any{"operator_gcid": "auditor-1"})
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/governance/hitl/d-1/claim", bytes.NewReader(body))
	req = req.WithContext(upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{TenantID: "t1", GCID: "auditor-1"}))
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("status = %d; want 409, body=%s", rr.Code, rr.Body.String())
	}
}

// 422 — blank operator_gcid in body. The BFF rejects locally BEFORE the
// upstream is called (saves a round-trip + matches the FE contract precisely).
func TestOPlusHITLClaim_Unprocessable_BlankOperator(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer srv.Close()
	h := newHITLOPlusHandler(t, srv.URL)
	body := postJSONBody(t, map[string]any{"operator_gcid": "   "})
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/governance/hitl/d-1/claim", bytes.NewReader(body))
	req = req.WithContext(upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{TenantID: "t1", GCID: "auditor-1"}))
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d; want 422", rr.Code)
	}
	if called {
		t.Error("upstream was called despite blank operator_gcid (should fail-fast at BFF)")
	}
}

// 422 — malformed JSON.
func TestOPlusHITLClaim_Unprocessable_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("upstream must not be called on malformed body")
	}))
	defer srv.Close()
	h := newHITLOPlusHandler(t, srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/governance/hitl/d-1/claim", strings.NewReader(`{ not-json`))
	req = req.WithContext(upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{TenantID: "t1", GCID: "auditor-1"}))
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d; want 422", rr.Code)
	}
}

// 503 — upstream unreachable.
func TestOPlusHITLClaim_Unavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	h := newHITLOPlusHandler(t, srv.URL)
	body := postJSONBody(t, map[string]any{"operator_gcid": "auditor-1"})
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/governance/hitl/d-1/claim", bytes.NewReader(body))
	req = req.WithContext(upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{TenantID: "t1", GCID: "auditor-1"}))
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d; want 503", rr.Code)
	}
}

// 405 — wrong method (the route exists; only POST is valid).
func TestOPlusHITLClaim_RejectsGET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("upstream must not be called for wrong-method request")
	}))
	defer srv.Close()
	h := newHITLOPlusHandler(t, srv.URL)
	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/governance/hitl/d-1/claim", nil)
	req = req.WithContext(upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{TenantID: "t1", GCID: "auditor-1"}))
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405", rr.Code)
	}
}

// -----------------------------------------------------------------------------
// Release — happy / 403 (non-assignee) / 404 / 503
// -----------------------------------------------------------------------------

func TestOPlusHITLRelease_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hitl/decisions/d-1/release" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision_id":"d-1","run_id":"r-1","assignee_gcid":null,"decision":"pending","autonomy_level":"hitl_l1","lifecycle_stage":"runtime"}`))
	}))
	defer srv.Close()
	h := newHITLOPlusHandler(t, srv.URL)
	body := postJSONBody(t, map[string]any{"operator_gcid": "auditor-gcid-1"})
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/governance/hitl/d-1/release", bytes.NewReader(body))
	req = req.WithContext(upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{TenantID: "tenant-1", GCID: "auditor-gcid-1"}))
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200, body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["decision_id"] != "d-1" {
		t.Errorf("decision_id = %v", resp["decision_id"])
	}
	if resp["assignee_gcid"] != nil {
		t.Errorf("assignee_gcid = %v; want null after release", resp["assignee_gcid"])
	}
}

// 403 — release by a non-assignee. The BFF passes through the upstream 403.
func TestOPlusHITLRelease_Forbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	h := newHITLOPlusHandler(t, srv.URL)
	body := postJSONBody(t, map[string]any{"operator_gcid": "auditor-1"})
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/governance/hitl/d-1/release", bytes.NewReader(body))
	req = req.WithContext(upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{TenantID: "t1", GCID: "auditor-1"}))
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403", rr.Code)
	}
}

func TestOPlusHITLRelease_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	h := newHITLOPlusHandler(t, srv.URL)
	body := postJSONBody(t, map[string]any{"operator_gcid": "auditor-1"})
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/governance/hitl/d-missing/release", bytes.NewReader(body))
	req = req.WithContext(upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{TenantID: "t1", GCID: "auditor-1"}))
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rr.Code)
	}
}

func TestOPlusHITLRelease_Unavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	h := newHITLOPlusHandler(t, srv.URL)
	body := postJSONBody(t, map[string]any{"operator_gcid": "auditor-1"})
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/governance/hitl/d-1/release", bytes.NewReader(body))
	req = req.WithContext(upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{TenantID: "t1", GCID: "auditor-1"}))
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d; want 503", rr.Code)
	}
}

// 503 — governance client unwired (no HTTP addr). The handler must surface
// 503 rather than panicking on the nil-safe HTTP call.
func TestOPlusHITLClaim_GovernanceUnwired(t *testing.T) {
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{}, // no addr
	})
	h := httpadapter.NewOPlusHandler(gc, nil, nil, nil)
	body := postJSONBody(t, map[string]any{"operator_gcid": "auditor-1"})
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/governance/hitl/d-1/claim", bytes.NewReader(body))
	req = req.WithContext(upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{TenantID: "t1", GCID: "auditor-1"}))
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d; want 503", rr.Code)
	}
}

// 404 — unknown sub-route under /bff/oplus/governance/hitl/... (i.e. the
// action is not claim/release/approve/reject).
func TestOPlusHITL_UnknownAction(t *testing.T) {
	h := newHITLOPlusHandler(t, "http://example")
	body := postJSONBody(t, map[string]any{"operator_gcid": "auditor-1"})
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/governance/hitl/d-1/cancel", bytes.NewReader(body))
	req = req.WithContext(upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{TenantID: "t1", GCID: "auditor-1"}))
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rr.Code)
	}
}

// -----------------------------------------------------------------------------
// Approve / Reject — verdict passthrough (happy + note forwarding + errors)
// -----------------------------------------------------------------------------

func TestOPlusHITLApprove_Happy_ForwardsNote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hitl/decisions/d-1/approve" {
			t.Errorf("path = %s; want /api/hitl/decisions/d-1/approve", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["operator_gcid"] != "auditor-gcid-1" {
			t.Errorf("operator_gcid = %v; want auditor-gcid-1", body["operator_gcid"])
		}
		if body["note"] != "meets rubric" {
			t.Errorf("note = %v; want 'meets rubric'", body["note"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision_id":"d-1","run_id":"r-1","assignee_gcid":"auditor-gcid-1","decision":"approved","autonomy_level":"hitl_l1","lifecycle_stage":"runtime"}`))
	}))
	defer srv.Close()

	h := newHITLOPlusHandler(t, srv.URL)
	body := postJSONBody(t, map[string]any{"operator_gcid": "auditor-gcid-1", "note": "meets rubric"})
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/governance/hitl/d-1/approve", bytes.NewReader(body))
	req = req.WithContext(upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{TenantID: "tenant-1", GCID: "auditor-gcid-1"}))
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200, body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["decision"] != "approved" {
		t.Errorf("decision = %v; want approved", resp["decision"])
	}
}

// note is optional — approve with no note must still succeed (operator_gcid
// alone is a valid body).
func TestOPlusHITLApprove_Happy_NoteOptional(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, has := body["note"]; has {
			t.Errorf("note should be omitted when not supplied: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision_id":"d-1","decision":"approved"}`))
	}))
	defer srv.Close()
	h := newHITLOPlusHandler(t, srv.URL)
	body := postJSONBody(t, map[string]any{"operator_gcid": "auditor-1"})
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/governance/hitl/d-1/approve", bytes.NewReader(body))
	req = req.WithContext(upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{TenantID: "t1", GCID: "auditor-1"}))
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d; want 200, body=%s", rr.Code, rr.Body.String())
	}
}

func TestOPlusHITLReject_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hitl/decisions/d-1/reject" {
			t.Errorf("path = %s; want /api/hitl/decisions/d-1/reject", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision_id":"d-1","run_id":"r-1","assignee_gcid":"auditor-1","decision":"rejected","autonomy_level":"hitl_l1","lifecycle_stage":"runtime"}`))
	}))
	defer srv.Close()
	h := newHITLOPlusHandler(t, srv.URL)
	body := postJSONBody(t, map[string]any{"operator_gcid": "auditor-1", "note": "off-policy"})
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/governance/hitl/d-1/reject", bytes.NewReader(body))
	req = req.WithContext(upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{TenantID: "t1", GCID: "auditor-1"}))
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200, body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp["decision"] != "rejected" {
		t.Errorf("decision = %v; want rejected", resp["decision"])
	}
}

// 403 — verdict by a non-assignee passes through the upstream 403.
func TestOPlusHITLApprove_Forbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	h := newHITLOPlusHandler(t, srv.URL)
	body := postJSONBody(t, map[string]any{"operator_gcid": "auditor-1"})
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/governance/hitl/d-1/approve", bytes.NewReader(body))
	req = req.WithContext(upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{TenantID: "t1", GCID: "auditor-1"}))
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403", rr.Code)
	}
}

// 409 — terminal verdict already recorded.
func TestOPlusHITLReject_Conflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()
	h := newHITLOPlusHandler(t, srv.URL)
	body := postJSONBody(t, map[string]any{"operator_gcid": "auditor-1"})
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/governance/hitl/d-1/reject", bytes.NewReader(body))
	req = req.WithContext(upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{TenantID: "t1", GCID: "auditor-1"}))
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("status = %d; want 409", rr.Code)
	}
}

// 422 — blank operator_gcid rejected locally before upstream (approve path).
func TestOPlusHITLApprove_Unprocessable_BlankOperator(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	h := newHITLOPlusHandler(t, srv.URL)
	body := postJSONBody(t, map[string]any{"operator_gcid": "  ", "note": "x"})
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/governance/hitl/d-1/approve", bytes.NewReader(body))
	req = req.WithContext(upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{TenantID: "t1", GCID: "auditor-1"}))
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d; want 422", rr.Code)
	}
	if called {
		t.Error("upstream called despite blank operator_gcid")
	}
}

// 422 — DisallowUnknownFields still rejects a key other than
// operator_gcid + note (e.g. a typo'd field) on the approve path.
func TestOPlusHITLApprove_Unprocessable_UnknownField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("upstream must not be called on body with unknown field")
	}))
	defer srv.Close()
	h := newHITLOPlusHandler(t, srv.URL)
	body := postJSONBody(t, map[string]any{"operator_gcid": "auditor-1", "notez": "typo"})
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/governance/hitl/d-1/approve", bytes.NewReader(body))
	req = req.WithContext(upstream.WithAuthCtx(req.Context(), upstream.AuthCtx{TenantID: "t1", GCID: "auditor-1"}))
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	httpadapter.RegisterOPlusRoutes(mux, h)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d; want 422 (unknown field rejected)", rr.Code)
	}
}
