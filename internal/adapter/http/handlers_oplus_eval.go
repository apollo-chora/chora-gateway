// handlers_oplus_eval.go — O+ Agent-Eval evidence drill-down BFF (IMDA D2).
//
//	GET /bff/oplus/eval-runs                    — crew-run index
//	GET /bff/oplus/eval-runs/{candidateLabel}   — per-row evidence drill-down
//
// Pass-through to chora-observability /api/v1/observability/eval-runs[...] (the
// BigQuery agent_eval_evidence view), reshaped to FE-canonical views wrapped in
// the OPlusEnvelope. Each crew run gets a `bigquery_url` deep-link and each
// member a `vertex_experiment_url` deep-link — the same selective-deep-link
// pattern as the AgentView Cloud-Trace / Agent-Engine links (anchoring-decision
// #2), built BFF-side from the canonical project (mirrors enrichCloudTraceURLs).
//
// Auth: `/bff/oplus/*` is auditor/admin-role-gated by the AuditorGate
// middleware. The crew-view groups by candidate_label; member identity is the
// experiment column (2026-06-07 crew-view decision).
package httpadapter

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

// EvalMetricSummaryView — per-metric autorater aggregate (FE-canonical).
type EvalMetricSummaryView struct {
	Metric   string  `json:"metric"`
	Count    int     `json:"count"`
	AvgScore float64 `json:"avg_score"`
}

// EvalAdversarialSummaryView — red-team aggregate (FE-canonical).
type EvalAdversarialSummaryView struct {
	Total   int `json:"total"`
	Blocked int `json:"blocked"`
	Leaked  int `json:"leaked"`
}

// EvalMemberView — one crew member with a Vertex Experiments deep-link.
type EvalMemberView struct {
	Experiment          string                      `json:"experiment"`
	MemberLabel         string                      `json:"member_label"`
	VertexExperimentURL *string                     `json:"vertex_experiment_url,omitempty"`
	AutoraterMetrics    []EvalMetricSummaryView     `json:"autorater_metrics"`
	Adversarial         *EvalAdversarialSummaryView `json:"adversarial"`
}

// EvalCrewRunView — one crew run with a BigQuery view deep-link.
type EvalCrewRunView struct {
	CandidateLabel string           `json:"candidate_label"`
	LastRecordedAt string           `json:"last_recorded_at"`
	BigQueryURL    *string          `json:"bigquery_url,omitempty"`
	Members        []EvalMemberView `json:"members"`
}

// EvalRunsResponse is the /bff/oplus/eval-runs body shape.
type EvalRunsResponse struct {
	OPlusEnvelope
	Runs []EvalCrewRunView `json:"runs"`
}

// EvalEvidenceRowView — one per-row evidence record (FE-canonical).
type EvalEvidenceRowView struct {
	Experiment         string  `json:"experiment"`
	Kind               string  `json:"kind"`
	CaseID             string  `json:"case_id"`
	RowIndex           int     `json:"row_index"`
	Metric             string  `json:"metric"`
	Score              float64 `json:"score"`
	AdversarialVerdict string  `json:"adversarial_verdict,omitempty"`
	Explanation        string  `json:"explanation"`
	Prompt             string  `json:"prompt"`
	Response           string  `json:"response"`
	Reference          string  `json:"reference,omitempty"`
	RecordedAt         string  `json:"recorded_at"`
}

// EvalEvidenceResponse is the /bff/oplus/eval-runs/{label} body shape.
type EvalEvidenceResponse struct {
	OPlusEnvelope
	CandidateLabel string                `json:"candidate_label"`
	Rows           []EvalEvidenceRowView `json:"rows"`
}

// EvalRuns returns the crew-run index, reshaped + deep-linked.
func (h *OPlusHandler) EvalRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	ctx := withUpstreamAuthCtx(r)
	tenantID, _ := upstreamCtx(ctx)
	q := r.URL.Query()
	res, err := h.Observability.GetEvalRuns(ctx, tenantID,
		strings.TrimSpace(q.Get("experiment")), strings.TrimSpace(q.Get("kind")))
	if err != nil {
		writeOPlusError(w, "GetEvalRuns", err, h.now())
		return
	}
	project := evalConsoleProject()
	location := evalVertexLocation()
	runs := make([]EvalCrewRunView, 0, len(res.Runs))
	for _, run := range res.Runs {
		runs = append(runs, mapEvalCrewRun(run, project, location))
	}
	writeJSON(w, http.StatusOK, EvalRunsResponse{
		OPlusEnvelope: OPlusEnvelope{State: "live", FetchedAt: h.now()},
		Runs:          runs,
	})
}

// EvalRunEvidence returns the per-row drill-down for one crew run.
func (h *OPlusHandler) EvalRunEvidence(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GATEWAY_METHOD_NOT_ALLOWED", "GET only")
		return
	}
	ctx := withUpstreamAuthCtx(r)
	tenantID, _ := upstreamCtx(ctx)
	label := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/bff/oplus/eval-runs/"), "/")
	if decoded, derr := url.PathUnescape(label); derr == nil {
		label = decoded
	}
	if label == "" || strings.Contains(label, "/") {
		writeError(w, http.StatusNotFound, "GATEWAY_NOT_FOUND", "candidate label required")
		return
	}
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil {
			limit = n
		}
	}
	res, err := h.Observability.GetEvalRunEvidence(ctx, tenantID, label,
		strings.TrimSpace(q.Get("experiment")), strings.TrimSpace(q.Get("kind")), limit)
	if err != nil {
		writeOPlusError(w, "GetEvalRunEvidence", err, h.now())
		return
	}
	rows := make([]EvalEvidenceRowView, 0, len(res.Rows))
	for _, row := range res.Rows {
		rows = append(rows, mapEvalEvidenceRow(row))
	}
	writeJSON(w, http.StatusOK, EvalEvidenceResponse{
		OPlusEnvelope:  OPlusEnvelope{State: "live", FetchedAt: h.now()},
		CandidateLabel: res.CandidateLabel,
		Rows:           rows,
	})
}

// -----------------------------------------------------------------------------
// Reshapers + deep-link builders
// -----------------------------------------------------------------------------

func mapEvalCrewRun(run upstream.EvalRun, project, location string) EvalCrewRunView {
	members := make([]EvalMemberView, 0, len(run.Members))
	for _, m := range run.Members {
		mv := EvalMemberView{
			Experiment:       m.Experiment,
			MemberLabel:      evalMemberLabel(m.Experiment),
			AutoraterMetrics: mapEvalMetrics(m.AutoraterMetrics),
		}
		if vu := vertexExperimentURL(project, location, m.Experiment); vu != "" {
			mv.VertexExperimentURL = &vu
		}
		if m.Adversarial != nil {
			mv.Adversarial = &EvalAdversarialSummaryView{
				Total:   m.Adversarial.Total,
				Blocked: m.Adversarial.Blocked,
				Leaked:  m.Adversarial.Leaked,
			}
		}
		members = append(members, mv)
	}
	v := EvalCrewRunView{
		CandidateLabel: run.CandidateLabel,
		LastRecordedAt: run.LastRecordedAt,
		Members:        members,
	}
	if bq := bigQueryEvidenceURL(project); bq != "" {
		v.BigQueryURL = &bq
	}
	return v
}

func mapEvalMetrics(in []upstream.EvalMetricSummary) []EvalMetricSummaryView {
	out := make([]EvalMetricSummaryView, 0, len(in))
	for _, m := range in {
		out = append(out, EvalMetricSummaryView{Metric: m.Metric, Count: m.Count, AvgScore: m.AvgScore})
	}
	return out
}

func mapEvalEvidenceRow(r upstream.EvalEvidenceRow) EvalEvidenceRowView {
	return EvalEvidenceRowView{
		Experiment:         r.Experiment,
		Kind:               r.Kind,
		CaseID:             r.CaseID,
		RowIndex:           r.RowIndex,
		Metric:             r.Metric,
		Score:              r.Score,
		AdversarialVerdict: r.AdversarialVerdict,
		Explanation:        r.Explanation,
		Prompt:             r.Prompt,
		Response:           r.Response,
		Reference:          r.Reference,
		RecordedAt:         r.RecordedAt,
	}
}

// evalMemberLabel shortens a member experiment id for display:
// chora-agent-eval-qgen-question -> qgen-question.
func evalMemberLabel(experiment string) string {
	return strings.TrimPrefix(experiment, "chora-agent-eval-")
}

// evalConsoleProject resolves the logical project label for the evidence
// deep-links. console deep-links were removed with the cloud decoupling.
func evalConsoleProject() string {
	if v := strings.TrimSpace(os.Getenv("CHORA_SOURCE_PROJECT")); v != "" {
		return v
	}
	return "chora-local"
}

// evalVertexLocation resolves the experiment region for the per-member
// deep-link.
func evalVertexLocation() string {
	if v := strings.TrimSpace(os.Getenv("CHORA_VERTEX_EXPERIMENT_LOCATION")); v != "" {
		return v
	}
	return "us-central1"
}

// defaultEvalConsoleURL is the self-hosted observability console base for the
// eval-evidence deep-links. console deep-links (console.the cloud console)
// were removed with the cloud decoupling; override with CHORA_EVAL_CONSOLE_URL.
const defaultEvalConsoleURL = "https://observability.chora.local"

// evalConsoleBaseURL returns the self-hosted console base URL.
func evalConsoleBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("CHORA_EVAL_CONSOLE_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultEvalConsoleURL
}

// bigQueryEvidenceURL deep-links to the agent_eval_rows evidence store in the
// self-hosted observability console. The per-run filtering happens in the O+
// in-app drill-down (/bff/oplus/eval-runs/{candidateLabel}); this link is the
// "open the raw store" convenience.
func bigQueryEvidenceURL(project string) string {
	if project == "" {
		return ""
	}
	return fmt.Sprintf("%s/bigquery/agent_eval_rows?project=%s",
		evalConsoleBaseURL(), url.QueryEscape(project))
}

// vertexExperimentURL deep-links to a crew member's experiment runs (the
// per-member summary trend) in the self-hosted console.
func vertexExperimentURL(project, location, experiment string) string {
	if project == "" || location == "" || experiment == "" {
		return ""
	}
	return fmt.Sprintf("%s/vertex-ai/experiments/%s/%s/runs?project=%s",
		evalConsoleBaseURL(), url.PathEscape(location), url.PathEscape(experiment), url.QueryEscape(project))
}
