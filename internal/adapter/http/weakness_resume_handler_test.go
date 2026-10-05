// weakness_resume_handler_test.go — behaviour specs for the Growth-Edge HITL
// resume BFF route (ADR-205 WS-2/WS-8, CHO-1973). Internal-package test so specs
// can stamp MeshClaims directly via withMeshClaims (reused from
// closure_handler_test.go); the JWT middleware is exercised in jwt_auth_test.go.
//
// The route accepts the FE PANEL-SHAPE bounded review payload (action + per-edge
// decisions + added_struggles + selected_outputs; NO run_id) and proxies it to
// the orchestrator with the upload id from the PATH and identity/trace from the
// SESSION — never the body. The orchestrator's job/panel response is forwarded
// through verbatim.
package httpadapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/adapter/clients"
)

const testWeaknessResumePath = "/api/v1/me/growth-edges/uploads/upl-1/resume"

type fakeWeaknessResumeBackend struct {
	gotUploadID string
	gotReq      clients.WeaknessResumeRequest
	gotID       clients.WeaknessResumeIdentity
	out         json.RawMessage
	err         error
	called      bool
}

func (f *fakeWeaknessResumeBackend) ResumeWeakness(_ context.Context, uploadID string, req clients.WeaknessResumeRequest, id clients.WeaknessResumeIdentity) (json.RawMessage, error) {
	f.called = true
	f.gotUploadID = uploadID
	f.gotReq = req
	f.gotID = id
	return f.out, f.err
}

func newWeaknessResumeTestHandler(t *testing.T, backend WeaknessResumeBackend) http.Handler {
	t.Helper()
	var h *WeaknessResumeHandler
	if backend != nil {
		var err error
		h, err = NewWeaknessResumeHandler(backend)
		if err != nil {
			t.Fatalf("NewWeaknessResumeHandler: %v", err)
		}
	}
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // sentinel: fell through to base
	})
	return WithWeaknessResumeRoute(base, h)
}

func TestWeaknessResume_401WithoutClaims(t *testing.T) {
	h := newWeaknessResumeTestHandler(t, &fakeWeaknessResumeBackend{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, testWeaknessResumePath,
		`{"action":"confirm","edges":[]}`, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestWeaknessResume_ForwardsPanelFromSessionNotBody(t *testing.T) {
	backend := &fakeWeaknessResumeBackend{
		out: json.RawMessage(`{"upload_id":"upl-1","status":"COMPLETED","upserted_growth_edge_ids":["ge-1"]}`),
	}
	h := newWeaknessResumeTestHandler(t, backend)
	rec := httptest.NewRecorder()
	// Body smuggles tenant_id + run_id — both MUST be ignored; identity rides the
	// session and run_id is dropped (thread keyed by {tenant, upload}).
	body := `{"action":"confirm",` +
		`"edges":[{"proposed_edge_id":"edge-1","decision":"accept","difficulty":"standard"},` +
		`{"proposed_edge_id":"edge-2","decision":"merge","merge_into_id":"edge-9"}],` +
		`"added_struggles":["adding-fractions"],` +
		`"selected_outputs":["focused_dose","familiar_coaching"],` + // consumption's kept pre-rename wire key, forwarded opaquely
		`"tenant_id":"evil","run_id":"smuggled"}`
	req := closureReq(t, http.MethodPost, testWeaknessResumePath, body, sessionClaims())
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if !backend.called {
		t.Fatal("backend not called")
	}
	if backend.gotUploadID != "upl-1" {
		t.Errorf("uploadID = %q, want upl-1 (from path)", backend.gotUploadID)
	}
	if backend.gotID.TenantID != testTenantID || backend.gotID.GCID != testSessionGCID {
		t.Errorf("identity not from session claims: %+v", backend.gotID)
	}
	if backend.gotID.Traceparent == "" {
		t.Error("traceparent not forwarded to backend")
	}
	// Panel-shape forwarded verbatim.
	if backend.gotReq.Action != "confirm" {
		t.Errorf("action not forwarded: %+v", backend.gotReq)
	}
	if len(backend.gotReq.Edges) != 2 {
		t.Fatalf("edges not forwarded: %+v", backend.gotReq.Edges)
	}
	if backend.gotReq.Edges[0].ProposedEdgeID != "edge-1" || backend.gotReq.Edges[0].Decision != "accept" {
		t.Errorf("edge[0] not forwarded: %+v", backend.gotReq.Edges[0])
	}
	if backend.gotReq.Edges[1].Decision != "merge" || backend.gotReq.Edges[1].MergeIntoID != "edge-9" {
		t.Errorf("edge[1] merge not forwarded: %+v", backend.gotReq.Edges[1])
	}
	if len(backend.gotReq.AddedStruggles) != 1 || backend.gotReq.AddedStruggles[0] != "adding-fractions" {
		t.Errorf("added_struggles not forwarded: %+v", backend.gotReq.AddedStruggles)
	}
	if len(backend.gotReq.SelectedOutputs) != 2 || backend.gotReq.SelectedOutputs[1] != "familiar_coaching" {
		t.Errorf("selected_outputs not forwarded: %+v", backend.gotReq.SelectedOutputs)
	}
	// Job/panel response forwarded through verbatim.
	out := decodeBody(t, rec)
	if out["status"] != "COMPLETED" || out["upload_id"] != "upl-1" {
		t.Errorf("response job not forwarded: %+v", out)
	}
}

func TestWeaknessResume_ReiteratePanelPassThrough(t *testing.T) {
	backend := &fakeWeaknessResumeBackend{
		out: json.RawMessage(`{"upload_id":"upl-1","status":"AWAITING_REVIEW",` +
			`"review":{"proposed_edges":[{"proposed_edge_id":"edge-7"}]}}`),
	}
	h := newWeaknessResumeTestHandler(t, backend)
	rec := httptest.NewRecorder()
	body := `{"action":"reiterate","edges":[{"proposed_edge_id":"edge-1","decision":"reject"}],` +
		`"added_struggles":[],"selected_outputs":[]}`
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, testWeaknessResumePath, body, sessionClaims()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if backend.gotReq.Action != "reiterate" {
		t.Errorf("action = %q, want reiterate", backend.gotReq.Action)
	}
	out := decodeBody(t, rec)
	if out["status"] != "AWAITING_REVIEW" {
		t.Errorf("reiterate status not forwarded: %+v", out)
	}
	if _, ok := out["review"].(map[string]any); !ok {
		t.Errorf("refreshed review panel not forwarded: %+v", out)
	}
}

func TestWeaknessResume_400OnMissingAction(t *testing.T) {
	backend := &fakeWeaknessResumeBackend{}
	h := newWeaknessResumeTestHandler(t, backend)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, testWeaknessResumePath,
		`{"edges":[]}`, sessionClaims()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if backend.called {
		t.Error("backend must NOT be called when action is missing")
	}
}

func TestWeaknessResume_DegradedReturns503(t *testing.T) {
	h := newWeaknessResumeTestHandler(t, nil) // nil backend → degraded route
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, testWeaknessResumePath,
		`{"action":"confirm","edges":[]}`, sessionClaims()))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if code := errCode(t, rec); code != "WEAKNESS_RESUME_UNAVAILABLE" {
		t.Errorf("error code = %q, want WEAKNESS_RESUME_UNAVAILABLE", code)
	}
}

func TestWeaknessResume_MirrorsUpstream4xx(t *testing.T) {
	backend := &fakeWeaknessResumeBackend{
		err: &clients.WeaknessResumeUpstreamError{
			StatusCode: http.StatusBadRequest, Code: "invalid_action", Message: "bad",
		},
	}
	h := newWeaknessResumeTestHandler(t, backend)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, testWeaknessResumePath,
		`{"action":"bogus","edges":[]}`, sessionClaims()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (mirrored)", rec.Code)
	}
	if code := errCode(t, rec); code != "invalid_action" {
		t.Errorf("error code = %q, want invalid_action (mirrored)", code)
	}
}

func TestWeaknessResume_503OnUpstreamUnavailable(t *testing.T) {
	backend := &fakeWeaknessResumeBackend{err: clients.ErrWeaknessResumeUpstreamUnavailable}
	h := newWeaknessResumeTestHandler(t, backend)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, closureReq(t, http.MethodPost, testWeaknessResumePath,
		`{"action":"confirm","edges":[]}`, sessionClaims()))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestWeaknessResume_NonResumePathFallsThroughToBase(t *testing.T) {
	h := newWeaknessResumeTestHandler(t, &fakeWeaknessResumeBackend{})
	rec := httptest.NewRecorder()
	// A sibling growth-edges path must NOT be captured by the resume route.
	h.ServeHTTP(rec, closureReq(t, http.MethodGet, "/api/v1/me/growth-edges/uploads/upl-1",
		"", sessionClaims()))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418 (fell through to base)", rec.Code)
	}
}
