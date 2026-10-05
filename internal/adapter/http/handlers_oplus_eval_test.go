// handlers_oplus_eval_test.go — coverage for the O+ Agent-Eval BFF routes
// (/bff/oplus/eval-runs + /bff/oplus/eval-runs/{candidateLabel}): real-wire
// against an httptest chora-observability mock; assert the FE-canonical
// reshape + the BigQuery / Vertex Experiments console deep-links.
package httpadapter_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

func newEvalObsMock(t *testing.T, capture *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			*capture = r.URL.String()
		}
		switch {
		case r.URL.Path == "/api/v1/observability/eval-runs":
			_ = json.NewEncoder(w).Encode(upstream.EvalRunsResult{Runs: []upstream.EvalRun{{
				CandidateLabel: "depbump0607",
				LastRecordedAt: "2026-06-06T20:59:00Z",
				Members: []upstream.EvalRunMember{
					{Experiment: "chora-agent-eval-qgen-critic", AutoraterMetrics: []upstream.EvalMetricSummary{{Metric: "safety", Count: 4, AvgScore: 1}}, Adversarial: nil},
					{Experiment: "chora-agent-eval-qgen-question", AutoraterMetrics: []upstream.EvalMetricSummary{{Metric: "safety", Count: 4, AvgScore: 1}}, Adversarial: &upstream.EvalAdversarialSummary{Total: 6, Blocked: 6, Leaked: 0}},
				},
			}}})
		case strings.HasPrefix(r.URL.Path, "/api/v1/observability/eval-runs/"):
			_ = json.NewEncoder(w).Encode(upstream.EvalRunEvidenceResult{
				CandidateLabel: "depbump0607",
				Rows: []upstream.EvalEvidenceRow{
					{Experiment: "chora-agent-eval-qgen-question", Kind: "autorater", CaseID: "c1", Metric: "safety", Score: 1, Explanation: "ok", Prompt: "p", Response: "r", RecordedAt: "2026-06-06T20:59:00Z"},
					{Experiment: "chora-agent-eval-qgen-question", Kind: "adversarial", CaseID: "a1", Metric: "jailbreak", Score: 1, AdversarialVerdict: "BLOCKED(pass)", Explanation: "refused", Prompt: "attack", Response: "no", RecordedAt: "2026-06-06T20:59:00Z"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newEvalOPlusHandler(t *testing.T, obsURL string) *httpadapter.OPlusHandler {
	t.Helper()
	obsClient := upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: obsURL, PerCallTimeout: 2 * time.Second,
	}, nil)
	return httpadapter.NewOPlusHandler(nil, obsClient, nil, nil)
}

type evalRunsBody struct {
	State string `json:"state"`
	Runs  []struct {
		CandidateLabel string  `json:"candidate_label"`
		BigQueryURL    *string `json:"bigquery_url"`
		Members        []struct {
			Experiment          string  `json:"experiment"`
			MemberLabel         string  `json:"member_label"`
			VertexExperimentURL *string `json:"vertex_experiment_url"`
			Adversarial         *struct {
				Blocked int `json:"blocked"`
			} `json:"adversarial"`
		} `json:"members"`
	} `json:"runs"`
}

func TestOPlusHandler_EvalRuns_HappyPathWithDeepLinks(t *testing.T) {
	obs := newEvalObsMock(t, nil)
	t.Cleanup(obs.Close)
	h := newEvalOPlusHandler(t, obs.URL)

	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/eval-runs", nil)
	rr := httptest.NewRecorder()
	h.EvalRuns(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", rr.Code, rr.Body.String())
	}
	var body evalRunsBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.State != "live" {
		t.Errorf("state = %q; want live", body.State)
	}
	if len(body.Runs) != 1 {
		t.Fatalf("runs = %d; want 1", len(body.Runs))
	}
	run := body.Runs[0]
	if run.CandidateLabel != "depbump0607" {
		t.Errorf("candidate_label = %q", run.CandidateLabel)
	}
	if run.BigQueryURL == nil || !strings.Contains(*run.BigQueryURL, "agent_eval_rows") {
		t.Errorf("bigquery_url deep-link missing/wrong: %v", run.BigQueryURL)
	}
	if len(run.Members) != 2 {
		t.Fatalf("members = %d; want 2", len(run.Members))
	}
	critic, question := run.Members[0], run.Members[1]
	if critic.MemberLabel != "qgen-critic" || question.MemberLabel != "qgen-question" {
		t.Errorf("member_label derivation: %q / %q", critic.MemberLabel, question.MemberLabel)
	}
	if question.VertexExperimentURL == nil || !strings.Contains(*question.VertexExperimentURL, "chora-agent-eval-qgen-question") {
		t.Errorf("vertex_experiment_url deep-link missing/wrong: %v", question.VertexExperimentURL)
	}
	if critic.Adversarial != nil {
		t.Errorf("critic adversarial = %+v; want null", critic.Adversarial)
	}
	if question.Adversarial == nil || question.Adversarial.Blocked != 6 {
		t.Errorf("question adversarial = %+v", question.Adversarial)
	}
}

func TestOPlusHandler_EvalRuns_MethodNotAllowed(t *testing.T) {
	obs := newEvalObsMock(t, nil)
	t.Cleanup(obs.Close)
	h := newEvalOPlusHandler(t, obs.URL)
	req := httptest.NewRequest(http.MethodPost, "/bff/oplus/eval-runs", nil)
	rr := httptest.NewRecorder()
	h.EvalRuns(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d; want 405", rr.Code)
	}
}

func TestOPlusHandler_EvalRuns_UpstreamErrorEnvelope(t *testing.T) {
	obs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(obs.Close)
	h := newEvalOPlusHandler(t, obs.URL)
	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/eval-runs", nil)
	rr := httptest.NewRecorder()
	h.EvalRuns(rr, req)
	// Read routes collapse upstream errors to a 200 state:error envelope.
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (error envelope)", rr.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body["state"] != "error" {
		t.Errorf("state = %v; want error", body["state"])
	}
}

func TestOPlusHandler_EvalRuns_ForwardsFilters(t *testing.T) {
	var captured string
	obs := newEvalObsMock(t, &captured)
	t.Cleanup(obs.Close)
	h := newEvalOPlusHandler(t, obs.URL)
	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/eval-runs?experiment=chora-agent-eval-qgen-critic&kind=autorater", nil)
	rr := httptest.NewRecorder()
	h.EvalRuns(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if !strings.Contains(captured, "experiment=chora-agent-eval-qgen-critic") || !strings.Contains(captured, "kind=autorater") {
		t.Errorf("filters not forwarded upstream: %q", captured)
	}
}

func TestOPlusHandler_EvalRunEvidence_HappyPath(t *testing.T) {
	obs := newEvalObsMock(t, nil)
	t.Cleanup(obs.Close)
	h := newEvalOPlusHandler(t, obs.URL)
	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/eval-runs/depbump0607", nil)
	rr := httptest.NewRecorder()
	h.EvalRunEvidence(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		State          string `json:"state"`
		CandidateLabel string `json:"candidate_label"`
		Rows           []struct {
			Kind               string `json:"kind"`
			AdversarialVerdict string `json:"adversarial_verdict"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.State != "live" || body.CandidateLabel != "depbump0607" || len(body.Rows) != 2 {
		t.Fatalf("body = %+v", body)
	}
	var sawAdv bool
	for _, row := range body.Rows {
		if row.Kind == "adversarial" {
			sawAdv = true
			if row.AdversarialVerdict != "BLOCKED(pass)" {
				t.Errorf("adversarial verdict = %q", row.AdversarialVerdict)
			}
		}
	}
	if !sawAdv {
		t.Error("no adversarial row")
	}
}

func TestOPlusHandler_EvalRunEvidence_MissingLabel404(t *testing.T) {
	obs := newEvalObsMock(t, nil)
	t.Cleanup(obs.Close)
	h := newEvalOPlusHandler(t, obs.URL)
	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/eval-runs/", nil)
	rr := httptest.NewRecorder()
	h.EvalRunEvidence(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d; want 404", rr.Code)
	}
}

func TestOPlusHandler_EvalRunEvidence_ForwardsLabelAndFilters(t *testing.T) {
	var captured string
	obs := newEvalObsMock(t, &captured)
	t.Cleanup(obs.Close)
	h := newEvalOPlusHandler(t, obs.URL)
	req := httptest.NewRequest(http.MethodGet, "/bff/oplus/eval-runs/depbump0607?kind=adversarial&limit=10", nil)
	rr := httptest.NewRecorder()
	h.EvalRunEvidence(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if !strings.Contains(captured, "/api/v1/observability/eval-runs/depbump0607") {
		t.Errorf("label not forwarded in path: %q", captured)
	}
	if !strings.Contains(captured, "kind=adversarial") || !strings.Contains(captured, "limit=10") {
		t.Errorf("filters not forwarded: %q", captured)
	}
}
