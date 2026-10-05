// handlers_oplus_transformers_test.go — internal-package unit tests for the
// FE-canonical view transformers introduced by the BFF↔FE schema audit
// reconciliation. Companion to handlers_oplus_test.go (black-box). Covers
// audit findings #1..#5 transformation logic.
package httpadapter

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

// -----------------------------------------------------------------------------
// Audit finding #3 — Dashboard posture transformer
// -----------------------------------------------------------------------------

func TestDeriveDimensionStatus_Thresholds(t *testing.T) {
	cases := []struct {
		score int32
		want  string
	}{
		{100, "achieved"},
		{85, "achieved"},
		{84, "partial"},
		{72, "partial"},
		{60, "partial"},
		{59, "attention"},
		{30, "attention"},
		{29, "pending"},
		{0, "pending"},
	}
	for _, c := range cases {
		got := deriveDimensionStatus(c.score)
		if got != c.want {
			t.Errorf("deriveDimensionStatus(%d) = %s; want %s", c.score, got, c.want)
		}
	}
}

func TestBuildPostureView_CanonicalOrder(t *testing.T) {
	// Producer emits dimensions in random order — transformer must
	// canonicalise to D1..D4.
	input := []upstream.IMDADimensionSummary{
		{ID: "transparency", ScorePct: 58},
		{ID: "fairness_and_human_oversight", ScorePct: 64},
		{ID: "accountability", ScorePct: 72},
		{ID: "safety_and_robustness", ScorePct: 81},
	}
	posture := buildPostureView(input)
	if len(posture) != 4 {
		t.Fatalf("len(posture) = %d; want 4", len(posture))
	}
	wantOrder := []string{"accountability", "transparency", "safety_and_robustness", "fairness_and_human_oversight"}
	for i, p := range posture {
		if p.Num != i+1 {
			t.Errorf("posture[%d].Num = %d; want %d", i, p.Num, i+1)
		}
		if p.Label != wantOrder[i] {
			t.Errorf("posture[%d].Label = %s; want %s", i, p.Label, wantOrder[i])
		}
	}
	// First entry: accountability, score 72 → partial (60 ≤ 72 < 85).
	if posture[0].Score != 72 {
		t.Errorf("posture[0].Score = %d; want 72", posture[0].Score)
	}
	if posture[0].Status != "partial" {
		t.Errorf("posture[0].Status = %s; want partial", posture[0].Status)
	}
}

func TestBuildPostureView_MissingDimensionFillsPending(t *testing.T) {
	// Only 2 of 4 dimensions present — missing rows must become pending.
	input := []upstream.IMDADimensionSummary{
		{ID: "accountability", ScorePct: 90},
	}
	posture := buildPostureView(input)
	if len(posture) != 4 {
		t.Fatalf("len(posture) = %d; want 4", len(posture))
	}
	if posture[0].Status != "achieved" {
		t.Errorf("posture[0].Status = %s; want achieved", posture[0].Status)
	}
	// Missing rows: status MUST be pending regardless of score (score=0 →
	// would map to pending anyway, but transformer also overrides explicitly).
	for i := 1; i < 4; i++ {
		if posture[i].Status != "pending" {
			t.Errorf("posture[%d].Status = %s; want pending", i, posture[i].Status)
		}
	}
}

func TestAllBaselineAchieved(t *testing.T) {
	allOK := []PostureItemView{
		{Num: 1, Label: "accountability", Score: 90, Status: "achieved"},
		{Num: 2, Label: "transparency", Score: 88, Status: "achieved"},
		{Num: 3, Label: "safety_and_robustness", Score: 95, Status: "achieved"},
		{Num: 4, Label: "fairness_and_human_oversight", Score: 87, Status: "achieved"},
	}
	if !allBaselineAchieved(allOK) {
		t.Error("allBaselineAchieved(all achieved) = false; want true")
	}

	mixed := append([]PostureItemView{}, allOK...)
	mixed[1].Status = "partial"
	if allBaselineAchieved(mixed) {
		t.Error("allBaselineAchieved(one partial) = true; want false")
	}
	if allBaselineAchieved(nil) {
		t.Error("allBaselineAchieved(nil) = true; want false")
	}
}

// -----------------------------------------------------------------------------
// Audit finding #5 — Rubric item transformer
// -----------------------------------------------------------------------------

func TestSplitToolsFromEvidenceSource(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"Cloud Trace + AgentDecisionLog", []string{"cloud_trace", "agentdecisionlog"}},
		{"deepteam, promptfoo", []string{"deepteam", "promptfoo"}},
		{"axe-core via Playwright", []string{"axe-core_via_playwright"}},
		{"", []string{}},
		{"  ", []string{}},
	}
	for _, c := range cases {
		got := splitToolsFromEvidenceSource(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitToolsFromEvidenceSource(%q) = %v; want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitToolsFromEvidenceSource(%q)[%d] = %s; want %s", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestMapRubricItem(t *testing.T) {
	url := "https://trace.chora.local/trace/abc"
	ri := upstream.RubricItem{
		ID:                "1.1",
		Title:             "Audit trail for all AI decisions",
		EvidenceSource:    "Cloud Trace + AgentDecisionLog",
		Status:            "PARTIAL",
		EvidenceSourceURL: url,
	}
	view := mapRubricItem(ri)
	if view.Ref != "1.1" {
		t.Errorf("Ref = %s; want 1.1", view.Ref)
	}
	if view.Requirement != "Audit trail for all AI decisions" {
		t.Errorf("Requirement = %s", view.Requirement)
	}
	if view.Status != "partial" {
		t.Errorf("Status = %s; want lowercase 'partial'", view.Status)
	}
	if view.EvidenceSourceURL == nil || *view.EvidenceSourceURL != url {
		t.Errorf("EvidenceSourceURL = %v; want pointer to %s", view.EvidenceSourceURL, url)
	}
	if len(view.Tools) != 2 {
		t.Errorf("Tools = %v; want 2 items", view.Tools)
	}
}

func TestMapRubricItem_NoURL_NilPointer(t *testing.T) {
	ri := upstream.RubricItem{ID: "2.1", Title: "X", Status: "PASS"}
	view := mapRubricItem(ri)
	if view.EvidenceSourceURL != nil {
		t.Errorf("EvidenceSourceURL = %v; want nil (no URL provided)", view.EvidenceSourceURL)
	}
}

// -----------------------------------------------------------------------------
// Audit finding #1 — Decisions transformer
// -----------------------------------------------------------------------------

func TestSlugifyAgent(t *testing.T) {
	cases := map[string]string{
		"qgen_question":     "qgen-question",
		"QGen_Critic":       "qgen-critic",
		"  qgen_reporter  ": "qgen-reporter",
		"":                  "",
	}
	for in, want := range cases {
		got := slugifyAgent(in)
		if got != want {
			t.Errorf("slugifyAgent(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestFormatUSD(t *testing.T) {
	v := 0.0021
	if got := formatUSD(&v); got != "$0.0021" {
		t.Errorf("formatUSD(0.0021) = %s; want $0.0021", got)
	}
	if got := formatUSD(nil); got != "" {
		t.Errorf("formatUSD(nil) = %q; want empty string", got)
	}
}

func TestMapAgentDecision_AllFields(t *testing.T) {
	conf := 0.94
	cost := 0.0021
	d := upstream.AgentDecisionRow{
		LogID:         "log-001",
		AgentID:       "qgen_question",
		AGID:          "agid_x",
		Decision:      "permitted",
		TraceID:       "tid-1",
		CreatedAt:     time.Date(2026, 5, 26, 9, 14, 22, 0, time.UTC),
		CorrelationID: "wf-001",
		DecisionKind:  "qna_generate",
		ModelID:       "gemini-2.5-pro",
		Confidence:    &conf,
		CostUSD:       &cost,
		AutonomyLevel: "HOOTL",
		CloudTraceURL: "https://trace.chora.local/trace/tid-1?span=sid-1",
		Reasoning: &upstream.AgentDecisionReasoning{
			Summary:    "Stem clear; 4 plausible options with one unambiguous key.",
			InputHash:  "sha256:aaa",
			OutputHash: "sha256:bbb",
		},
	}
	view := mapAgentDecision(d)
	if view.ReasoningSummary != "Stem clear; 4 plausible options with one unambiguous key." {
		t.Errorf("ReasoningSummary = %q; want the critic rationale", view.ReasoningSummary)
	}
	if view.InputHash != "sha256:aaa" || view.OutputHash != "sha256:bbb" {
		t.Errorf("hashes = %q/%q; want the PII-safe citation", view.InputHash, view.OutputHash)
	}
	if view.ID != "log-001" {
		t.Errorf("ID = %s; want log-001", view.ID)
	}
	if view.WorkflowID != "wf-001" {
		t.Errorf("WorkflowID = %s; want wf-001 (from CorrelationID)", view.WorkflowID)
	}
	if view.AgentSlug != "agid-x" {
		t.Errorf("AgentSlug = %s; want agid-x", view.AgentSlug)
	}
	if view.DecisionType != "qna_generate" {
		t.Errorf("DecisionType = %s; want qna_generate", view.DecisionType)
	}
	if view.Model != "gemini-2.5-pro" {
		t.Errorf("Model = %s", view.Model)
	}
	if view.Cost != "$0.0021" {
		t.Errorf("Cost = %s; want $0.0021", view.Cost)
	}
	if view.TraceID == nil || *view.TraceID != "tid-1" {
		t.Errorf("TraceID = %v; want pointer to tid-1", view.TraceID)
	}
}

func TestMapAgentDecision_FallbackPath(t *testing.T) {
	// CorrelationID empty → WorkflowID falls back to LogID.
	// DecisionKind empty → DecisionType falls back to Decision string.
	// CostUSD nil → empty string.
	d := upstream.AgentDecisionRow{
		LogID:    "log-002",
		AgentID:  "qgen_critic",
		Decision: "permitted",
	}
	view := mapAgentDecision(d)
	if view.WorkflowID != "log-002" {
		t.Errorf("WorkflowID = %s; want log-002 (fallback)", view.WorkflowID)
	}
	if view.DecisionType != "permitted" {
		t.Errorf("DecisionType = %s; want 'permitted' (fallback from Decision)", view.DecisionType)
	}
	if view.Cost != "" {
		t.Errorf("Cost = %q; want empty", view.Cost)
	}
	if view.TraceID != nil {
		t.Errorf("TraceID = %v; want nil (no trace)", view.TraceID)
	}
}

// TestMapAgentDecision_PrefersVerdict proves the qgen quality-gate verdict
// (accepted | rejected | completed_with_warning | refused) is the human-
// meaningful outcome that takes precedence over the richer decision_kind AND
// the base decision_type ENUM — so the O+ DECISION TYPE column + reasoning-panel
// step render "accepted" rather than "qna_generate"/"respond" (CHO-1700
// follow-up). The verdict is sourced from obs attributes["decision"].
func TestMapAgentDecision_PrefersVerdict(t *testing.T) {
	d := upstream.AgentDecisionRow{
		LogID:        "log-003",
		AgentID:      "qgen_critic",
		Verdict:      "accepted",
		DecisionKind: "critique",
		DecisionType: "respond",
		Decision:     "permitted",
	}
	view := mapAgentDecision(d)
	if view.DecisionType != "accepted" {
		t.Errorf("DecisionType = %s; want accepted (verdict preferred over kind/type)", view.DecisionType)
	}
}

// TestMapAgentDecision_VerdictFallback proves that when the verdict is absent
// (older rows, non-qgen agents) the column still falls back to decision_kind →
// decision_type → decision, so no regression for decisions without a verdict.
func TestMapAgentDecision_VerdictFallback(t *testing.T) {
	d := upstream.AgentDecisionRow{
		LogID:        "log-004",
		AgentID:      "qgen_question",
		DecisionType: "respond",
	}
	view := mapAgentDecision(d)
	if view.DecisionType != "respond" {
		t.Errorf("DecisionType = %s; want respond (fallback when no verdict)", view.DecisionType)
	}
}

// TestMapAgentDecision_PromptConditionsPassThrough proves the durable
// prompt-explainability map<string,string> (ADR-197 M-A.5) round-trips from the
// upstream chora-observability decision row straight through the BFF transformer
// onto the O+ governance decision view — no transformation of the map — and that
// it serializes to the pinned `prompt_conditions` JSON key.
func TestMapAgentDecision_PromptConditionsPassThrough(t *testing.T) {
	pc := map[string]string{
		"difficulty":      "intermediate",
		"cognitive_level": "apply",
		"subject":         "algebra",
	}
	d := upstream.AgentDecisionRow{
		LogID:            "log-005",
		AgentID:          "qgen_question",
		PromptConditions: pc,
	}
	view := mapAgentDecision(d)
	if !reflect.DeepEqual(view.PromptConditions, pc) {
		t.Errorf("PromptConditions = %v; want pass-through %v", view.PromptConditions, pc)
	}

	// JSON round-trip on the pinned contract key.
	b, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		PromptConditions map[string]string `json:"prompt_conditions"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got.PromptConditions, pc) {
		t.Errorf("prompt_conditions JSON = %v; want %v", got.PromptConditions, pc)
	}
}

// TestMapAgentDecision_PromptConditionsOmittedWhenEmpty proves the omitempty
// contract: when the upstream row carries no prompt_conditions, the key is
// absent from the serialized governance decision view (not `null`/`{}`).
func TestMapAgentDecision_PromptConditionsOmittedWhenEmpty(t *testing.T) {
	view := mapAgentDecision(upstream.AgentDecisionRow{LogID: "log-006", AgentID: "qgen_critic"})
	if view.PromptConditions != nil {
		t.Errorf("PromptConditions = %v; want nil when upstream omitted it", view.PromptConditions)
	}
	b, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "prompt_conditions") {
		t.Errorf("serialized view should omit prompt_conditions key; got %s", string(b))
	}
}

// -----------------------------------------------------------------------------
// Audit finding #2 — HITL transformer
// -----------------------------------------------------------------------------

func TestNormaliseGate(t *testing.T) {
	cases := map[string]string{
		"question_review":    "question_review",
		"report_review":      "report_review",
		"material_decision":  "material_decision",
		"Question_Review":    "question_review",
		"reviewer_question":  "question_review",
		"final_report_thing": "report_review",
		"":                   "material_decision",
		"weird_thing":        "material_decision",
	}
	for in, want := range cases {
		got := normaliseGate(in)
		if got != want {
			t.Errorf("normaliseGate(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestMapHITLItem(t *testing.T) {
	alice := "assessor-alice"
	h := upstream.HITLPendingItem{
		DecisionID:    "h-001",
		AgentID:       "Classification Agent",
		CreatedAt:     time.Date(2026, 5, 26, 9, 18, 44, 0, time.UTC),
		Reason:        "Classification reports insufficient material",
		CorrelationID: "wf-002",
		DecisionKind:  "material_decision",
		Summary:       "Classification reports insufficient material. Choose: upload more or trigger web research.",
		AutonomyLevel: "HITL-L1",
		AssigneeGCID:  &alice,
	}
	view := mapHITLItem(h)
	if view.ID != "h-001" {
		t.Errorf("ID = %s", view.ID)
	}
	if view.WorkflowID != "wf-002" {
		t.Errorf("WorkflowID = %s", view.WorkflowID)
	}
	if view.Gate != "material_decision" {
		t.Errorf("Gate = %s", view.Gate)
	}
	if view.Summary == "" || !strings.Contains(view.Summary, "Classification") {
		t.Errorf("Summary = %q", view.Summary)
	}
	if view.AutonomyLevel != "HITL-L1" {
		t.Errorf("AutonomyLevel = %s", view.AutonomyLevel)
	}
	if view.Assignee == nil || *view.Assignee != "assessor-alice" {
		t.Errorf("Assignee = %v", view.Assignee)
	}
	if view.WaitingSince == "" {
		t.Error("WaitingSince empty")
	}
}

func TestMapHITLItem_SummaryFallsBackToReason(t *testing.T) {
	h := upstream.HITLPendingItem{
		DecisionID: "h-002",
		Reason:     "policy match",
		// AssigneeGCID intentionally nil — exercises the unassigned path.
	}
	view := mapHITLItem(h)
	if view.Summary != "policy match" {
		t.Errorf("Summary = %q; want 'policy match' (fallback from Reason)", view.Summary)
	}
	if view.Assignee != nil {
		t.Errorf("Assignee = %v; want nil (unassigned semantic)", view.Assignee)
	}
}

// -----------------------------------------------------------------------------
// Audit finding #4 — A2A transformers
// -----------------------------------------------------------------------------

func TestBuildPartnerLookup(t *testing.T) {
	ids := []upstream.A2AIdentity{
		{AGID: "agid-1", PartnerName: "Partner X"},
		{AGID: "agid-2", DisplayName: "Display Y"},
		{AGID: ""},
	}
	lookup := buildPartnerLookup(ids)
	if lookup["agid-1"] != "Partner X" {
		t.Errorf("lookup[agid-1] = %s; want 'Partner X'", lookup["agid-1"])
	}
	if lookup["agid-2"] != "Display Y" {
		t.Errorf("lookup[agid-2] = %s; want 'Display Y' (DisplayName fallback)", lookup["agid-2"])
	}
	if _, has := lookup[""]; has {
		t.Error("lookup contains empty-string key — should be skipped")
	}
}

func TestMapA2AContract(t *testing.T) {
	lookup := map[string]string{"agid-1": "Partner X"}
	c := upstream.A2AContract{
		ContractID:       "c-001",
		PartnerID:        "partner-id-x",
		Status:           "active",
		CreatedAt:        time.Date(2026, 3, 14, 0, 0, 0, 0, time.UTC),
		AGID:             "agid-1",
		Scope:            []string{"read:atoms"},
		LastInvocationAt: "2026-05-26T09:42:11Z",
		Invocations30d:   1842,
	}
	view := mapA2AContract(c, lookup)
	if view.ID != "c-001" {
		t.Errorf("ID = %s; want c-001 (renamed from contract_id)", view.ID)
	}
	if view.Partner != "Partner X" {
		t.Errorf("Partner = %s; want 'Partner X' (lookup hit)", view.Partner)
	}
	if view.AGID != "agid-1" {
		t.Errorf("AGID = %s", view.AGID)
	}
	if len(view.Scope) != 1 || view.Scope[0] != "read:atoms" {
		t.Errorf("Scope = %v", view.Scope)
	}
	if view.LastInvocation == nil || *view.LastInvocation != "2026-05-26T09:42:11Z" {
		t.Errorf("LastInvocation = %v", view.LastInvocation)
	}
	if view.Invocations30d != 1842 {
		t.Errorf("Invocations30d = %d", view.Invocations30d)
	}
}

func TestMapA2AContract_PartnerFallback(t *testing.T) {
	// No PartnerName, no lookup hit → falls back to PartnerID.
	c := upstream.A2AContract{ContractID: "c-002", PartnerID: "raw-partner", AGID: "missing-agid"}
	view := mapA2AContract(c, map[string]string{})
	if view.Partner != "raw-partner" {
		t.Errorf("Partner = %s; want 'raw-partner' (fallback to PartnerID)", view.Partner)
	}
}

func TestMapA2AIdentity_PassThroughTrustLevelAndKeyFingerprint(t *testing.T) {
	// Per [[feedback-no-stubs-real-wiring]] the BFF transformer is now a
	// pure pass-through for both fields. chora-a2a-gateway derives
	// trust_level + key_fingerprint from real backend state; the BFF
	// never fabricates defaults (the previous "pilot" default + the
	// sha256(agid) placeholder were removed in this change).
	id := upstream.A2AIdentity{
		AGID:           "agid-xyz",
		DisplayName:    "X",
		TrustLevel:     "verified",
		KeyFingerprint: "deadbeefdeadbeef0011223344556677",
	}
	view := mapA2AIdentity(id)
	if view.TrustLevel != "verified" {
		t.Errorf("TrustLevel = %s; want pass-through 'verified'", view.TrustLevel)
	}
	if view.KeyFingerprint != "deadbeefdeadbeef0011223344556677" {
		t.Errorf("KeyFingerprint = %q; want pass-through producer value", view.KeyFingerprint)
	}
}

func TestMapA2AIdentity_HonestEmptyWhenProducerOmitsFields(t *testing.T) {
	// When the producer (chora-a2a-gateway) has no key on file or no
	// trust signal computed yet, both fields are emitted as empty
	// strings — per [[feedback-no-stubs-real-wiring]] empty is honest;
	// sha256(agid) would be fabricated and is forbidden.
	id := upstream.A2AIdentity{AGID: "agid-no-key", DisplayName: "Y"}
	view := mapA2AIdentity(id)
	if view.TrustLevel != "" {
		t.Errorf("TrustLevel = %q; want empty (no upstream signal → honest empty, not fabricated 'pilot')", view.TrustLevel)
	}
	if view.KeyFingerprint != "" {
		t.Errorf("KeyFingerprint = %q; want empty (no upstream key → honest empty, not sha256(agid) placeholder)", view.KeyFingerprint)
	}
	// Defensive guard against the placeholder we just removed.
	if strings.HasPrefix(view.KeyFingerprint, "sha256:") {
		t.Errorf("KeyFingerprint = %q; placeholder regression — sha256(agid) is fake and forbidden", view.KeyFingerprint)
	}
}

func TestMapA2AInvocation_ComputedLatency(t *testing.T) {
	start := time.Date(2026, 5, 26, 9, 42, 11, 0, time.UTC)
	end := start.Add(142 * time.Millisecond)
	inv := upstream.A2AInvocation{
		InvocationID: "inv-001",
		AGID:         "agid-1",
		Outcome:      "permitted",
		StartedAt:    start,
		EndedAt:      end,
	}
	view := mapA2AInvocation(inv, map[string]string{"agid-1": "Partner X"})
	if view.ID != "inv-001" {
		t.Errorf("ID = %s; want inv-001 (rename of invocation_id)", view.ID)
	}
	if view.Status != "success" {
		t.Errorf("Status = %s; want success (rename of outcome=permitted)", view.Status)
	}
	if view.LatencyMS != 142 {
		t.Errorf("LatencyMS = %d; want 142 (computed end-start)", view.LatencyMS)
	}
	if view.Partner != "Partner X" {
		t.Errorf("Partner = %s", view.Partner)
	}
	// Endpoint missing on producer → transformer emits empty string per the
	// honest-null contract (no "unknown" placeholder; FE renders em dash).
	if view.Endpoint != "" {
		t.Errorf("Endpoint = %q; want empty (no placeholder substitution)", view.Endpoint)
	}
}

// Producer-supplied endpoint MUST round-trip pass-through onto the FE view —
// the M12 TODO defaulting to "unknown" is cleared as of 2026-05-26.
func TestMapA2AInvocation_PassesEndpointThrough(t *testing.T) {
	inv := upstream.A2AInvocation{
		InvocationID: "inv-002",
		AGID:         "agid-2",
		Outcome:      "permitted",
		Endpoint:     "https://a2a.chora.site/a2a/invoke#recommend_content",
	}
	view := mapA2AInvocation(inv, nil)
	if view.Endpoint != "https://a2a.chora.site/a2a/invoke#recommend_content" {
		t.Errorf("Endpoint = %q; want canonical URL preserved verbatim", view.Endpoint)
	}
}

func TestNormaliseInvocationStatus(t *testing.T) {
	cases := map[string]string{
		"permitted": "success",
		"success":   "success",
		"OK":        "success",
		"denied":    "denied",
		"DENIED":    "denied",
		"timeout":   "error",
		"":          "error",
	}
	for in, want := range cases {
		got := normaliseInvocationStatus(in)
		if got != want {
			t.Errorf("normaliseInvocationStatus(%q) = %s; want %s", in, got, want)
		}
	}
}

// -----------------------------------------------------------------------------
// Priority-driven traffic-light (2026-06-02) — the dimension status is driven
// by per-item P1/P2/P3 FAIL counts, not the raw score:
//   any P1 FAIL        → attention (RED)
//   >= 2 P2 FAILs      → partial   (AMBER)
//   otherwise (items)  → achieved  (GREEN)
//   no items           → pending
// -----------------------------------------------------------------------------

func ri(priority, status string) upstream.RubricItem {
	return upstream.RubricItem{ID: "x", Title: "t", Status: status, Priority: priority}
}

func TestDeriveDimensionStatusWithPriority(t *testing.T) {
	cases := []struct {
		name  string
		score int32
		items []upstream.RubricItem
		want  string
	}{
		{"no items -> score fallback (pending at low score)", 0, nil, "pending"},
		{"no items -> score fallback (achieved at high score)", 95, nil, "achieved"},
		{"single P1 fail -> attention even at high score", 99,
			[]upstream.RubricItem{ri("P1", "PASS"), ri("P1", "FAIL")}, "attention"},
		{"two P2 fails -> partial", 90,
			[]upstream.RubricItem{ri("P2", "FAIL"), ri("P2", "FAIL"), ri("P1", "PASS")}, "partial"},
		{"single P2 fail -> achieved (needs 2)", 10,
			[]upstream.RubricItem{ri("P2", "FAIL"), ri("P1", "PASS")}, "achieved"},
		{"P3 fails never downgrade", 10,
			[]upstream.RubricItem{ri("P3", "FAIL"), ri("P3", "FAIL"), ri("P3", "FAIL")}, "achieved"},
		{"all pass -> achieved", 100,
			[]upstream.RubricItem{ri("P1", "PASS"), ri("P2", "PASS")}, "achieved"},
		{"P1 dominates over many P2 fails", 50,
			[]upstream.RubricItem{ri("P1", "FAIL"), ri("P2", "FAIL"), ri("P2", "FAIL")}, "attention"},
		{"P1 PARTIAL is not a fail", 50,
			[]upstream.RubricItem{ri("P1", "PARTIAL"), ri("P2", "FAIL"), ri("P2", "FAIL")}, "partial"},
		{"lowercase status tolerated", 50,
			[]upstream.RubricItem{ri("P1", "fail")}, "attention"},
	}
	for _, c := range cases {
		if got := deriveDimensionStatusWithPriority(c.score, c.items); got != c.want {
			t.Errorf("%s: got %q; want %q", c.name, got, c.want)
		}
	}
}

func TestMapRubricItem_PrefersRefOverIDSlug(t *testing.T) {
	// The id is a long slug that overflows the narrow ref column; the short
	// `ref` ("1.1") must win for display.
	view := mapRubricItem(upstream.RubricItem{ID: "audit_trail_coverage", Ref: "1.1", Title: "Audit", Status: "PASS"})
	if view.Ref != "1.1" {
		t.Errorf("Ref = %q; want 1.1 (ref preferred over id slug)", view.Ref)
	}
	// Falls back to id when ref is absent.
	if got := mapRubricItem(upstream.RubricItem{ID: "2.1", Title: "X", Status: "PASS"}).Ref; got != "2.1" {
		t.Errorf("Ref fallback = %q; want 2.1", got)
	}
}

func TestMapRubricItem_Priority(t *testing.T) {
	view := mapRubricItem(upstream.RubricItem{ID: "1.2", Title: "Risk", Status: "FAIL", Priority: "P1"})
	if view.Priority != "P1" {
		t.Errorf("priority = %q; want P1", view.Priority)
	}
	// Empty priority passes through empty (legacy items).
	if got := mapRubricItem(upstream.RubricItem{ID: "x", Status: "PASS"}).Priority; got != "" {
		t.Errorf("empty priority = %q; want empty", got)
	}
}
