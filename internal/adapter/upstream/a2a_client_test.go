// a2a_client_test.go — Phase C tests for the chora-a2a HTTP client. Covers
// both the "live" mode and the explicit "pending" sentinel.
package upstream_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

func TestA2AClient_PendingWhenAddrEmpty(t *testing.T) {
	ac := upstream.NewA2AClient(upstream.A2AConfig{}, nil)
	if !ac.Pending() {
		t.Fatal("Pending() = false; want true when HTTPAddr is empty")
	}
	if _, err := ac.GetContracts(context.Background(), "t1"); !errors.Is(err, upstream.ErrA2APending) {
		t.Errorf("GetContracts err = %v; want ErrA2APending", err)
	}
	if _, err := ac.GetIdentities(context.Background(), "t1"); !errors.Is(err, upstream.ErrA2APending) {
		t.Errorf("GetIdentities err = %v; want ErrA2APending", err)
	}
	if _, err := ac.GetInvocations(context.Background(), "t1", "", 0); !errors.Is(err, upstream.ErrA2APending) {
		t.Errorf("GetInvocations err = %v; want ErrA2APending", err)
	}
}

func TestA2AClient_GetContracts_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/a2a/contracts" {
			t.Errorf("path = %s", r.URL.Path)
		}
		body := []upstream.A2AContract{
			{ContractID: "c-1", TenantID: "t1", PartnerID: "partner-a", Status: "active"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	ac := upstream.NewA2AClient(upstream.A2AConfig{HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second}, nil)
	cs, err := ac.GetContracts(context.Background(), "t1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(cs) != 1 || cs[0].ContractID != "c-1" {
		t.Errorf("contracts = %+v", cs)
	}
}

func TestA2AClient_GetIdentities_Envelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{
			"identities": []upstream.A2AIdentity{
				{AGID: "agid-1", DisplayName: "External Agent A"},
			},
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	ac := upstream.NewA2AClient(upstream.A2AConfig{HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second}, nil)
	ids, err := ac.GetIdentities(context.Background(), "t1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(ids) != 1 || ids[0].AGID != "agid-1" {
		t.Errorf("ids = %+v", ids)
	}
}

func TestA2AClient_GetInvocations_Happy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("since") == "" {
			t.Error("since query param missing")
		}
		body := map[string]any{
			"items": []upstream.A2AInvocation{
				{InvocationID: "inv-1", TenantID: "t1", Outcome: "permitted"},
				{InvocationID: "inv-2", TenantID: "t1", Outcome: "denied"},
			},
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	ac := upstream.NewA2AClient(upstream.A2AConfig{HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second}, nil)
	inv, err := ac.GetInvocations(context.Background(), "t1", time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339), 50)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(inv) != 2 {
		t.Errorf("invocations = %d; want 2", len(inv))
	}
}

// Producer-supplied endpoint MUST round-trip through the BFF client decoder
// onto the A2AInvocation struct — clearing the M12 TODO placeholder in the
// BFF transformer that previously defaulted to "unknown".
func TestA2AClient_GetInvocations_DecodesEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{
			"invocations": []map[string]any{
				{
					"invocation_id": "inv-3",
					"tenant_id":     "t1",
					"outcome":       "permitted",
					"endpoint":      "https://a2a.chora.site/a2a/invoke#recommend_content",
				},
			},
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	ac := upstream.NewA2AClient(upstream.A2AConfig{HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second}, nil)
	inv, err := ac.GetInvocations(context.Background(), "t1", "", 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(inv) != 1 {
		t.Fatalf("invocations = %d; want 1", len(inv))
	}
	if inv[0].Endpoint != "https://a2a.chora.site/a2a/invoke#recommend_content" {
		t.Errorf("Endpoint = %q; want canonical capability URL preserved", inv[0].Endpoint)
	}
}

func TestA2AClient_GetContracts_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	ac := upstream.NewA2AClient(upstream.A2AConfig{HTTPAddr: srv.URL, PerCallTimeout: 1 * time.Second}, nil)
	if _, err := ac.GetContracts(context.Background(), "t1"); !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("err = %v; want ErrUpstream wrap", err)
	}
}

func TestLoadA2AConfigFromEnv(t *testing.T) {
	t.Setenv("CHORA_A2A_HTTP_ADDR", "http://chora-a2a:8080")
	c := upstream.LoadA2AConfigFromEnv()
	if c.HTTPAddr != "http://chora-a2a:8080" {
		t.Errorf("addr = %s", c.HTTPAddr)
	}
}
