// companion_bridge_learner_sovereign_test.go — HTTP route binding tests for the
// learner-sovereign Discovery Graph Companion routes bolted onto the existing
// CompanionBridge (ADR-149 growth routes live in companion_bridge_handler_test.go):
//
//	POST /api/v1/me/companions/acquire        → chora-consumption POST /v1/me/companions/acquire
//	GET  /api/v1/me/companions/bindings       → chora-consumption GET  /v1/me/companions/bindings
//	GET  /api/v1/me/companions/{id}/memory    → chora-consumption GET  /v1/me/companions/{id}/memory
//
// `acquire` + `bindings` are single-segment collection actions that would
// OTHERWISE be mis-parsed as a bare {id} instance GET — these tests pin the
// special-case dispatch (POST acquire, GET bindings) + the method gate.
//
// Reuses newConsumptionStub / newTenancyStub / newBridgeHandler / doReq from
// companion_bridge_handler_test.go (same package).
//
// Strict TDD: tests written BEFORE the handler + bridge implementation.
package httpadapter_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/companionbridge"
)

// captureConsumption stands up an inline chora-consumption fake that records the
// path + method + body it received, so the routing can be asserted precisely.
func captureConsumption(t *testing.T, status int, respBody string, gotPath, gotMethod, gotBody *string) http.Handler {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotPath = r.URL.Path
		*gotMethod = r.Method
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(buf)
		}
		*gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	cfg := companionbridge.Config{ConsumptionURL: srv.URL, PerCallTimeout: time.Second}
	return httpadapter.NewCompanionBridgeMux(companionbridge.New(cfg))
}

// -----------------------------------------------------------------------------
// POST /api/v1/me/companions/acquire — learner-sovereign Companion acquire.
// -----------------------------------------------------------------------------

func TestCompanionBridge_Acquire_DownstreamPathAndMethod(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	h := captureConsumption(t, http.StatusCreated,
		`{"companion_id":"f-new","binding_id":"b-1"}`, &gotPath, &gotMethod, &gotBody)

	w := doReq(t, h, http.MethodPost, "/api/v1/me/companions/acquire", `{"species":"dragon"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/me/companions/acquire" {
		t.Errorf("downstream path=%q want /v1/me/companions/acquire", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("downstream method=%q want POST", gotMethod)
	}
	// Body forwarded (species is snake-safe so it survives any camel→snake pass).
	if !strings.Contains(gotBody, "species") || !strings.Contains(gotBody, "dragon") {
		t.Errorf("acquire body not forwarded downstream: %q", gotBody)
	}
}

// `acquire` must NOT be mis-parsed as a bare {id} instance GET — the bare GET is
// GET-only, so a GET to /acquire proves the special-case (the acquire action is
// POST-only) is in force by returning 405 rather than proxying a GET.
func TestCompanionBridge_Acquire_WrongMethod_405(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	for _, m := range []string{http.MethodGet, http.MethodDelete, http.MethodPatch} {
		w := doReq(t, h, m, "/api/v1/me/companions/acquire", "")
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /acquire: status=%d want 405", m, w.Code)
		}
	}
}

// -----------------------------------------------------------------------------
// GET /api/v1/me/companions/bindings — list the learner's Companion bindings.
// -----------------------------------------------------------------------------

func TestCompanionBridge_Bindings_DownstreamPathAndMethod(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	h := captureConsumption(t, http.StatusOK,
		`{"bindings":[{"companion_id":"f1","concept_id":"c1"}]}`, &gotPath, &gotMethod, &gotBody)

	w := doReq(t, h, http.MethodGet, "/api/v1/me/companions/bindings", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/me/companions/bindings" {
		t.Errorf("downstream path=%q want /v1/me/companions/bindings", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("downstream method=%q want GET", gotMethod)
	}
}

func TestCompanionBridge_Bindings_WrongMethod_405(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	for _, m := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch} {
		w := doReq(t, h, m, "/api/v1/me/companions/bindings", `{}`)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /bindings: status=%d want 405", m, w.Code)
		}
	}
}

// -----------------------------------------------------------------------------
// GET /api/v1/me/companions/{id}/memory — the per-Companion RAG memory read.
// -----------------------------------------------------------------------------

func TestCompanionBridge_Memory_DownstreamPathAndMethod(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	h := captureConsumption(t, http.StatusOK,
		`{"companion_id":"f1","memory_items":[]}`, &gotPath, &gotMethod, &gotBody)

	w := doReq(t, h, http.MethodGet, "/api/v1/me/companions/f1/memory", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/me/companions/f1/memory" {
		t.Errorf("downstream path=%q want /v1/me/companions/f1/memory", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("downstream method=%q want GET", gotMethod)
	}
}

// Without the `case "memory"` leaf the switch default 404s; WITH it, a non-GET
// method 405s. The 405 (not 404) proves the memory leaf is wired + method-gated.
func TestCompanionBridge_Memory_WrongMethod_405(t *testing.T) {
	h := newBridgeHandler(t, newConsumptionStub(t), newTenancyStub(t))
	for _, m := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch} {
		w := doReq(t, h, m, "/api/v1/me/companions/f1/memory", `{}`)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /f1/memory: status=%d want 405", m, w.Code)
		}
	}
}
