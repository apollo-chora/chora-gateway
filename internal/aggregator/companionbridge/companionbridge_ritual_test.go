// companionbridge_ritual_test.go — the CHO-2016 Grimoire Rituals proxy lane,
// pinned at the status contract the FE branches on.
//
// CHO-2145 made the run POST ASYNC: chora-consumption now acks 202 + the
// running record and executes the ~30s LLM phase detached, because this bridge
// 504s the route at PerCallTimeout (5s) and a synchronous run therefore
// surfaced a false "Something went wrong" on a healthy run.
//
// The bridge needed NO change for that — `classify` forwards every sub-500
// status verbatim — but the FE's running→poll branch now DEPENDS on the 202
// arriving intact, so these tests lock it: a 202 rewritten to 200 (or swallowed
// into a 502) would silently strand the learner on a spinner, or re-open the
// false-error bug. The run route keeps the 5s budget on purpose: the ack is a
// few bounded DB/gRPC hops, and the slow phase no longer rides the request.
package companionbridge_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/companionbridge"
)

const runAckBody = `{"runId":"run-1","ritualId":"rit-1","revisionNo":1,"status":"running","manaCharged":35,"startedAt":"2026-07-11T00:00:00Z","story":{"title":"Morning Review","status":"running","steps":[],"manaCost":35}}`

// The 202 ack must reach the FE with its status AND body intact — that is the
// non-error acknowledgement the whole story exists to deliver.
func TestRunRitual_Passes202AckThroughVerbatim(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(runAckBody))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})

	resp, err := b.RunRitual(context.Background(), basicAuth(), "fam-1", "rit-1", []byte(`{"triggerSource":"manual"}`))
	if err != nil {
		t.Fatalf("RunRitual: %v", err)
	}
	if resp.Status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (the async ack — the FE polls on it)", resp.Status)
	}
	body := decodeJSON(t, resp.Body)
	if body["status"] != "running" {
		t.Errorf("status field = %v, want running", body["status"])
	}
	if body["runId"] != "run-1" {
		t.Errorf("runId = %v, want run-1 (the poll key)", body["runId"])
	}
	if cons.lastPath != "/v1/me/companions/fam-1/rituals/rit-1/run" {
		t.Errorf("path = %q", cons.lastPath)
	}
	if cons.lastMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", cons.lastMethod)
	}
	// Rituals are camelCase END-TO-END — the body must NOT be snake-cased.
	if got := string(cons.lastBody); got != `{"triggerSource":"manual"}` {
		t.Errorf("body = %s, want the camelCase body passed through verbatim", got)
	}
}

// skipped_budget is TERMINAL AT ACK (the mana reserve refused) — consumption
// answers 200 with the final record, not 202. It must stay a 200: a designed
// pause the FE renders directly, never an error and never a poll.
func TestRunRitual_Passes200SkippedBudgetThrough(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"runId":"run-2","ritualId":"rit-1","revisionNo":1,"status":"skipped_budget","manaCharged":0,"startedAt":"2026-07-11T00:00:00Z"}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})

	resp, _ := b.RunRitual(context.Background(), basicAuth(), "fam-1", "rit-1", []byte(`{}`))
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (terminal at ack)", resp.Status)
	}
	if body := decodeJSON(t, resp.Body); body["status"] != "skipped_budget" {
		t.Errorf("status field = %v, want skipped_budget", body["status"])
	}
}

// Run history is the poll target: the terminal state the FE waits for arrives
// here, so the list must pass through with its statuses intact.
func TestListRitualRuns_PassesTerminalRunsThrough(t *testing.T) {
	cons := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"runs":[{"runId":"run-1","ritualId":"rit-1","revisionNo":1,"status":"completed","manaCharged":35,"startedAt":"2026-07-11T00:00:00Z","completedAt":"2026-07-11T00:00:31Z"}]}`))
	})
	b := companionbridge.New(companionbridge.Config{ConsumptionURL: cons.URL, PerCallTimeout: time.Second})

	resp, err := b.ListRitualRuns(context.Background(), basicAuth(), "fam-1", "rit-1")
	if err != nil {
		t.Fatalf("ListRitualRuns: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	runs, ok := decodeJSON(t, resp.Body)["runs"].([]any)
	if !ok || len(runs) != 1 {
		t.Fatalf("runs = %v, want 1", decodeJSON(t, resp.Body)["runs"])
	}
	run, _ := runs[0].(map[string]any)
	if run["status"] != "completed" || run["runId"] != "run-1" {
		t.Errorf("polled run = %v, want completed run-1", run)
	}
	if cons.lastPath != "/v1/me/companions/fam-1/rituals/rit-1/runs" {
		t.Errorf("path = %q", cons.lastPath)
	}
}
