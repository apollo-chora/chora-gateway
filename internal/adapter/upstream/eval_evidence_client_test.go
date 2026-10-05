package upstream_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

func evalClient(url string) *upstream.ObservabilityClient {
	return upstream.NewObservabilityClient(upstream.ObservabilityConfig{
		HTTPAddr: url, PerCallTimeout: 2 * time.Second,
	}, nil)
}

func TestGetEvalRuns_NotWired(t *testing.T) {
	c := evalClient("")
	_, err := c.GetEvalRuns(context.Background(), "t1", "", "")
	if !errors.Is(err, upstream.ErrUpstream) {
		t.Fatalf("err = %v; want ErrUpstream", err)
	}
}

func TestGetEvalRuns_HappyAndForwards(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		_, _ = w.Write([]byte(`{"runs":[{"candidate_label":"r1","last_recorded_at":"2026-06-06T20:00:00Z","members":[{"experiment":"chora-agent-eval-qgen-question","autorater_metrics":[{"metric":"safety","count":4,"avg_score":1}],"adversarial":{"total":6,"blocked":6,"leaked":0}}]}]}`))
	}))
	defer srv.Close()
	out, err := evalClient(srv.URL).GetEvalRuns(context.Background(), "t1", "chora-agent-eval-qgen-question", "autorater")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(out.Runs) != 1 || out.Runs[0].CandidateLabel != "r1" {
		t.Fatalf("runs = %+v", out.Runs)
	}
	if out.Runs[0].Members[0].Adversarial == nil || out.Runs[0].Members[0].Adversarial.Blocked != 6 {
		t.Errorf("adversarial parse: %+v", out.Runs[0].Members[0].Adversarial)
	}
	if !strings.Contains(gotURL, "experiment=chora-agent-eval-qgen-question") || !strings.Contains(gotURL, "kind=autorater") || !strings.Contains(gotURL, "tenant_id=t1") {
		t.Errorf("query not forwarded: %q", gotURL)
	}
}

func TestGetEvalRuns_EmptyBodyIsEmptySlice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound) // httpGetJSON maps 404 -> nil body
	}))
	defer srv.Close()
	out, err := evalClient(srv.URL).GetEvalRuns(context.Background(), "t1", "", "")
	if err != nil {
		t.Fatalf("err = %v; want nil on 404", err)
	}
	if out.Runs == nil || len(out.Runs) != 0 {
		t.Errorf("runs = %+v; want non-nil empty", out.Runs)
	}
}

func TestGetEvalRuns_Malformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer srv.Close()
	_, err := evalClient(srv.URL).GetEvalRuns(context.Background(), "t1", "", "")
	if !errors.Is(err, upstream.ErrUpstream) {
		t.Fatalf("err = %v; want ErrUpstream", err)
	}
}

func TestGetEvalRunEvidence_HappyAndPathEscaped(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"candidate_label":"depbump0607","rows":[{"experiment":"chora-agent-eval-qgen-question","kind":"adversarial","metric":"jailbreak","score":1,"adversarial_verdict":"BLOCKED(pass)","recorded_at":"2026-06-06T20:00:00Z"}]}`))
	}))
	defer srv.Close()
	out, err := evalClient(srv.URL).GetEvalRunEvidence(context.Background(), "t1", "depbump0607", "", "adversarial", 10)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if out.CandidateLabel != "depbump0607" || len(out.Rows) != 1 {
		t.Fatalf("out = %+v", out)
	}
	if out.Rows[0].AdversarialVerdict != "BLOCKED(pass)" {
		t.Errorf("verdict = %q", out.Rows[0].AdversarialVerdict)
	}
	if gotPath != "/api/v1/observability/eval-runs/depbump0607" {
		t.Errorf("path = %q", gotPath)
	}
}
