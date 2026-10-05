// governance_client_test.go — Phase C tests for the real-wire governance
// client. Covers:
//
//   - GetIMDADashboard via a fake gRPC client (4-dimension scoring)
//   - QueryAuditEvents via a fake gRPC client
//   - GetHITLPending via httptest server (both envelope shapes)
//   - GetDimensionRubric via httptest server
//   - error paths (nil client + 5xx + transport failure + malformed JSON)
package upstream_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	governancev1 "github.com/apollo-chora/chora-contracts/gen/go/chora/services/governance/v1"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

// fakeGovernanceGRPC implements upstream.GovernanceGRPCClient and records
// the last request seen.
type fakeGovernanceGRPC struct {
	dashboardResp *governancev1.GetIMDADashboardResponse
	dashboardErr  error
	auditResp     *governancev1.QueryAuditEventsResponse
	auditErr      error

	lastDashboardReq *governancev1.GetIMDADashboardRequest
	lastAuditReq     *governancev1.QueryAuditEventsRequest
}

func (f *fakeGovernanceGRPC) GetIMDADashboard(_ context.Context, in *governancev1.GetIMDADashboardRequest, _ ...grpc.CallOption) (*governancev1.GetIMDADashboardResponse, error) {
	f.lastDashboardReq = in
	return f.dashboardResp, f.dashboardErr
}

func (f *fakeGovernanceGRPC) QueryAuditEvents(_ context.Context, in *governancev1.QueryAuditEventsRequest, _ ...grpc.CallOption) (*governancev1.QueryAuditEventsResponse, error) {
	f.lastAuditReq = in
	return f.auditResp, f.auditErr
}

func TestGovernanceClient_GetIMDADashboard_Happy(t *testing.T) {
	rpc := &fakeGovernanceGRPC{
		dashboardResp: &governancev1.GetIMDADashboardResponse{
			TenantId: "tenant-1",
			Dimensions: []*governancev1.IMDADimensionAssessment{
				{Dimension: governancev1.IMDADimension_IMDA_DIMENSION_ACCOUNTABILITY, Score: 72, Indicators: []string{"#8 Data Governance"}},
				{Dimension: governancev1.IMDADimension_IMDA_DIMENSION_TRANSPARENCY, Score: 58, Indicators: []string{}},
				{Dimension: governancev1.IMDADimension_IMDA_DIMENSION_SAFETY_AND_ROBUSTNESS, Score: 81},
				{Dimension: governancev1.IMDADimension_IMDA_DIMENSION_FAIRNESS_AND_HUMAN_OVERSIGHT, Score: 64},
			},
		},
	}
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{PerCallTimeout: 1 * time.Second},
		RPC:      rpc,
	})
	dims, err := gc.GetIMDADashboard(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rpc.lastDashboardReq == nil || rpc.lastDashboardReq.GetTenantId() != "tenant-1" {
		t.Errorf("dashboard req tenant mismatch: %+v", rpc.lastDashboardReq)
	}
	if len(dims) != 4 {
		t.Fatalf("len(dims) = %d; want 4", len(dims))
	}
	ids := map[string]int32{}
	for _, d := range dims {
		ids[d.ID] = d.ScorePct
	}
	if ids["accountability"] != 72 || ids["transparency"] != 58 ||
		ids["safety_and_robustness"] != 81 || ids["fairness_and_human_oversight"] != 64 {
		t.Errorf("scores mismatch: %v", ids)
	}
}

func TestGovernanceClient_GetIMDADashboard_NilRPC(t *testing.T) {
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{PerCallTimeout: 1 * time.Second},
		RPC:      nil,
	})
	if _, err := gc.GetIMDADashboard(context.Background(), "tenant-1"); !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("err = %v; want ErrUpstream wrap", err)
	}
}

func TestGovernanceClient_QueryAuditEvents_Happy(t *testing.T) {
	now := time.Now().UTC()
	rpc := &fakeGovernanceGRPC{
		auditResp: &governancev1.QueryAuditEventsResponse{
			Events: []*governancev1.AuditEvent{
				{
					EventId:   "evt-1",
					TenantId:  "tenant-1",
					Gcid:      "gcid-1",
					Action:    "atom.publish",
					Decision:  governancev1.AuditDecision_AUDIT_DECISION_PERMITTED,
					CreatedAt: timestamppb.New(now),
				},
				{
					EventId:   "evt-2",
					TenantId:  "tenant-1",
					Action:    "policy.evaluate",
					Decision:  governancev1.AuditDecision_AUDIT_DECISION_DENIED,
					Reason:    "policy match",
					CreatedAt: timestamppb.New(now.Add(-1 * time.Minute)),
				},
			},
		},
	}
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{PerCallTimeout: 1 * time.Second},
		RPC:      rpc,
	})
	ev, err := gc.QueryAuditEvents(context.Background(), "tenant-1", 10)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(ev) != 2 {
		t.Fatalf("len(ev) = %d; want 2", len(ev))
	}
	if ev[0].Decision != "permitted" || ev[1].Decision != "denied" {
		t.Errorf("decisions = %s,%s; want permitted,denied", ev[0].Decision, ev[1].Decision)
	}
	if ev[1].Reason != "policy match" {
		t.Errorf("reason = %q; want %q", ev[1].Reason, "policy match")
	}
}

func TestGovernanceClient_GetHITLPending_BothEnvelopeShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"items envelope", `{"items":[{"decision_id":"d1","tenant_id":"t1","agent_id":"a1","created_at":"2026-05-26T01:00:00Z"}]}`},
		{"bare array", `[{"decision_id":"d1","tenant_id":"t1","agent_id":"a1","created_at":"2026-05-26T01:00:00Z"}]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/hitl/pending" {
					t.Errorf("path = %s; want /api/hitl/pending", r.URL.Path)
				}
				if r.URL.Query().Get("tenant_id") != "t1" {
					t.Errorf("tenant_id = %s; want t1", r.URL.Query().Get("tenant_id"))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
				Endpoint: upstream.GovernanceConfig{HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second},
			})
			items, err := gc.GetHITLPending(context.Background(), "t1", 50)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if len(items) != 1 {
				t.Fatalf("len(items) = %d; want 1", len(items))
			}
			if items[0].DecisionID != "d1" {
				t.Errorf("decision_id = %s; want d1", items[0].DecisionID)
			}
		})
	}
}

func TestGovernanceClient_GetHITLPending_NoAddr(t *testing.T) {
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{},
	})
	if _, err := gc.GetHITLPending(context.Background(), "t1", 0); !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("err = %v; want ErrUpstream wrap", err)
	}
}

func TestGovernanceClient_GetHITLPending_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second},
	})
	if _, err := gc.GetHITLPending(context.Background(), "t1", 0); !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("err = %v; want ErrUpstream wrap", err)
	}
}

func TestGovernanceClient_GetDimensionRubric_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/imda/dimensions/accountability/rubric" {
			t.Errorf("path = %s; want .../accountability/rubric", r.URL.Path)
		}
		// Per-spec: governance /api/* require the gcid header (GOV_GCID_REQUIRED).
		if got := r.Header.Get("gcid"); got != "g1" {
			t.Errorf("gcid header = %q; want g1", got)
		}
		if got := r.Header.Get("X-Tenant-Id"); got != "t1" {
			t.Errorf("X-Tenant-Id header = %q; want t1", got)
		}
		w.Header().Set("Content-Type", "application/json")
		// Mirror chora-governance's REAL dimensionRubricResponse shape:
		// `dimension` + `items` + `pass_rate` (0..1) — NOT the BFF's own struct.
		// Guards the tag mapping (regression: BFF expected `rubric_items`/
		// `score_pct` → empty drilldowns + 0 score on a live 200).
		_, _ = w.Write([]byte(`{"dimension":"accountability","items":[` +
			`{"id":"ri-1","title":"Audit trail coverage","status":"PASS"},` +
			`{"id":"ri-2","title":"RACI doc","status":"PARTIAL"},` +
			`{"id":"ri-3","title":"RACI ratified","status":"FAIL"}],` +
			`"total":3,"pass_rate":0.5}`))
	}))
	defer srv.Close()
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second},
	})
	ctx := upstream.WithAuthCtx(context.Background(), upstream.AuthCtx{TenantID: "t1", GCID: "g1"})
	rub, err := gc.GetDimensionRubric(ctx, "t1", "accountability")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rub == nil || rub.ID != "accountability" || len(rub.RubricItems) != 3 {
		t.Errorf("rub = %+v; want 3 rubric items mapped from `items`", rub)
	}
	if rub != nil && rub.ScorePct != 50 {
		t.Errorf("ScorePct = %d; want 50 (governance pass_rate 0.5 * 100)", rub.ScorePct)
	}
}

func TestGovernanceClient_GetDimensionRubric_EmptyDimension(t *testing.T) {
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: "http://example", PerCallTimeout: 1 * time.Second},
	})
	if _, err := gc.GetDimensionRubric(context.Background(), "t1", "  "); !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("err = %v; want ErrUpstream wrap", err)
	}
}

// TestLoadGovernanceConfigFromEnv smoke-checks the env loader.
func TestLoadGovernanceConfigFromEnv(t *testing.T) {
	t.Setenv("CHORA_GOVERNANCE_GRPC_ADDR", "chora-governance:9090")
	t.Setenv("CHORA_GOVERNANCE_HTTP_ADDR", "http://chora-governance:8080")
	c := upstream.LoadGovernanceConfigFromEnv()
	if c.GRPCAddr != "chora-governance:9090" {
		t.Errorf("grpc addr = %s", c.GRPCAddr)
	}
	if c.HTTPAddr != "http://chora-governance:8080" {
		t.Errorf("http addr = %s", c.HTTPAddr)
	}
	if c.PerCallTimeout != upstream.DefaultGovernanceCallTimeout {
		t.Errorf("timeout = %v; want default", c.PerCallTimeout)
	}
}

// TestCanonicalIMDADimensions guards against accidental ADR-141 label
// drift.
func TestCanonicalIMDADimensions(t *testing.T) {
	expected := []string{
		"accountability",
		"transparency",
		"safety_and_robustness",
		"fairness_and_human_oversight",
	}
	if len(upstream.CanonicalIMDADimensions) != len(expected) {
		t.Fatalf("len = %d; want 4", len(upstream.CanonicalIMDADimensions))
	}
	for i, want := range expected {
		if upstream.CanonicalIMDADimensions[i] != want {
			t.Errorf("[%d] = %s; want %s", i, upstream.CanonicalIMDADimensions[i], want)
		}
	}
}

// fmt is used only to silence imports when the package is otherwise
// minimal.
var _ = fmt.Sprintf

// -----------------------------------------------------------------------------
// HITL claim + release (N13 — BFF self-claim passthrough)
// -----------------------------------------------------------------------------
//
// N7 shipped POST /api/hitl/decisions/{id}/{claim|release} on chora-governance.
// The BFF passthrough client wraps them with typed errors so the BFF handler
// can map upstream 404/409/403/422/503 to FE-facing statuses cleanly.

func TestGovernanceClient_ClaimHITLDecision_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s; want POST", r.Method)
		}
		if r.URL.Path != "/api/hitl/decisions/d-1/claim" {
			t.Errorf("path = %s; want /api/hitl/decisions/d-1/claim", r.URL.Path)
		}
		if got := r.Header.Get("X-Tenant-Id"); got != "t1" {
			t.Errorf("X-Tenant-Id = %s; want t1", got)
		}
		// Body must carry the operator_gcid.
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["operator_gcid"] != "auditor-gcid-1" {
			t.Errorf("body.operator_gcid = %v; want auditor-gcid-1", body["operator_gcid"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"decision_id":"d-1","run_id":"r-1","assignee_gcid":"auditor-gcid-1","decision":"pending","autonomy_level":"hitl_l1","lifecycle_stage":"runtime","decided_at":""}`))
	}))
	defer srv.Close()

	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second},
	})
	item, err := gc.ClaimHITLDecision(context.Background(), "t1", "d-1", "auditor-gcid-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if item == nil {
		t.Fatal("item nil")
	}
	if item.DecisionID != "d-1" {
		t.Errorf("decision_id = %s; want d-1", item.DecisionID)
	}
	if item.AssigneeGcid == nil || *item.AssigneeGcid != "auditor-gcid-1" {
		t.Errorf("assignee_gcid = %v; want auditor-gcid-1", item.AssigneeGcid)
	}
}

func TestGovernanceClient_ClaimHITLDecision_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"GOV_HITL_NOT_FOUND","message":"hitl decision not found"}}`))
	}))
	defer srv.Close()
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second},
	})
	if _, err := gc.ClaimHITLDecision(context.Background(), "t1", "d-missing", "auditor-1"); !errors.Is(err, upstream.ErrHITLNotFound) {
		t.Errorf("err = %v; want ErrHITLNotFound", err)
	}
}

func TestGovernanceClient_ClaimHITLDecision_Conflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"GOV_HITL_ALREADY_CLAIMED"}}`))
	}))
	defer srv.Close()
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second},
	})
	if _, err := gc.ClaimHITLDecision(context.Background(), "t1", "d-1", "auditor-1"); !errors.Is(err, upstream.ErrHITLConflict) {
		t.Errorf("err = %v; want ErrHITLConflict", err)
	}
}

func TestGovernanceClient_ClaimHITLDecision_Unprocessable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer srv.Close()
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second},
	})
	if _, err := gc.ClaimHITLDecision(context.Background(), "t1", "d-1", "  "); !errors.Is(err, upstream.ErrHITLInvalid) {
		t.Errorf("err = %v; want ErrHITLInvalid", err)
	}
}

func TestGovernanceClient_ClaimHITLDecision_Unavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second},
	})
	if _, err := gc.ClaimHITLDecision(context.Background(), "t1", "d-1", "auditor-1"); !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("err = %v; want ErrUpstream wrap", err)
	}
}

func TestGovernanceClient_ReleaseHITLDecision_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s; want POST", r.Method)
		}
		if r.URL.Path != "/api/hitl/decisions/d-1/release" {
			t.Errorf("path = %s; want .../release", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision_id":"d-1","run_id":"r-1","assignee_gcid":null,"decision":"pending","autonomy_level":"hitl_l1","lifecycle_stage":"runtime","decided_at":""}`))
	}))
	defer srv.Close()
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second},
	})
	item, err := gc.ReleaseHITLDecision(context.Background(), "t1", "d-1", "auditor-gcid-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if item == nil || item.DecisionID != "d-1" {
		t.Errorf("item = %+v; want decision_id=d-1", item)
	}
	if item.AssigneeGcid != nil {
		t.Errorf("assignee_gcid = %v; want nil (released)", item.AssigneeGcid)
	}
}

func TestGovernanceClient_ReleaseHITLDecision_Forbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"GOV_HITL_NOT_ASSIGNEE"}}`))
	}))
	defer srv.Close()
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second},
	})
	if _, err := gc.ReleaseHITLDecision(context.Background(), "t1", "d-1", "auditor-1"); !errors.Is(err, upstream.ErrHITLForbidden) {
		t.Errorf("err = %v; want ErrHITLForbidden", err)
	}
}

func TestGovernanceClient_ClaimHITLDecision_NoAddr(t *testing.T) {
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{},
	})
	if _, err := gc.ClaimHITLDecision(context.Background(), "t1", "d-1", "auditor-1"); !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("err = %v; want ErrUpstream", err)
	}
}

func TestGovernanceClient_ApproveHITLDecision_Happy_ForwardsNote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s; want POST", r.Method)
		}
		if r.URL.Path != "/api/hitl/decisions/d-1/approve" {
			t.Errorf("path = %s; want /api/hitl/decisions/d-1/approve", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["operator_gcid"] != "auditor-gcid-1" {
			t.Errorf("body.operator_gcid = %v; want auditor-gcid-1", body["operator_gcid"])
		}
		if body["note"] != "looks good" {
			t.Errorf("body.note = %v; want 'looks good'", body["note"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision_id":"d-1","run_id":"r-1","assignee_gcid":"auditor-gcid-1","decision":"approved","autonomy_level":"hitl_l1","lifecycle_stage":"runtime","decided_at":"2026-06-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second},
	})
	item, err := gc.ApproveHITLDecision(context.Background(), "t1", "d-1", "auditor-gcid-1", "looks good")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if item == nil || item.Decision != "approved" {
		t.Errorf("item = %+v; want decision=approved", item)
	}
}

// When note is empty the JSON body must OMIT the note key entirely (so the
// upstream sees the same shape claim/release send).
func TestGovernanceClient_ApproveHITLDecision_OmitsEmptyNote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if _, has := body["note"]; has {
			t.Errorf("note key should be omitted when empty; body=%+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision_id":"d-1","decision":"approved"}`))
	}))
	defer srv.Close()
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second},
	})
	if _, err := gc.ApproveHITLDecision(context.Background(), "t1", "d-1", "auditor-1", ""); err != nil {
		t.Fatalf("err: %v", err)
	}
}

func TestGovernanceClient_RejectHITLDecision_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hitl/decisions/d-1/reject" {
			t.Errorf("path = %s; want .../reject", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision_id":"d-1","run_id":"r-1","assignee_gcid":"auditor-gcid-1","decision":"rejected","autonomy_level":"hitl_l1","lifecycle_stage":"runtime","decided_at":"2026-06-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second},
	})
	item, err := gc.RejectHITLDecision(context.Background(), "t1", "d-1", "auditor-gcid-1", "off-policy")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if item == nil || item.Decision != "rejected" {
		t.Errorf("item = %+v; want decision=rejected", item)
	}
}

func TestGovernanceClient_RejectHITLDecision_Forbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second},
	})
	if _, err := gc.RejectHITLDecision(context.Background(), "t1", "d-1", "auditor-1", ""); !errors.Is(err, upstream.ErrHITLForbidden) {
		t.Errorf("err = %v; want ErrHITLForbidden", err)
	}
}

func TestGovernanceClient_ApproveHITLDecision_Conflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second},
	})
	if _, err := gc.ApproveHITLDecision(context.Background(), "t1", "d-1", "auditor-1", "note"); !errors.Is(err, upstream.ErrHITLConflict) {
		t.Errorf("err = %v; want ErrHITLConflict (terminal verdict)", err)
	}
}

func TestGovernanceClient_ApproveHITLDecision_NoAddr(t *testing.T) {
	gc := upstream.NewGovernanceClient(upstream.GovernanceClientConfig{Endpoint: upstream.GovernanceConfig{}})
	if _, err := gc.ApproveHITLDecision(context.Background(), "t1", "d-1", "auditor-1", "n"); !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("err = %v; want ErrUpstream", err)
	}
}
