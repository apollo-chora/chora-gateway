package httpadapter_test

// CHO-2368 — /bff/oplus/prompts/{agent_id}/versions[/{version}] catalogue
// passthrough. Three-test-per-route convention + the raw-passthrough
// future_field proof, mirroring TestOPlusHandler_AgentPrompts_*.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

func newPromptsCatalogueHandler(t *testing.T, backend http.HandlerFunc) *httpadapter.OPlusHandler {
	t.Helper()
	h := newOPlusHandlerForTest(t)
	if backend != nil {
		srv := httptest.NewServer(backend)
		t.Cleanup(srv.Close)
		h.AIKernel = upstream.NewAIKernelClient(upstream.AIKernelConfig{
			HTTPAddr:       srv.URL,
			PerCallTimeout: 2 * time.Second,
		}, srv.Client())
	}
	return h
}

func TestOPlusHandler_PromptCatalogue_VersionsPassThrough(t *testing.T) {
	var gotPath string
	h := newPromptsCatalogueHandler(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"agent_id":"qgen_question","versions":[{"version":"v1","future_field":"kept"}],"total":1}`))
	})
	rr := httptest.NewRecorder()
	h.PromptCatalogue(rr, httptest.NewRequest(http.MethodGet, "/bff/oplus/prompts/qgen_question/versions", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if gotPath != "/v1/prompt-registry/agents/qgen_question/versions" {
		t.Errorf("wrong upstream path: %s", gotPath)
	}
	var body struct {
		State    string          `json:"state"`
		Registry json.RawMessage `json:"registry"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.State != "live" {
		t.Errorf("want state=live, got %s", body.State)
	}
	var reg map[string]any
	if err := json.Unmarshal(body.Registry, &reg); err != nil {
		t.Fatalf("registry not raw JSON: %v", err)
	}
	versions, _ := reg["versions"].([]any)
	if len(versions) != 1 {
		t.Fatalf("want 1 version, got %v", reg["versions"])
	}
	first, _ := versions[0].(map[string]any)
	if first["future_field"] != "kept" {
		t.Errorf("raw passthrough must keep unknown upstream fields")
	}
}

func TestOPlusHandler_PromptCatalogue_VersionContentPassThrough(t *testing.T) {
	var gotPath string
	h := newPromptsCatalogueHandler(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"agent_id":"companion","version":"v1","segments":[{"segment_id":"untrusted_fence","locked":true}]}`))
	})
	rr := httptest.NewRecorder()
	h.PromptCatalogue(rr, httptest.NewRequest(http.MethodGet, "/bff/oplus/prompts/companion/versions/v1", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if gotPath != "/v1/prompt-registry/agents/companion/versions/v1" {
		t.Errorf("wrong upstream path: %s", gotPath)
	}
	if !strings.Contains(rr.Body.String(), `"locked":true`) {
		t.Errorf("segment locked flag must pass through: %s", rr.Body.String())
	}
}

func TestOPlusHandler_PromptCatalogue_RejectsNonGET(t *testing.T) {
	h := newPromptsCatalogueHandler(t, func(w http.ResponseWriter, r *http.Request) {})
	rr := httptest.NewRecorder()
	h.PromptCatalogue(rr, httptest.NewRequest(http.MethodPost, "/bff/oplus/prompts/companion/versions", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rr.Code)
	}
}

func TestOPlusHandler_PromptCatalogue_BadPaths404(t *testing.T) {
	h := newPromptsCatalogueHandler(t, func(w http.ResponseWriter, r *http.Request) {})
	for _, path := range []string{
		"/bff/oplus/prompts/",
		"/bff/oplus/prompts/companion",
		"/bff/oplus/prompts/companion/other",
		"/bff/oplus/prompts/companion/versions/v1/extra",
	} {
		rr := httptest.NewRecorder()
		h.PromptCatalogue(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s: want 404, got %d", path, rr.Code)
		}
	}
}

func TestOPlusHandler_PromptCatalogue_UpstreamNotFound404(t *testing.T) {
	h := newPromptsCatalogueHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	rr := httptest.NewRecorder()
	h.PromptCatalogue(rr, httptest.NewRequest(http.MethodGet, "/bff/oplus/prompts/nope/versions", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404 for unknown agent, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestOPlusHandler_PromptCatalogue_UpstreamError(t *testing.T) {
	h := newPromptsCatalogueHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	rr := httptest.NewRecorder()
	h.PromptCatalogue(rr, httptest.NewRequest(http.MethodGet, "/bff/oplus/prompts/companion/versions", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("upstream failure collapses to 200 envelope, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"state":"error"`) {
		t.Errorf("want state=error envelope, got %s", rr.Body.String())
	}
}

func TestOPlusHandler_PromptCatalogue_UnconfiguredUpstream(t *testing.T) {
	h := newPromptsCatalogueHandler(t, nil) // no AIKernel client wired
	rr := httptest.NewRecorder()
	h.PromptCatalogue(rr, httptest.NewRequest(http.MethodGet, "/bff/oplus/prompts/companion/versions", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("unconfigured upstream collapses to 200 envelope, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"state":"error"`) {
		t.Errorf("want state=error envelope, got %s", rr.Body.String())
	}
}
