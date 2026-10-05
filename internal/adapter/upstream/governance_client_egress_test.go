// governance_client_egress_test.go — tests for the O+ external-egress audit
// read (CHO-2245). QueryEgressAudit calls GET /api/audit?action=external_egress
// on chora-governance (HTTP) and maps upstream status honestly: a governance
// DENY (4xx) becomes a *GovernanceReadError (surfaced as 4xx by the BFF, never
// masked as a 5xx); 5xx / transport → ErrUpstream.
package upstream_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

func newEgressAuditMock(t *testing.T, status int, body string) (*httptest.Server, *string) {
	t.Helper()
	var gotAction string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/audit" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotAction = r.URL.Query().Get("action")
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &gotAction
}

func newEgressClient(t *testing.T, httpAddr string) *upstream.GovernanceClient {
	t.Helper()
	return upstream.NewGovernanceClient(upstream.GovernanceClientConfig{
		Endpoint: upstream.GovernanceConfig{HTTPAddr: httpAddr, PerCallTimeout: 2 * time.Second},
	})
}

func TestGovernanceClient_QueryEgressAudit_Happy(t *testing.T) {
	t.Parallel()
	const body = `{"items":[
		{"event_id":"e1","tenant_id":"t1","action":"external_egress","decision":"denied",
		 "reason":"external egress denied; denial_reason=model_armor_pre_block",
		 "subject_type":"agent","subject_id":"companion_seeker",
		 "created_at":"2026-07-17T10:00:00Z",
		 "after":{"agentId":"companion_seeker","denialReason":"model_armor_pre_block","result":"AUDIT_RESULT_DENIED"}}
	],"total":1}`
	srv, gotAction := newEgressAuditMock(t, http.StatusOK, body)
	client := newEgressClient(t, srv.URL)

	got, err := client.QueryEgressAudit(context.Background(), "t1", 50)
	if err != nil {
		t.Fatalf("QueryEgressAudit: %v", err)
	}
	if *gotAction != "external_egress" {
		t.Errorf("upstream action param = %q; want external_egress", *gotAction)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events; want 1", len(got))
	}
	if got[0].Action != "external_egress" {
		t.Errorf("Action = %q; want external_egress", got[0].Action)
	}
	if got[0].Decision != "denied" {
		t.Errorf("Decision = %q; want denied", got[0].Decision)
	}
	if got[0].SubjectID != "companion_seeker" {
		t.Errorf("SubjectID = %q; want companion_seeker", got[0].SubjectID)
	}
	// The rich payload (armor verdicts, denial_reason, ...) rides through in After.
	if len(got[0].After) == 0 {
		t.Errorf("After payload should be preserved for O+ drill-down")
	}
}

// A governance DENY (403) must surface as a *GovernanceReadError carrying the
// status so the BFF returns 4xx — never a masked 5xx.
func TestGovernanceClient_QueryEgressAudit_Deny4xx(t *testing.T) {
	t.Parallel()
	srv, _ := newEgressAuditMock(t, http.StatusForbidden, "")
	client := newEgressClient(t, srv.URL)

	_, err := client.QueryEgressAudit(context.Background(), "t1", 50)
	if err == nil {
		t.Fatalf("expected error on upstream 403")
	}
	var re *upstream.GovernanceReadError
	if !errors.As(err, &re) {
		t.Fatalf("want *GovernanceReadError; got %T (%v)", err, err)
	}
	if re.StatusCode != http.StatusForbidden {
		t.Errorf("StatusCode = %d; want 403", re.StatusCode)
	}
}

func TestGovernanceClient_QueryEgressAudit_Upstream5xx(t *testing.T) {
	t.Parallel()
	srv, _ := newEgressAuditMock(t, http.StatusInternalServerError, "")
	client := newEgressClient(t, srv.URL)

	_, err := client.QueryEgressAudit(context.Background(), "t1", 50)
	if err == nil {
		t.Fatalf("expected error on upstream 500")
	}
	if !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("want ErrUpstream on 5xx; got %v", err)
	}
	var re *upstream.GovernanceReadError
	if errors.As(err, &re) {
		t.Errorf("5xx must NOT be a GovernanceReadError (4xx-only); got status %d", re.StatusCode)
	}
}

func TestGovernanceClient_QueryEgressAudit_NotWired(t *testing.T) {
	t.Parallel()
	client := newEgressClient(t, "") // empty HTTP addr
	_, err := client.QueryEgressAudit(context.Background(), "t1", 50)
	if err == nil || !errors.Is(err, upstream.ErrUpstream) {
		t.Fatalf("want ErrUpstream when HTTP addr unwired; got %v", err)
	}
}
