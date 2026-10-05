package upstream_test

// CHO-2368 — gateway → chora-ai-kernel-orchestrator prompt catalogue reads.
// Same idiom as observability_client_test.go: httptest backend asserting path
// + headers, raw-passthrough body, 404 → (nil, nil), no-addr → ErrUpstream.

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

func aikernelClientFor(t *testing.T, srv *httptest.Server) *upstream.AIKernelClient {
	t.Helper()
	return upstream.NewAIKernelClient(upstream.AIKernelConfig{
		HTTPAddr:       srv.URL,
		PerCallTimeout: 2 * time.Second,
	}, srv.Client())
}

func TestAIKernelClient_GetPromptVersions_Happy(t *testing.T) {
	var gotPath, gotRoles string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRoles = r.Header.Get("x-mesh-user-roles")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agent_id":"qgen_question","versions":[{"version":"v1","future_field":"kept"}],"total":1}`))
	}))
	defer srv.Close()

	ctx := upstream.WithAuthCtx(context.Background(), upstream.AuthCtx{
		GCID: "gcid-1", TenantID: "t-1", Roles: []string{"auditor"},
	})
	raw, err := aikernelClientFor(t, srv).GetPromptVersions(ctx, "t-1", "qgen_question")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/v1/prompt-registry/agents/qgen_question/versions" {
		t.Errorf("wrong upstream path: %s", gotPath)
	}
	if gotRoles == "" {
		t.Errorf("mesh roles header missing (role-gated upstreams fail closed)")
	}
	if string(raw) == "" || !json.Valid(raw) {
		t.Fatalf("expected raw JSON passthrough, got %q", raw)
	}
}

func TestAIKernelClient_GetPromptVersionContent_EncodesSegments(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"version":"1.1.0"}`))
	}))
	defer srv.Close()

	_, err := aikernelClientFor(t, srv).GetPromptVersionContent(context.Background(), "t-1", "oe_evaluator", "1.1.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/v1/prompt-registry/agents/oe_evaluator/versions/1.1.0" {
		t.Errorf("wrong upstream path: %s", gotPath)
	}
}

func TestAIKernelClient_NotFound_ReturnsNilNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	raw, err := aikernelClientFor(t, srv).GetPromptVersions(context.Background(), "t-1", "nope")
	if err != nil {
		t.Fatalf("404 must map to (nil, nil), got err=%v", err)
	}
	if raw != nil {
		t.Fatalf("404 must map to (nil, nil), got body=%q", raw)
	}
}

func TestAIKernelClient_NoAddr_ErrUpstream(t *testing.T) {
	c := upstream.NewAIKernelClient(upstream.AIKernelConfig{}, nil)
	_, err := c.GetPromptVersions(context.Background(), "t-1", "qgen_question")
	if !errors.Is(err, upstream.ErrUpstream) {
		t.Fatalf("want ErrUpstream, got %v", err)
	}
}
