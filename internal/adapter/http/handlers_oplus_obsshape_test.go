// handlers_oplus_obsshape_test.go — verifies mapAgentDecision reads the fields
// chora-observability ACTUALLY serializes today on agent_decision rows:
// `agid` (not `agent_id`) and `decision_type` (not the enriched `decision_kind`).
// Without these fallbacks the O+ Decision Traces Agent + Type columns render
// blank even though the qgen/critic telemetry carries the data. Regression
// guard for the 2026-06-02 obs↔gateway field-name reconciliation.
package httpadapter

import (
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

func TestMapAgentDecision_ObsCurrentShape(t *testing.T) {
	d := upstream.AgentDecisionRow{
		LogID:         "019e736c-9c8e-79ab-9597-4310fab28646",
		AGID:          "qgen_crew", // obs emits `agid`, NOT `agent_id`
		DecisionType:  "respond",   // obs emits `decision_type`, NOT `decision_kind`
		CorrelationID: "55863056-5617-49cd-8819-b8a9baca1f93",
		Traceparent:   "00-c7d2b4a939168862fa71880b5fb2e08b-f171040a776ba3d9-01",
		CreatedAt:     time.Date(2026, 6, 1, 11, 9, 16, 0, time.UTC),
	}
	view := mapAgentDecision(d)
	if view.Agent != "qgen_crew" {
		t.Errorf("Agent = %q; want qgen_crew (AGID fallback when agent_id absent)", view.Agent)
	}
	if view.AgentSlug != "qgen-crew" {
		t.Errorf("AgentSlug = %q; want qgen-crew", view.AgentSlug)
	}
	if view.DecisionType != "respond" {
		t.Errorf("DecisionType = %q; want respond (decision_type fallback)", view.DecisionType)
	}
	if view.WorkflowID != "55863056-5617-49cd-8819-b8a9baca1f93" {
		t.Errorf("WorkflowID = %q; want correlation_id", view.WorkflowID)
	}
}

// TestMapAgentDecision_DecisionKindWinsOverDecisionType ensures the enriched
// CHO-1560 `decision_kind` still takes precedence over the current-shape
// `decision_type` when both are present (future-proofs the fallback order).
func TestMapAgentDecision_DecisionKindWinsOverDecisionType(t *testing.T) {
	d := upstream.AgentDecisionRow{
		LogID:        "log-x",
		AGID:         "qgen_critic",
		DecisionKind: "critique",
		DecisionType: "respond",
	}
	if got := mapAgentDecision(d).DecisionType; got != "critique" {
		t.Errorf("DecisionType = %q; want critique (decision_kind precedence)", got)
	}
}

// TestMapAgentDecision_ExplicitAgentIDWins ensures an explicit agent_id (legacy
// enriched producers) is preferred over the AGID fallback.
func TestMapAgentDecision_ExplicitAgentIDWins(t *testing.T) {
	d := upstream.AgentDecisionRow{LogID: "log-y", AgentID: "explicit_agent", AGID: "agid_z"}
	if got := mapAgentDecision(d).Agent; got != "explicit_agent" {
		t.Errorf("Agent = %q; want explicit_agent (agent_id precedence)", got)
	}
}
