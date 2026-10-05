// Package phyllis_test exercises the Phyllis MVP route aggregation layer.
//
// Per docs/m13/phyllis-mvp-2026-05-08.md, the BFF gateway exposes 11 routes
// covering the 8-step happy path: identity context, tenant resolution, course
// CRUD, enrollment, atom playback + feedback, AI generation with guardrails,
// and Companion daily-dose retrieval. Each route fans out to one or more
// downstream services, propagates W3C traceparent + Authorization headers,
// honours a 5s per-call + 15s per-aggregation budget, and returns either
// 200 OK with merged JSON or pass-through error codes (401/403/404/5xx).
package phyllis_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

// stubUpstream stands up a small httptest server with a configurable handler.
// Each test composes its own constellation of stubs (identity, tenancy,
// creation, delivery, consumption, model-broker) and wires them into the
// aggregator under test.
type stubUpstream struct {
	*httptest.Server
	calls    atomic.Int64
	lastAuth string
	lastTP   string
}

func newStubUpstream(t *testing.T, h http.HandlerFunc) *stubUpstream {
	t.Helper()
	s := &stubUpstream{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		s.lastAuth = r.Header.Get("Authorization")
		s.lastTP = r.Header.Get("traceparent")
		h(w, r)
	}))
	t.Cleanup(s.Server.Close)
	return s
}

func decodeJSON(t *testing.T, body io.Reader) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(body).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

// -----------------------------------------------------------------------------
// GetMe — fan-out to chora-identity:/me
// -----------------------------------------------------------------------------

func TestGetMe_HappyPath(t *testing.T) {
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me" {
			t.Errorf("path = %s; want /me", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"gcid":"phyllis-gcid","email":"phyllis@mtm.sg","display_name":"Phyllis","linked_idps":["google"]}`))
	})

	cfg := phyllis.Config{
		IdentityURL:    identity.URL,
		PerCallTimeout: 1 * time.Second,
	}
	a := phyllis.New(cfg, nil)

	res, err := a.GetMe(context.Background(), phyllis.AuthCtx{Bearer: "phyllis-gcid", Traceparent: "00-aaa-bbb-01"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", res.Status)
	}
	body := decodeJSON(t, strings.NewReader(string(res.Body)))
	if body["gcid"] != "phyllis-gcid" {
		t.Errorf("gcid = %v", body["gcid"])
	}
	if identity.lastAuth != "Bearer phyllis-gcid" {
		t.Errorf("Authorization header = %q; want pass-through", identity.lastAuth)
	}
	if identity.lastTP != "00-aaa-bbb-01" {
		t.Errorf("traceparent = %q; want propagated", identity.lastTP)
	}
}

func TestGetMe_Pass401Through(t *testing.T) {
	identity := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"GATEWAY_INVALID_JWT","message":"jwt invalid"}}`))
	})
	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: 1 * time.Second}, nil)

	res, err := a.GetMe(context.Background(), phyllis.AuthCtx{Bearer: "bad"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401 pass-through", res.Status)
	}
}

func TestGetMe_5xxCascade(t *testing.T) {
	identity := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"INTERNAL"}}`))
	})
	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: 1 * time.Second}, nil)

	res, err := a.GetMe(context.Background(), phyllis.AuthCtx{Bearer: "x"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 (5xx cascade normalised)", res.Status)
	}
}

func TestGetMe_TimeoutReturns504(t *testing.T) {
	identity := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: 50 * time.Millisecond}, nil)
	res, err := a.GetMe(context.Background(), phyllis.AuthCtx{Bearer: "x"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusGatewayTimeout {
		t.Errorf("status = %d; want 504", res.Status)
	}
}

// -----------------------------------------------------------------------------
// GetMyRoles — fan-out to chora-identity:/me/roles?course_id=X
// -----------------------------------------------------------------------------

func TestGetMyRoles_PerCourse(t *testing.T) {
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/roles" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("course_id") != "cspo" {
			t.Errorf("course_id = %s", r.URL.Query().Get("course_id"))
		}
		_, _ = w.Write([]byte(`{"role":"instructor","course_id":"cspo"}`))
	})
	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: 1 * time.Second}, nil)

	res, err := a.GetMyRoles(context.Background(), phyllis.AuthCtx{Bearer: "x"}, "cspo")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d", res.Status)
	}
	body := decodeJSON(t, strings.NewReader(string(res.Body)))
	if body["role"] != "instructor" {
		t.Errorf("role = %v", body["role"])
	}
}

// -----------------------------------------------------------------------------
// GetMyTenant — pass-through to chora-tenancy:/api/tenants/{id}
//
// A6 follow-up (CHO-1545): the prior path was /tenants/{id}, but
// chora-tenancy serves the tenant detail under /api/tenants/{id} (its
// HTTP adapter mux — same bug class A6 already fixed for GetAtom +
// gatewayproxy.GetTenant). The path mismatch made GET /api/tenants/me
// 404 at chora-tenancy. These tests pin the corrected path.
// -----------------------------------------------------------------------------

func TestGetMyTenant_HappyPath(t *testing.T) {
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tenants/mtm-singapore" {
			t.Errorf("path = %s; want /api/tenants/mtm-singapore", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"tenant_id":"mtm-singapore","name":"MTM Singapore","plan":"core"}`))
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)

	res, err := a.GetMyTenant(context.Background(), phyllis.AuthCtx{Bearer: "x", TenantID: "mtm-singapore"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d", res.Status)
	}
	body := decodeJSON(t, strings.NewReader(string(res.Body)))
	if body["tenant_id"] != "mtm-singapore" {
		t.Errorf("tenant_id = %v", body["tenant_id"])
	}
}

// TestGetMyTenant_TargetsApiTenantsPath is the A6-follow-up regression
// guard: GetMyTenant MUST target chora-tenancy's /api/tenants/{id} path
// (NOT the legacy /tenants/{id}, which 404s — chora-tenancy mounts the
// tenant detail handler under /api/tenants/). Mirrors the verified
// gatewayproxy.GetTenant path. A regression here re-breaks Phyllis
// step 1b's GET /api/tenants/me.
func TestGetMyTenant_TargetsApiTenantsPath(t *testing.T) {
	var gotPath string
	tenancy := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"tenant_id":"mtm-singapore"}`))
	})
	a := phyllis.New(phyllis.Config{TenancyURL: tenancy.URL, PerCallTimeout: 1 * time.Second}, nil)

	res, err := a.GetMyTenant(context.Background(), phyllis.AuthCtx{Bearer: "x", TenantID: "mtm-singapore"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", res.Status)
	}
	if gotPath != "/api/tenants/mtm-singapore" {
		t.Errorf("downstream path = %q; want /api/tenants/mtm-singapore (NOT the legacy /tenants/{id})", gotPath)
	}
}

func TestGetMyTenant_NoTenantInAuth_400(t *testing.T) {
	a := phyllis.New(phyllis.Config{TenancyURL: "http://unused", PerCallTimeout: 1 * time.Second}, nil)
	res, err := a.GetMyTenant(context.Background(), phyllis.AuthCtx{Bearer: "x"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 when tenant_id missing", res.Status)
	}
}

// -----------------------------------------------------------------------------
// CreateCourse — composite: chora-creation:/atoms + chora-delivery:/courses
// -----------------------------------------------------------------------------

func TestCreateCourse_Composite_AllOK(t *testing.T) {
	creation := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/atoms" {
			t.Errorf("creation: method=%s path=%s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"atom_id":"atom-root-001"}`))
	})
	delivery := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/courses" {
			t.Errorf("delivery: method=%s path=%s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"course_id":"course-cspo-001","title":"CSPO"}`))
	})
	a := phyllis.New(phyllis.Config{
		CreationURL:    creation.URL,
		DeliveryURL:    delivery.URL,
		PerCallTimeout: 1 * time.Second,
	}, nil)

	body := []byte(`{"title":"CSPO Fundamentals"}`)
	res, err := a.CreateCourse(context.Background(), phyllis.AuthCtx{Bearer: "x"}, body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", res.Status)
	}
	out := decodeJSON(t, strings.NewReader(string(res.Body)))
	if out["course_id"] != "course-cspo-001" {
		t.Errorf("course_id = %v", out["course_id"])
	}
	if out["root_atom_id"] != "atom-root-001" {
		t.Errorf("root_atom_id = %v", out["root_atom_id"])
	}
	if creation.calls.Load() != 1 {
		t.Errorf("creation calls = %d; want 1", creation.calls.Load())
	}
	if delivery.calls.Load() != 1 {
		t.Errorf("delivery calls = %d; want 1", delivery.calls.Load())
	}
}

func TestCreateCourse_CreationFails_Returns502(t *testing.T) {
	creation := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	delivery := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("delivery must NOT be called when creation fails")
	})
	a := phyllis.New(phyllis.Config{
		CreationURL:    creation.URL,
		DeliveryURL:    delivery.URL,
		PerCallTimeout: 1 * time.Second,
	}, nil)
	res, err := a.CreateCourse(context.Background(), phyllis.AuthCtx{Bearer: "x"}, []byte(`{"title":"x"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", res.Status)
	}
}

// -----------------------------------------------------------------------------
// GetCatalog — fan-out to chora-delivery:/v1/courses (Phyllis Step 5
// PublicDiscovery). The aggregator targets the consolidated /v1/courses
// route with a `visibility` query param (NOT the legacy /courses?public=true
// path — that route is tenantRequired-gated and 400s an unauthed request).
//
// Contract:
//   - public=true OR no tenant context  → /v1/courses?visibility=public
//     (anonymous catalogue browse — strictly public-visibility rows only;
//     never leaks tenant_only rows).
//   - public=false AND tenant present   → /v1/courses?visibility=tenant_or_public
//     (authed catalogue browse — caller's tenant rows + public rows).
// -----------------------------------------------------------------------------

func TestGetCatalog_PublicExplicit_UsesV1VisibilityPublic(t *testing.T) {
	delivery := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/courses" {
			t.Errorf("path = %s; want /v1/courses", r.URL.Path)
		}
		if got := r.URL.Query().Get("visibility"); got != "public" {
			t.Errorf("visibility query = %q; want public", got)
		}
		if r.URL.Query().Has("public") {
			t.Errorf("legacy `public` query param must not be sent; got %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"items":[{"id":"csm-prep","visibility":"public"}],"total":1}`))
	})
	a := phyllis.New(phyllis.Config{DeliveryURL: delivery.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, err := a.GetCatalog(context.Background(), phyllis.AuthCtx{}, true, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", res.Status)
	}
}

// Plain /api/catalog with NO query and NO auth (the Phyllis demo's UNAUTHED
// public-discovery curl) must still resolve to visibility=public — an
// anonymous caller must NEVER receive tenant_or_public (which would leak
// other tenants' tenant_only rows).
func TestGetCatalog_Anonymous_NoTenant_UsesV1VisibilityPublic(t *testing.T) {
	delivery := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/courses" {
			t.Errorf("path = %s; want /v1/courses", r.URL.Path)
		}
		if got := r.URL.Query().Get("visibility"); got != "public" {
			t.Errorf("anonymous caller visibility = %q; want public (no tenant leak)", got)
		}
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	})
	a := phyllis.New(phyllis.Config{DeliveryURL: delivery.URL, PerCallTimeout: 1 * time.Second}, nil)
	// public=false (no ?public=true query) AND AuthCtx{} (no tenant) — the
	// unauthed plain /api/catalog case.
	res, err := a.GetCatalog(context.Background(), phyllis.AuthCtx{}, false, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", res.Status)
	}
}

// Authed catalogue browse (tenant present, public=false) → tenant_or_public
// so the learner sees their own tenant's catalogue plus public courses.
func TestGetCatalog_AuthedTenant_UsesV1VisibilityTenantOrPublic(t *testing.T) {
	delivery := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/courses" {
			t.Errorf("path = %s; want /v1/courses", r.URL.Path)
		}
		if got := r.URL.Query().Get("visibility"); got != "tenant_or_public" {
			t.Errorf("authed caller visibility = %q; want tenant_or_public", got)
		}
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	})
	a := phyllis.New(phyllis.Config{DeliveryURL: delivery.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, err := a.GetCatalog(context.Background(),
		phyllis.AuthCtx{TenantID: "01970000-0000-7000-8000-000000000001"}, false, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", res.Status)
	}
}

// Even with public=true an authed caller stays on visibility=public — an
// explicit public-discovery request is public-only regardless of tenant.
func TestGetCatalog_AuthedTenant_PublicExplicit_StaysVisibilityPublic(t *testing.T) {
	delivery := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("visibility"); got != "public" {
			t.Errorf("visibility = %q; want public", got)
		}
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	})
	a := phyllis.New(phyllis.Config{DeliveryURL: delivery.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, err := a.GetCatalog(context.Background(),
		phyllis.AuthCtx{TenantID: "01970000-0000-7000-8000-000000000001"}, true, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", res.Status)
	}
}

// -----------------------------------------------------------------------------
// CreateEnrollment — chora-delivery:/enrollments + emits event (callback)
// -----------------------------------------------------------------------------

func TestCreateEnrollment_HappyPath_EmitsEvent(t *testing.T) {
	delivery := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/enrollments" {
			t.Errorf("delivery: method=%s path=%s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"enrollment_id":"enr-001","course_id":"csm-prep","gcid":"phyllis-gcid"}`))
	})

	var emitted []string
	emit := func(_ context.Context, topic string, _ []byte) error {
		emitted = append(emitted, topic)
		return nil
	}

	a := phyllis.New(phyllis.Config{
		DeliveryURL:    delivery.URL,
		PerCallTimeout: 1 * time.Second,
	}, emit)

	res, err := a.CreateEnrollment(context.Background(), phyllis.AuthCtx{Bearer: "x"}, []byte(`{"course_id":"csm-prep","gcid":"phyllis-gcid"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", res.Status)
	}
	if len(emitted) != 1 || emitted[0] != "chora.delivery.enrollment.created.v1" {
		t.Errorf("emitted topics = %v; want [chora.delivery.enrollment.created.v1]", emitted)
	}
}

func TestCreateEnrollment_DownstreamFails_NoEvent(t *testing.T) {
	delivery := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	emitted := 0
	emit := func(_ context.Context, _ string, _ []byte) error { emitted++; return nil }
	a := phyllis.New(phyllis.Config{DeliveryURL: delivery.URL, PerCallTimeout: 1 * time.Second}, emit)

	_, _ = a.CreateEnrollment(context.Background(), phyllis.AuthCtx{Bearer: "x"}, []byte(`{}`))
	if emitted != 0 {
		t.Errorf("emitted = %d; want 0 when downstream fails", emitted)
	}
}

// -----------------------------------------------------------------------------
// GetAtom — proxy to chora-creation (atom content) through the A16 gate.
// The dead /atoms/{id}/session side-load was removed (CHO-1968): chora-
// consumption has no such endpoint, so it always 404'd and fabricated a
// spurious "session_error: upstream_unavailable" degraded banner.
// -----------------------------------------------------------------------------

func TestGetAtom_HealthyAtom_NoSessionSideLoad(t *testing.T) {
	var atomCalls, sessionCalls atomic.Int64
	creation := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		atomCalls.Add(1)
		// chora-creation serves the atom under /api/atoms/{id} (legacy
		// AtomHandler mux) — NOT /atoms/{id}. The gateway-facing path is
		// /api/atoms/{id}; the BFF forwards to the SAME path.
		if r.URL.Path != "/api/atoms/atom-001" {
			t.Errorf("creation path = %s; want /api/atoms/atom-001", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"atom_id":"atom-001","title":"What is photosynthesis?"}`))
	})
	// The dead session side-load must NEVER fire — any hit here is a regression.
	consumption := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		sessionCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})

	a := phyllis.New(phyllis.Config{
		CreationURL:    creation.URL,
		ConsumptionURL: consumption.URL,
		PerCallTimeout: 1 * time.Second,
	}, nil)

	res, err := a.GetAtom(context.Background(), phyllis.AuthCtx{Bearer: "x"}, "atom-001", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d", res.Status)
	}
	body := decodeJSON(t, strings.NewReader(string(res.Body)))
	atomBlock, ok := body["atom"].(map[string]any)
	if !ok || atomBlock["atom_id"] != "atom-001" {
		t.Errorf("missing atom block: %v", body)
	}
	if _, present := body["session"]; present {
		t.Errorf("session block must be gone after side-load deletion: %v", body)
	}
	if _, present := body["session_error"]; present {
		t.Errorf("session_error must be gone after side-load deletion: %v", body)
	}
	if atomCalls.Load() != 1 {
		t.Errorf("atom calls = %d; want 1", atomCalls.Load())
	}
	if sessionCalls.Load() != 0 {
		t.Errorf("consumption session side-load must not fire; got %d", sessionCalls.Load())
	}
}

func TestGetAtom_AtomFails_Returns502_NoSessionBlock(t *testing.T) {
	creation := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	consumption := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"session_id":"sess-001"}`))
	})
	a := phyllis.New(phyllis.Config{
		CreationURL:    creation.URL,
		ConsumptionURL: consumption.URL,
		PerCallTimeout: 1 * time.Second,
	}, nil)
	res, _ := a.GetAtom(context.Background(), phyllis.AuthCtx{Bearer: "x"}, "atom-001", "")
	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 when primary atom call fails", res.Status)
	}
}

func TestGetAtom_AtomNotFound_Returns404(t *testing.T) {
	creation := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"NOT_FOUND"}}`))
	})
	consumption := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	a := phyllis.New(phyllis.Config{CreationURL: creation.URL, ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GetAtom(context.Background(), phyllis.AuthCtx{Bearer: "x"}, "missing", "")
	if res.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404", res.Status)
	}
}

// -----------------------------------------------------------------------------
// WS-0b A16 — GetAtom question_payload propagation + correct_option_id strip
// -----------------------------------------------------------------------------

// TestGetAtom_QuestionPayload_PropagatesFromUpstream — when chora-creation
// emits a question_payload, the BFF surfaces it under envelope.atom unchanged.
func TestGetAtom_QuestionPayload_PropagatesFromUpstream(t *testing.T) {
	creation := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"atom_id":"atom-001",
			"title":"MCQ Atom",
			"question_payload":{
				"type":"mcq",
				"question_id":"q-001",
				"prompt":"Which planet?",
				"options":[
					{"option_id":"opt_1","label":"Earth"},
					{"option_id":"opt_2","label":"Mars"}
				],
				"xp_on_correct":50
			}
		}`))
	})
	consumption := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	a := phyllis.New(phyllis.Config{CreationURL: creation.URL, ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GetAtom(context.Background(), phyllis.AuthCtx{Bearer: "x"}, "atom-001", "")
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", res.Status, string(res.Body))
	}
	body := decodeJSON(t, strings.NewReader(string(res.Body)))
	atomBlk, _ := body["atom"].(map[string]any)
	qp, ok := atomBlk["question_payload"].(map[string]any)
	if !ok {
		t.Fatalf("question_payload missing in BFF response; got body=%s", string(res.Body))
	}
	if qp["type"] != "mcq" {
		t.Errorf("question_payload.type = %v; want mcq", qp["type"])
	}
	options, _ := qp["options"].([]any)
	if len(options) != 2 {
		t.Errorf("question_payload.options length = %d; want 2", len(options))
	}
}

// TestGetAtom_QuestionPayload_StripsCorrectOptionId_InLearnerMode — when
// caller is not in author mode (no ?mode=author OR roles do not include
// author/instructor), the BFF strips question_payload.correct_option_id
// from the upstream response as a defense-in-depth measure.
func TestGetAtom_QuestionPayload_StripsCorrectOptionId_InLearnerMode(t *testing.T) {
	creation := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		// Upstream LEAKS correct_option_id — the BFF must strip it.
		_, _ = w.Write([]byte(`{
			"atom_id":"atom-001",
			"question_payload":{
				"type":"mcq",
				"question_id":"q-001",
				"options":[
					{"option_id":"opt_1","label":"A"},
					{"option_id":"opt_2","label":"B"}
				],
				"correct_option_id":"opt_1",
				"xp_on_correct":50
			}
		}`))
	})
	consumption := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	a := phyllis.New(phyllis.Config{CreationURL: creation.URL, ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GetAtom(context.Background(), phyllis.AuthCtx{Bearer: "x"}, "atom-001", "")
	if res.Status != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.Status, string(res.Body))
	}
	body := decodeJSON(t, strings.NewReader(string(res.Body)))
	atomBlk, _ := body["atom"].(map[string]any)
	qp, _ := atomBlk["question_payload"].(map[string]any)
	if _, present := qp["correct_option_id"]; present {
		t.Errorf("correct_option_id leaked in learner mode: %v", qp)
	}
}

// TestGetAtom_QuestionPayload_StripsCorrectOptionId_WhenAuthorModeWithoutRole
// — mode=author query param alone is NOT enough. The caller must ALSO hold
// the author/instructor role on the tenant. A non-privileged caller with
// mode=author still gets the learner-safe projection (silent fall-through,
// not a 403 — gateway.yaml documents this as deliberate).
func TestGetAtom_QuestionPayload_StripsCorrectOptionId_WhenAuthorModeWithoutRole(t *testing.T) {
	creation := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"atom_id":"atom-001",
			"question_payload":{
				"type":"mcq",
				"question_id":"q-001",
				"options":[{"option_id":"o1","label":"A"},{"option_id":"o2","label":"B"}],
				"correct_option_id":"o1",
				"xp_on_correct":10
			}
		}`))
	})
	consumption := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	a := phyllis.New(phyllis.Config{CreationURL: creation.URL, ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
	// Caller asks for author mode but has only learner role.
	auth := phyllis.AuthCtx{Bearer: "x", Roles: []string{"learner"}}
	res, _ := a.GetAtom(context.Background(), auth, "atom-001", "author")
	if res.Status != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.Status, string(res.Body))
	}
	body := decodeJSON(t, strings.NewReader(string(res.Body)))
	atomBlk, _ := body["atom"].(map[string]any)
	qp, _ := atomBlk["question_payload"].(map[string]any)
	if _, present := qp["correct_option_id"]; present {
		t.Errorf("correct_option_id leaked when mode=author + role=learner: %v", qp)
	}
}

// TestGetAtom_QuestionPayload_PreservesCorrectOptionId_WhenAuthorModeAndRole
// — when BOTH mode=author AND the caller holds an author/instructor role,
// the BFF preserves correct_option_id from the upstream response.
func TestGetAtom_QuestionPayload_PreservesCorrectOptionId_WhenAuthorModeAndRole(t *testing.T) {
	creation := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"atom_id":"atom-001",
			"question_payload":{
				"type":"mcq",
				"question_id":"q-001",
				"options":[{"option_id":"o1","label":"A"},{"option_id":"o2","label":"B"}],
				"correct_option_id":"o1",
				"xp_on_correct":10
			}
		}`))
	})
	consumption := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	a := phyllis.New(phyllis.Config{CreationURL: creation.URL, ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
	auth := phyllis.AuthCtx{Bearer: "x", Roles: []string{"author"}}
	res, _ := a.GetAtom(context.Background(), auth, "atom-001", "author")
	if res.Status != http.StatusOK {
		t.Fatalf("status=%d body=%s", res.Status, string(res.Body))
	}
	body := decodeJSON(t, strings.NewReader(string(res.Body)))
	atomBlk, _ := body["atom"].(map[string]any)
	qp, _ := atomBlk["question_payload"].(map[string]any)
	if got := qp["correct_option_id"]; got != "opt_1" && got != "o1" {
		// Accept either id (the upstream stub used o1; this assertion is permissive
		// in case the stub is later updated).
		t.Errorf("correct_option_id should be preserved for author + mode=author; got %v", got)
	}
}

// TestGetAtom_QuestionPayload_MalformedMCQ_Returns502 — upstream emits
// question_payload type=mcq with empty options[]. The BFF surfaces 502
// rather than letting the empty payload reach the FE (fail loud per
// feedback_no_stubs_real_wiring).
func TestGetAtom_QuestionPayload_MalformedMCQ_Returns502(t *testing.T) {
	creation := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"atom_id":"atom-001",
			"question_payload":{"type":"mcq","question_id":"q","options":[],"xp_on_correct":0}
		}`))
	})
	consumption := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	a := phyllis.New(phyllis.Config{CreationURL: creation.URL, ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GetAtom(context.Background(), phyllis.AuthCtx{Bearer: "x"}, "atom-001", "")
	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 for malformed mcq question_payload; body=%s", res.Status, string(res.Body))
	}
	if !strings.Contains(string(res.Body), "GATEWAY_UPSTREAM_5XX") {
		t.Errorf("expected GATEWAY_UPSTREAM_5XX envelope; got body=%s", string(res.Body))
	}
}

// TestGetAtom_NoQuestionPayload_PassesThrough — atom without question_payload
// is unmodified by the BFF (no contract violation, no field strip).
func TestGetAtom_NoQuestionPayload_PassesThrough(t *testing.T) {
	creation := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"atom_id":"atom-001","title":"Outline","body":"Read me"}`))
	})
	consumption := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"session_id":"s1"}`))
	})
	a := phyllis.New(phyllis.Config{CreationURL: creation.URL, ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GetAtom(context.Background(), phyllis.AuthCtx{Bearer: "x"}, "atom-001", "")
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d", res.Status)
	}
	body := decodeJSON(t, strings.NewReader(string(res.Body)))
	atomBlk, _ := body["atom"].(map[string]any)
	if _, present := atomBlk["question_payload"]; present {
		t.Errorf("question_payload should be absent; got %v", atomBlk["question_payload"])
	}
	// Atom block still readable + session block populated.
	if atomBlk["atom_id"] != "atom-001" {
		t.Errorf("atom_id missing in BFF envelope")
	}
}

// TestAuthCtx_HasAuthorRole — typed role checks (case-insensitive over a
// small canonical set; explicit reject for non-author roles).
func TestAuthCtx_HasAuthorRole(t *testing.T) {
	cases := []struct {
		roles []string
		want  bool
	}{
		{[]string{"author"}, true},
		{[]string{"instructor"}, true},
		{[]string{"Author", "learner"}, true},
		{[]string{"INSTRUCTOR"}, true},
		{[]string{"learner"}, false},
		{[]string{}, false},
		{nil, false},
		{[]string{"admin"}, false}, // admin is NOT an author role for this gate
	}
	for i, c := range cases {
		got := phyllis.AuthCtx{Roles: c.roles}.HasAuthorRole()
		if got != c.want {
			t.Errorf("case[%d] roles=%v: got %v, want %v", i, c.roles, got, c.want)
		}
	}
}

// -----------------------------------------------------------------------------
// SubmitFeedback — POST /atoms/{id}/feedback
// -----------------------------------------------------------------------------

func TestSubmitFeedback_HappyPath(t *testing.T) {
	consumption := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/atoms/atom-001/feedback" {
			t.Errorf("consumption: method=%s path=%s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"accepted":true,"sm2_interval_days":7}`))
	})
	a := phyllis.New(phyllis.Config{ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)

	res, err := a.SubmitFeedback(context.Background(), phyllis.AuthCtx{Bearer: "x"}, "atom-001", []byte(`{"quality":4}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusAccepted {
		t.Errorf("status = %d; want 202", res.Status)
	}
}

// -----------------------------------------------------------------------------
// GenerateAI — POST /api/ai/generate
// Per Tier 2 D6 + S3.1 audit P0 fix: BFF now calls Gateway:/llm/generate
// (LLM execution arm), NOT Router:/generate (which previously did canned
// generation, a Tier 2 D6 violation). When the Gateway URL is unset the
// aggregator falls back to the legacy Router URL for backwards compat.
// -----------------------------------------------------------------------------

func TestGenerateAI_PrefersGatewayOverRouter(t *testing.T) {
	// When both URLs are configured the Gateway wins. The Router is the
	// legacy fallback target.
	gatewayCalled := false
	gateway := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/llm/generate" {
			t.Errorf("gateway: method=%s path=%s", r.Method, r.URL.Path)
		}
		gatewayCalled = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"text":"world","prompt_tokens":5,"completion_tokens":7}`))
	})
	router := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("router should NOT be called when gateway URL is set")
		w.WriteHeader(http.StatusOK)
	})
	a := phyllis.New(phyllis.Config{
		ModelBrokerGatewayURL: gateway.URL,
		ModelBrokerRouterURL:  router.URL,
		PerCallTimeout:        2 * time.Second,
	}, nil)
	res, err := a.GenerateAI(context.Background(), phyllis.AuthCtx{Bearer: "x"}, []byte(`{"prompt":"hi","agent_id":"ai_assist","model_id":"gemini-2.5-flash"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d", res.Status)
	}
	if !gatewayCalled {
		t.Errorf("expected gateway path called")
	}
	if router.calls.Load() != 0 {
		t.Errorf("expected router not called; got %d", router.calls.Load())
	}
}

func TestGenerateAI_GatewayOnly_HappyPath(t *testing.T) {
	gateway := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/llm/generate" {
			t.Errorf("gateway: method=%s path=%s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"text":"world","prompt_tokens":5,"completion_tokens":7}`))
	})
	a := phyllis.New(phyllis.Config{ModelBrokerGatewayURL: gateway.URL, PerCallTimeout: 2 * time.Second}, nil)
	res, err := a.GenerateAI(context.Background(), phyllis.AuthCtx{Bearer: "x"}, []byte(`{"prompt":"Generate 10 MCQs","agent_id":"ai_assist","model_id":"gemini-2.5-flash"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d", res.Status)
	}
}

func TestGenerateAI_LegacyRouterFallback(t *testing.T) {
	// When ModelBrokerGatewayURL is unset, fall back to Router:/generate
	// for backwards compatibility while the Gateway is rolling out.
	router := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/generate" {
			t.Errorf("router: method=%s path=%s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"verdict":"approved","content":"10 MCQs..."}`))
	})
	a := phyllis.New(phyllis.Config{ModelBrokerRouterURL: router.URL, PerCallTimeout: 2 * time.Second}, nil)
	res, err := a.GenerateAI(context.Background(), phyllis.AuthCtx{Bearer: "x"}, []byte(`{"prompt":"Generate 10 MCQs"}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d", res.Status)
	}
}

func TestGenerateAI_NoUpstreamConfigured_Returns502(t *testing.T) {
	a := phyllis.New(phyllis.Config{PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GenerateAI(context.Background(), phyllis.AuthCtx{Bearer: "x"}, []byte(`{"prompt":"x"}`))
	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 when neither gateway nor router URL set", res.Status)
	}
}

func TestGenerateAI_GatewayDown_LegacyRouterFallback(t *testing.T) {
	// Optional behaviour: when Gateway returns 5xx and a fallback Router
	// is configured, the aggregator MUST fall back so demo flow stays
	// alive while Vertex API quota / outages are resolved.
	gateway := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	router := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/generate" {
			t.Errorf("router path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"verdict":"approved","content":"fallback"}`))
	})
	a := phyllis.New(phyllis.Config{
		ModelBrokerGatewayURL: gateway.URL,
		ModelBrokerRouterURL:  router.URL,
		PerCallTimeout:        2 * time.Second,
	}, nil)
	res, _ := a.GenerateAI(context.Background(), phyllis.AuthCtx{Bearer: "x"}, []byte(`{"prompt":"x"}`))
	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200 from fallback", res.Status)
	}
	if router.calls.Load() != 1 {
		t.Errorf("expected router fallback to be called; got %d", router.calls.Load())
	}
}

// -----------------------------------------------------------------------------
// GetCompanionMe + GetDailyDose
// -----------------------------------------------------------------------------

func TestGetCompanionMe_HappyPath(t *testing.T) {
	consumption := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/companion/me" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"companion_id":"fam-001","name":"Pip","level":4}`))
	})
	a := phyllis.New(phyllis.Config{ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, err := a.GetCompanionMe(context.Background(), phyllis.AuthCtx{Bearer: "x"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d", res.Status)
	}
}

func TestGetDailyDose_HappyPath(t *testing.T) {
	consumption := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/companion/daily-dose" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"atoms":["a1","a2","a3","a4","a5"]}`))
	})
	a := phyllis.New(phyllis.Config{ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GetDailyDose(context.Background(), phyllis.AuthCtx{Bearer: "x"}, "", "")
	if res.Status != http.StatusOK {
		t.Errorf("status = %d", res.Status)
	}
}

// TestGetDailyDose_ForwardsGrowthEdgeID — Phase 2A focused practice: a non-empty
// growthEdgeID is forwarded as ?growth_edge_id to the upstream daily-dose call so
// chora-consumption scopes the dose to drilling that one Growth Edge.
func TestGetDailyDose_ForwardsGrowthEdgeID(t *testing.T) {
	var gotQuery string
	consumption := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("growth_edge_id")
		_, _ = w.Write([]byte(`{"atoms":[]}`))
	})
	a := phyllis.New(phyllis.Config{ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
	_, _ = a.GetDailyDose(context.Background(), phyllis.AuthCtx{Bearer: "x"}, "019e0000-0000-7000-a000-000000000099", "")
	if gotQuery != "019e0000-0000-7000-a000-000000000099" {
		t.Errorf("upstream growth_edge_id = %q, want the forwarded edge id", gotQuery)
	}
}

// TestGetDailyDose_OmitsGrowthEdgeIDWhenEmpty — the unscoped call MUST NOT add a
// growth_edge_id query param (byte-identical to the pre-Phase-2A behaviour).
func TestGetDailyDose_OmitsGrowthEdgeIDWhenEmpty(t *testing.T) {
	var gotRawQuery string
	consumption := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"atoms":[]}`))
	})
	a := phyllis.New(phyllis.Config{ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
	_, _ = a.GetDailyDose(context.Background(), phyllis.AuthCtx{Bearer: "x"}, "", "")
	if gotRawQuery != "" {
		t.Errorf("upstream raw query = %q, want empty (no growth_edge_id on the unscoped call)", gotRawQuery)
	}
}

// TestGetDailyDose_StampsLowercaseGcidHeader pins the D1.5 fix: chora-consumption's
// requireContext reads X-Tenant-Id + the LOWERCASE `gcid` header. Without the
// lowercase gcid header, handleDailyDose returns "gcid required" — the Phyllis
// step 8 blocker. The Phyllis aggregator's outbound call MUST stamp `gcid`
// (in addition to the canonical chora-gcid mesh header) for every
// chora-consumption-bound route.
func TestGetDailyDose_StampsLowercaseGcidHeader(t *testing.T) {
	var gotGcid, gotTenant, gotChoraGcid string
	consumption := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotGcid = r.Header.Get("gcid")
		gotTenant = r.Header.Get("X-Tenant-Id")
		gotChoraGcid = r.Header.Get("chora-gcid")
		_, _ = w.Write([]byte(`{"atoms":[]}`))
	})
	a := phyllis.New(phyllis.Config{ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
	_, _ = a.GetDailyDose(context.Background(), phyllis.AuthCtx{
		Bearer:   "x",
		GCID:     "01970000-0000-7000-8000-000000001999",
		TenantID: "01970000-0000-7000-8000-0000000000bb",
	}, "", "")
	if gotGcid != "01970000-0000-7000-8000-000000001999" {
		t.Errorf("lowercase gcid header = %q; want the GCID — chora-consumption requireContext reads it", gotGcid)
	}
	if gotTenant != "01970000-0000-7000-8000-0000000000bb" {
		t.Errorf("X-Tenant-Id header = %q; want the TenantID", gotTenant)
	}
	// Canonical mesh header still rides along.
	if gotChoraGcid != "01970000-0000-7000-8000-000000001999" {
		t.Errorf("chora-gcid header = %q; want the GCID", gotChoraGcid)
	}
}

// TestGetCompanionMe_StampsLowercaseGcidHeader — same D1.5 invariant for the
// /api/companion/me route (handleCompanionMe also uses requireContext).
func TestGetCompanionMe_StampsLowercaseGcidHeader(t *testing.T) {
	var gotGcid string
	consumption := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotGcid = r.Header.Get("gcid")
		_, _ = w.Write([]byte(`{"companion_id":"fam-001"}`))
	})
	a := phyllis.New(phyllis.Config{ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
	_, _ = a.GetCompanionMe(context.Background(), phyllis.AuthCtx{
		Bearer: "x",
		GCID:   "01970000-0000-7000-8000-000000001999",
	})
	if gotGcid != "01970000-0000-7000-8000-000000001999" {
		t.Errorf("lowercase gcid header = %q; want the GCID", gotGcid)
	}
}

// TestGetKGClusters_FansOutToConsumption pins the FE-unblocker route per
// docs/m13/kg-fog-aplus-integration-backend-handoff-2026-05-13.md §1.1.
// The BFF proxies /api/v1/me/knowledge-graph/clusters to consumption.
func TestGetKGClusters_FansOutToConsumption(t *testing.T) {
	var gotPath string
	consumption := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"data":{"clusters":[],"capRemaining":3,"capMax":3}}`))
	})
	a := phyllis.New(phyllis.Config{ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GetKGClusters(context.Background(), phyllis.AuthCtx{
		Bearer: "x", TenantID: "tenant-x", GCID: "gcid-y",
	})
	if res.Status != http.StatusOK {
		t.Errorf("status = %d", res.Status)
	}
	if gotPath != "/v1/me/knowledge-graph/clusters" {
		t.Errorf("downstream path = %q; want /v1/me/knowledge-graph/clusters", gotPath)
	}
}

// TestCreateKGCluster_FansOutToConsumption pins §1.2 POST fan-out.
func TestCreateKGCluster_FansOutToConsumption(t *testing.T) {
	var gotPath, gotBody string
	consumption := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"clusterId":"cl-1"}}`))
	})
	a := phyllis.New(phyllis.Config{ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.CreateKGCluster(context.Background(),
		phyllis.AuthCtx{Bearer: "x", TenantID: "tenant-x", GCID: "gcid-y"},
		[]byte(`{"seedTopic":"agile"}`))
	if res.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", res.Status)
	}
	if gotPath != "/v1/me/knowledge-graph/clusters" {
		t.Errorf("downstream path = %q", gotPath)
	}
	if gotBody != `{"seedTopic":"agile"}` {
		t.Errorf("downstream body = %q", gotBody)
	}
}

// -----------------------------------------------------------------------------
// Aggregation budget — total elapsed > AggregationBudget returns 504
// -----------------------------------------------------------------------------

func TestAggregationBudget_ExceededReturns504(t *testing.T) {
	creation := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(80 * time.Millisecond)
		_, _ = w.Write([]byte(`{"atom_id":"x"}`))
	})
	consumption := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(80 * time.Millisecond)
		_, _ = w.Write([]byte(`{"session_id":"s"}`))
	})
	a := phyllis.New(phyllis.Config{
		CreationURL:       creation.URL,
		ConsumptionURL:    consumption.URL,
		PerCallTimeout:    500 * time.Millisecond,
		AggregationBudget: 20 * time.Millisecond, // unrealistically tight to force timeout
	}, nil)
	res, err := a.GetAtom(context.Background(), phyllis.AuthCtx{Bearer: "x"}, "x", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusGatewayTimeout {
		t.Errorf("status = %d; want 504 when aggregation budget blown", res.Status)
	}
}

// -----------------------------------------------------------------------------
// Defaults — zero PerCallTimeout / AggregationBudget use 5s / 15s defaults
// -----------------------------------------------------------------------------

func TestConfig_AppliesDefaults(t *testing.T) {
	c := phyllis.Config{}
	c.ApplyDefaults()
	if c.PerCallTimeout != 5*time.Second {
		t.Errorf("PerCallTimeout = %v; want 5s default", c.PerCallTimeout)
	}
	if c.AggregationBudget != 15*time.Second {
		t.Errorf("AggregationBudget = %v; want 15s default", c.AggregationBudget)
	}
}

// -----------------------------------------------------------------------------
// LoadConfigFromEnv — env vars wired
// -----------------------------------------------------------------------------

func TestLoadConfigFromEnv(t *testing.T) {
	t.Setenv("SVC_IDENTITY_URL", "http://identity")
	t.Setenv("SVC_TENANCY_URL", "http://tenancy")
	t.Setenv("SVC_CREATION_URL", "http://creation")
	t.Setenv("SVC_DELIVERY_URL", "http://delivery")
	t.Setenv("SVC_CONSUMPTION_URL", "http://consumption")
	t.Setenv("SVC_MODEL_BROKER_ROUTER_URL", "http://mbr")
	t.Setenv("SVC_MODEL_BROKER_GATEWAY_URL", "http://mbg")

	c := phyllis.LoadConfigFromEnv()
	if c.IdentityURL != "http://identity" {
		t.Errorf("identity = %s", c.IdentityURL)
	}
	if c.TenancyURL != "http://tenancy" {
		t.Errorf("tenancy = %s", c.TenancyURL)
	}
	if c.CreationURL != "http://creation" {
		t.Errorf("creation = %s", c.CreationURL)
	}
	if c.DeliveryURL != "http://delivery" {
		t.Errorf("delivery = %s", c.DeliveryURL)
	}
	if c.ConsumptionURL != "http://consumption" {
		t.Errorf("consumption = %s", c.ConsumptionURL)
	}
	if c.ModelBrokerRouterURL != "http://mbr" {
		t.Errorf("mbr = %s", c.ModelBrokerRouterURL)
	}
	if c.ModelBrokerGatewayURL != "http://mbg" {
		t.Errorf("mbg = %s", c.ModelBrokerGatewayURL)
	}
}

// CHO-1799 — chora-gateway has two aggregators that resolve the
// chora-payments upstream from different env vars (phyllis → SVC_PAYMENTS_URL,
// gatewayproxy → CHORA_PAYMENTS_HTTP_ADDR). Every K8s deployment.yaml today
// only sets CHORA_PAYMENTS_HTTP_ADDR so the phyllis aggregator's PaymentsURL
// was "" → /h/billing 502'd silently. Fallback contract: SVC_PAYMENTS_URL
// wins when set; otherwise reuse CHORA_PAYMENTS_HTTP_ADDR.
func TestLoadConfigFromEnv_PaymentsURL_PrefersSVCThenFallsBackToCHORA(t *testing.T) {
	t.Run("SVC_PAYMENTS_URL set + CHORA_PAYMENTS_HTTP_ADDR unset", func(t *testing.T) {
		t.Setenv("SVC_PAYMENTS_URL", "http://svc-payments")
		t.Setenv("CHORA_PAYMENTS_HTTP_ADDR", "")
		c := phyllis.LoadConfigFromEnv()
		if c.PaymentsURL != "http://svc-payments" {
			t.Errorf("PaymentsURL = %q, want http://svc-payments", c.PaymentsURL)
		}
	})

	t.Run("SVC_PAYMENTS_URL unset + CHORA_PAYMENTS_HTTP_ADDR set (the bug case)", func(t *testing.T) {
		t.Setenv("SVC_PAYMENTS_URL", "")
		t.Setenv("CHORA_PAYMENTS_HTTP_ADDR", "http://chora-payments")
		c := phyllis.LoadConfigFromEnv()
		if c.PaymentsURL != "http://chora-payments" {
			t.Errorf("PaymentsURL = %q, want http://chora-payments (fallback)", c.PaymentsURL)
		}
	})

	t.Run("both set — SVC_PAYMENTS_URL wins (explicit override)", func(t *testing.T) {
		t.Setenv("SVC_PAYMENTS_URL", "http://svc-payments")
		t.Setenv("CHORA_PAYMENTS_HTTP_ADDR", "http://chora-payments")
		c := phyllis.LoadConfigFromEnv()
		if c.PaymentsURL != "http://svc-payments" {
			t.Errorf("PaymentsURL = %q, want http://svc-payments (explicit wins)", c.PaymentsURL)
		}
	})

	t.Run("both unset — empty (no magic default)", func(t *testing.T) {
		t.Setenv("SVC_PAYMENTS_URL", "")
		t.Setenv("CHORA_PAYMENTS_HTTP_ADDR", "")
		c := phyllis.LoadConfigFromEnv()
		if c.PaymentsURL != "" {
			t.Errorf("PaymentsURL = %q, want empty when both unset", c.PaymentsURL)
		}
	})
}

// -----------------------------------------------------------------------------
// Internal — IsAuthError + IsServerError helpers (sentinels)
// -----------------------------------------------------------------------------

func TestPassThroughErrorClassification(t *testing.T) {
	if !phyllis.IsAuthStatus(http.StatusUnauthorized) {
		t.Error("401 should be IsAuthStatus")
	}
	if !phyllis.IsAuthStatus(http.StatusForbidden) {
		t.Error("403 should be IsAuthStatus")
	}
	if phyllis.IsAuthStatus(http.StatusOK) {
		t.Error("200 must NOT be IsAuthStatus")
	}
}

// -----------------------------------------------------------------------------
// Aggregator handles a context cancellation upstream - returns context error
// -----------------------------------------------------------------------------

func TestContextCanceled_Propagates(t *testing.T) {
	identity := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: 1 * time.Second}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	res, err := a.GetMe(ctx, phyllis.AuthCtx{Bearer: "x"})
	// Either the call exits with a 504/canceled or returns an error — both
	// acceptable; assert non-zero status or non-nil error so the BFF surface
	// can react.
	if err == nil && res.Status == http.StatusOK {
		t.Errorf("expected error or non-200 on cancel; got status=%d err=%v", res.Status, err)
	}
	_ = errors.New
}
