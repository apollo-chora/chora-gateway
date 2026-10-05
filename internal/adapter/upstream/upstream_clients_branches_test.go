// upstream_clients_branches_test.go — residual branch coverage for the
// chora-a2a / chora-aikernel / chora-eval-evidence HTTP clients:
//
//   - envelope `items` variants + empty-body + bare-array + malformed JSON
//     fallback paths in GetContracts/GetIdentities/GetInvocations
//   - httpGetJSON's 404 (nil,nil), 400 (ErrUpstream), transport-failure,
//     traceparent + bearer stamping branches
//   - LoadAIKernelConfigFromEnv (env read)
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

func newA2A(addr string) *upstream.A2AClient {
	return upstream.NewA2AClient(upstream.A2AConfig{HTTPAddr: addr, PerCallTimeout: 2 * time.Second}, nil)
}

// ---------------------------------------------------------------------------
// a2a envelope / empty / malformed variants
// ---------------------------------------------------------------------------

func TestA2A_GetContracts_ItemsEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []upstream.A2AContract{{ContractID: "c-9", Status: "active"}},
		})
	}))
	defer srv.Close()

	cs, err := newA2A(srv.URL).GetContracts(context.Background(), "t1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(cs) != 1 || cs[0].ContractID != "c-9" {
		t.Errorf("contracts = %+v; want items-envelope decode", cs)
	}
}

func TestA2A_GetContracts_EmptyBodyReturnsEmptySlice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // zero-length body
	}))
	defer srv.Close()

	cs, err := newA2A(srv.URL).GetContracts(context.Background(), "t1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if cs == nil || len(cs) != 0 {
		t.Errorf("contracts = %#v; want empty non-nil slice", cs)
	}
}

func TestA2A_GetContracts_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{not-json`))
	}))
	defer srv.Close()

	if _, err := newA2A(srv.URL).GetContracts(context.Background(), "t1"); err == nil {
		t.Fatal("expected malformed-JSON error")
	}
}

func TestA2A_GetIdentities_ItemsAndEmpty(t *testing.T) {
	// items-envelope variant
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []upstream.A2AIdentity{{AGID: "agid-2"}},
		})
	}))
	ids, err := newA2A(srv.URL).GetIdentities(context.Background(), "t1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(ids) != 1 || ids[0].AGID != "agid-2" {
		t.Errorf("ids = %+v; want items-envelope decode", ids)
	}
	srv.Close()

	// empty body
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv2.Close()

	ids2, err := newA2A(srv2.URL).GetIdentities(context.Background(), "t1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ids2 == nil || len(ids2) != 0 {
		t.Errorf("ids = %#v; want empty non-nil slice", ids2)
	}
}

func TestA2A_GetInvocations_EmptyAndMalformed(t *testing.T) {
	// empty body
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	inv, err := newA2A(srv.URL).GetInvocations(context.Background(), "t1", "", 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if inv == nil || len(inv) != 0 {
		t.Errorf("invocations = %#v; want empty non-nil slice", inv)
	}
	srv.Close()

	// malformed JSON
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`nope`))
	}))
	defer srv2.Close()

	if _, err := newA2A(srv2.URL).GetInvocations(context.Background(), "t1", "", 0); err == nil {
		t.Fatal("expected malformed-JSON error")
	}
}

func TestA2A_GetContracts_404And400(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/a2a/contracts" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		// identities path → 404
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	ac := newA2A(srv.URL)
	// 400-family → ErrUpstream.
	if _, err := ac.GetContracts(context.Background(), "t1"); !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("400 err = %v; want ErrUpstream wrap", err)
	}
	// 404 → httpGetJSON returns (nil,nil), which the caller normalises to an
	// empty non-nil slice (no data ≠ error).
	ids, err := ac.GetIdentities(context.Background(), "t1")
	if err != nil {
		t.Fatalf("404 err = %v; want nil", err)
	}
	if ids == nil || len(ids) != 0 {
		t.Errorf("404 ids = %#v; want empty non-nil slice", ids)
	}
}

// TestA2A_HTTPGetJSON_TransportFailure — a closed listener surfaces the
// transport-failure branch (connection refused wrapped in ErrUpstream).
func TestA2A_HTTPGetJSON_TransportFailure(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	if _, err := newA2A(deadURL).GetContracts(context.Background(), "t1"); err == nil {
		t.Fatal("expected transport error from closed listener")
	}
}

// TestA2A_HTTPGetJSON_StampsTraceparentAndBearer — the outbound GET must
// forward traceparent + Authorization from the AuthCtx stash.
func TestA2A_HTTPGetJSON_StampsTraceparentAndBearer(t *testing.T) {
	var gotTP, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTP = r.Header.Get("traceparent")
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode([]upstream.A2AContract{})
	}))
	defer srv.Close()

	ctx := upstream.WithAuthCtx(context.Background(), upstream.AuthCtx{
		Traceparent: "00-tp-01",
		Bearer:      "tok-9",
	})
	// Inject via the request context helper if the client reads it — fall
	// back to a direct bearer-less call if not exported; see test below.
	_, err := newA2A(srv.URL).GetContracts(ctx, "t1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotTP != "00-tp-01" {
		t.Errorf("traceparent = %q; want 00-tp-01", gotTP)
	}
	if gotAuth != "Bearer tok-9" {
		t.Errorf("Authorization = %q; want Bearer tok-9", gotAuth)
	}
}

func TestA2A_GetInvocations_DefaultLimit50(t *testing.T) {
	var gotLimit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []upstream.A2AInvocation{}})
	}))
	defer srv.Close()

	// limit<=0 → default 50
	if _, err := newA2A(srv.URL).GetInvocations(context.Background(), "t1", "", 0); err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotLimit != "50" {
		t.Errorf("limit = %q; want 50 default", gotLimit)
	}
}

// ---------------------------------------------------------------------------
// aikernel
// ---------------------------------------------------------------------------

func TestLoadAIKernelConfigFromEnv_ReadsEnv(t *testing.T) {
	t.Setenv("AI_KERNEL_ORCHESTRATOR_URL", "http://aikernel.internal:8080")
	cfg := upstream.LoadAIKernelConfigFromEnv()
	if cfg.HTTPAddr != "http://aikernel.internal:8080" {
		t.Errorf("HTTPAddr = %q; want env value", cfg.HTTPAddr)
	}
	if cfg.PerCallTimeout <= 0 {
		t.Errorf("PerCallTimeout = %s; want a positive default", cfg.PerCallTimeout)
	}
}

func TestAIKernel_GetPromptVersions_EmptyAddrErr(t *testing.T) {
	kc := upstream.NewAIKernelClient(upstream.AIKernelConfig{}, nil)
	if _, err := kc.GetPromptVersions(context.Background(), "t1", "p1"); err == nil {
		t.Fatal("expected error when HTTPAddr empty")
	}
}
