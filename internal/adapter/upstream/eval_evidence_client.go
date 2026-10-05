// eval_evidence_client.go — chora-observability agent-eval evidence client for
// the O+ Agent-Eval drill-down (IMDA D2 transparency).
//
//	GET /api/v1/observability/eval-runs                   — crew-run index
//	GET /api/v1/observability/eval-runs/{candidateLabel}  — per-row drill-down
//
// These mirror the producer JSON shapes (observability-admin.yaml). The BFF
// (handlers_oplus_eval.go) reshapes them into FE-canonical views + adds the
// BigQuery / Vertex Experiments console deep-links. Same transport + auth +
// no-stubs invariants as the rest of the ObservabilityClient.
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// EvalMetricSummary is a per-metric autorater aggregate within one crew member.
type EvalMetricSummary struct {
	Metric   string  `json:"metric"`
	Count    int     `json:"count"`
	AvgScore float64 `json:"avg_score"`
}

// EvalAdversarialSummary is a red-team outcome aggregate within one crew member.
type EvalAdversarialSummary struct {
	Total   int `json:"total"`
	Blocked int `json:"blocked"`
	Leaked  int `json:"leaked"`
}

// EvalRunMember is one crew member (experiment) within a crew run.
type EvalRunMember struct {
	Experiment       string                  `json:"experiment"`
	AutoraterMetrics []EvalMetricSummary     `json:"autorater_metrics"`
	Adversarial      *EvalAdversarialSummary `json:"adversarial"`
}

// EvalRun is one crew run keyed by candidate_label.
type EvalRun struct {
	CandidateLabel string          `json:"candidate_label"`
	LastRecordedAt string          `json:"last_recorded_at"`
	Members        []EvalRunMember `json:"members"`
}

// EvalRunsResult is the producer crew-run index body (`{runs:[...]}`).
type EvalRunsResult struct {
	Runs []EvalRun `json:"runs"`
}

// EvalEvidenceRow is one scored per-case evidence row.
type EvalEvidenceRow struct {
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

// EvalRunEvidenceResult is the producer drill-down body.
type EvalRunEvidenceResult struct {
	CandidateLabel string            `json:"candidate_label"`
	Rows           []EvalEvidenceRow `json:"rows"`
}

// GetEvalRuns fetches the crew-run index. experiment/kind are optional filters.
func (o *ObservabilityClient) GetEvalRuns(ctx context.Context, tenantID, experiment, kind string) (EvalRunsResult, error) {
	if o == nil || o.cfg.HTTPAddr == "" {
		return EvalRunsResult{}, fmt.Errorf("%w: observability: HTTP addr not wired", ErrUpstream)
	}
	q := url.Values{}
	if tenantID != "" {
		q.Set("tenant_id", tenantID)
	}
	if experiment != "" {
		q.Set("experiment", experiment)
	}
	if kind != "" {
		q.Set("kind", kind)
	}
	u := strings.TrimRight(o.cfg.HTTPAddr, "/") + "/api/v1/observability/eval-runs"
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	body, err := o.httpGetJSON(ctx, "upstream.observability.GetEvalRuns", u, tenantID)
	if err != nil {
		return EvalRunsResult{}, err
	}
	if len(body) == 0 {
		return EvalRunsResult{Runs: []EvalRun{}}, nil
	}
	var out EvalRunsResult
	if err := json.Unmarshal(body, &out); err != nil {
		return EvalRunsResult{}, fmt.Errorf("%w: GetEvalRuns: malformed JSON: %v", ErrUpstream, err)
	}
	if out.Runs == nil {
		out.Runs = []EvalRun{}
	}
	return out, nil
}

// GetEvalRunEvidence fetches the per-row drill-down for one crew run.
// experiment/kind/limit are optional (limit <= 0 lets the producer default).
func (o *ObservabilityClient) GetEvalRunEvidence(ctx context.Context, tenantID, candidateLabel, experiment, kind string, limit int) (EvalRunEvidenceResult, error) {
	if o == nil || o.cfg.HTTPAddr == "" {
		return EvalRunEvidenceResult{}, fmt.Errorf("%w: observability: HTTP addr not wired", ErrUpstream)
	}
	q := url.Values{}
	if tenantID != "" {
		q.Set("tenant_id", tenantID)
	}
	if experiment != "" {
		q.Set("experiment", experiment)
	}
	if kind != "" {
		q.Set("kind", kind)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	u := strings.TrimRight(o.cfg.HTTPAddr, "/") + "/api/v1/observability/eval-runs/" + url.PathEscape(candidateLabel)
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	body, err := o.httpGetJSON(ctx, "upstream.observability.GetEvalRunEvidence", u, tenantID)
	if err != nil {
		return EvalRunEvidenceResult{}, err
	}
	if len(body) == 0 {
		return EvalRunEvidenceResult{CandidateLabel: candidateLabel, Rows: []EvalEvidenceRow{}}, nil
	}
	var out EvalRunEvidenceResult
	if err := json.Unmarshal(body, &out); err != nil {
		return EvalRunEvidenceResult{}, fmt.Errorf("%w: GetEvalRunEvidence: malformed JSON: %v", ErrUpstream, err)
	}
	if out.Rows == nil {
		out.Rows = []EvalEvidenceRow{}
	}
	if out.CandidateLabel == "" {
		out.CandidateLabel = candidateLabel
	}
	return out, nil
}
