// gatewayproxy_branches_test.go — residual fan-out coverage for the
// gatewayproxy aggregator methods the RED-phase suite never reached:
// question CRUD + job lifecycle, atom lifecycle, proofing-tests,
// enrolments, tenant-member search, collections convert, learning paths,
// maps / ai-transparency passthroughs, question-banks, and the streaming
// mana-ledger export. Also pins classify()'s 501/503 passthrough + the
// request-canceled branch.
package gatewayproxy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

func newGBF(cfg gatewayproxy.Config) *gatewayproxy.Aggregator {
	if cfg.PerCallTimeout == 0 {
		cfg.PerCallTimeout = time.Second
	}
	return gatewayproxy.New(cfg)
}

// ---------------------------------------------------------------------------
// Question CRUD (P7)
// ---------------------------------------------------------------------------

func TestGBF_QuestionCRUD_ProxiesVerbatim(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"ok":true}`)
	a := newGBF(gatewayproxy.Config{CreationURL: cb.srv.URL})

	// PATCH edit
	resp, err := a.EditQuestion(context.Background(), sampleAuth(), "atom-1", "q-1", []byte(`{"stem":"x"}`))
	if err != nil {
		t.Fatalf("EditQuestion: %v", err)
	}
	if resp.Status != http.StatusOK || cb.method != http.MethodPatch || cb.path != "/api/atoms/atom-1/questions/q-1" {
		t.Errorf("EditQuestion → %s %s status=%d", cb.method, cb.path, resp.Status)
	}

	// GET read
	if _, err := a.GetQuestion(context.Background(), sampleAuth(), "atom-1", "q-1"); err != nil {
		t.Fatalf("GetQuestion: %v", err)
	}
	if cb.method != http.MethodGet || cb.path != "/api/atoms/atom-1/questions/q-1" {
		t.Errorf("GetQuestion → %s %s", cb.method, cb.path)
	}

	// DELETE
	if _, err := a.DeleteQuestion(context.Background(), sampleAuth(), "atom-1", "q-1"); err != nil {
		t.Fatalf("DeleteQuestion: %v", err)
	}
	if cb.method != http.MethodDelete {
		t.Errorf("DeleteQuestion method = %s; want DELETE", cb.method)
	}

	// Empty ids → 404 before any outbound call.
	for _, c := range []struct {
		name string
		call func() (gatewayproxy.Response, error)
	}{
		{"EditQuestion atom", func() (gatewayproxy.Response, error) {
			return a.EditQuestion(context.Background(), sampleAuth(), "", "q-1", nil)
		}},
		{"EditQuestion question", func() (gatewayproxy.Response, error) {
			return a.EditQuestion(context.Background(), sampleAuth(), "atom-1", "", nil)
		}},
		{"GetQuestion atom", func() (gatewayproxy.Response, error) {
			return a.GetQuestion(context.Background(), sampleAuth(), "", "q-1")
		}},
		{"GetQuestion question", func() (gatewayproxy.Response, error) {
			return a.GetQuestion(context.Background(), sampleAuth(), "atom-1", "")
		}},
		{"DeleteQuestion atom", func() (gatewayproxy.Response, error) {
			return a.DeleteQuestion(context.Background(), sampleAuth(), "", "q-1")
		}},
		{"DeleteQuestion question", func() (gatewayproxy.Response, error) {
			return a.DeleteQuestion(context.Background(), sampleAuth(), "atom-1", "")
		}},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			resp, err := c.call()
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if resp.Status != http.StatusNotFound {
				t.Errorf("%s status = %d; want 404", c.name, resp.Status)
			}
		})
	}
}

func TestGBF_QuestionJobLifecycle_ProxiesVerbatim(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusAccepted, `{"job_id":"j-1"}`)
	a := newGBF(gatewayproxy.Config{CreationURL: cb.srv.URL})

	// POST create job (content-type preserved)
	resp, err := a.CreateQuestionJob(context.Background(), sampleAuth(), "atom-1", "application/json", []byte(`{"kind":"ai_draft"}`))
	if err != nil {
		t.Fatalf("CreateQuestionJob: %v", err)
	}
	if resp.Status != http.StatusAccepted || cb.method != http.MethodPost || cb.path != "/api/atoms/atom-1/question-jobs" {
		t.Errorf("CreateQuestionJob → %s %s status=%d", cb.method, cb.path, resp.Status)
	}

	// GET poll
	if _, err := a.GetQuestionJob(context.Background(), sampleAuth(), "atom-1", "j-1"); err != nil {
		t.Fatalf("GetQuestionJob: %v", err)
	}
	if cb.method != http.MethodGet || cb.path != "/api/atoms/atom-1/question-jobs/j-1" {
		t.Errorf("GetQuestionJob → %s %s", cb.method, cb.path)
	}

	// POST accept
	if _, err := a.AcceptQuestionJob(context.Background(), sampleAuth(), "atom-1", "j-1", []byte(`{"candidates":[]}`)); err != nil {
		t.Fatalf("AcceptQuestionJob: %v", err)
	}
	if cb.path != "/api/atoms/atom-1/question-jobs/j-1/accept" {
		t.Errorf("AcceptQuestionJob path = %s", cb.path)
	}

	// POST regenerate-image
	if _, err := a.RegenerateQuestionJobImage(context.Background(), sampleAuth(), "atom-1", "j-1", []byte(`{"draft_id":"d1"}`)); err != nil {
		t.Fatalf("RegenerateQuestionJobImage: %v", err)
	}
	if cb.path != "/api/atoms/atom-1/question-jobs/j-1/regenerate-image" {
		t.Errorf("RegenerateQuestionJobImage path = %s", cb.path)
	}

	// POST model-answer job
	if _, err := a.CreateModelAnswerJob(context.Background(), sampleAuth(), "atom-1", "q-1", []byte(`{}`)); err != nil {
		t.Fatalf("CreateModelAnswerJob: %v", err)
	}
	if cb.path != "/api/atoms/atom-1/questions/q-1/ai-model-answer-jobs" {
		t.Errorf("CreateModelAnswerJob path = %s", cb.path)
	}

	// Empty job id → 404 pre-outbound.
	resp, err = a.GetQuestionJob(context.Background(), sampleAuth(), "atom-1", "")
	if err != nil {
		t.Fatalf("GetQuestionJob empty job: %v", err)
	}
	if resp.Status != http.StatusNotFound {
		t.Errorf("GetQuestionJob empty job status = %d; want 404", resp.Status)
	}
}

func TestGBF_QuestionTypes_Passthrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[]}`)
	a := newGBF(gatewayproxy.Config{CreationURL: cb.srv.URL})
	resp, err := a.GetQuestionTypes(context.Background(), sampleAuth())
	if err != nil {
		t.Fatalf("GetQuestionTypes: %v", err)
	}
	if resp.Status != http.StatusOK || cb.path != "/api/atoms/question-types" {
		t.Errorf("GetQuestionTypes → %s status=%d", cb.path, resp.Status)
	}
}

// ---------------------------------------------------------------------------
// Atom lifecycle
// ---------------------------------------------------------------------------

func TestGBF_AtomLifecycle_Passthrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusCreated, `{"atom_id":"atom-1"}`)
	a := newGBF(gatewayproxy.Config{CreationURL: cb.srv.URL})

	// POST create
	resp, err := a.CreateAtom(context.Background(), sampleAuth(), []byte(`{"title":"x"}`))
	if err != nil {
		t.Fatalf("CreateAtom: %v", err)
	}
	if resp.Status != http.StatusCreated || cb.method != http.MethodPost || cb.path != "/api/atoms" {
		t.Errorf("CreateAtom → %s %s status=%d", cb.method, cb.path, resp.Status)
	}

	// GET list (query forwarded)
	if _, err := a.ListAtoms(context.Background(), sampleAuth(), "status=published&page=1"); err != nil {
		t.Fatalf("ListAtoms: %v", err)
	}
	if cb.rawQ != "status=published&page=1" || cb.path != "/api/atoms" {
		t.Errorf("ListAtoms → %s?%s", cb.path, cb.rawQ)
	}

	// POST publish
	if _, err := a.PublishAtom(context.Background(), sampleAuth(), "atom-1"); err != nil {
		t.Fatalf("PublishAtom: %v", err)
	}
	if cb.path != "/api/atoms/atom-1/publish" {
		t.Errorf("PublishAtom path = %s", cb.path)
	}

	// Empty atom id → 404.
	resp, err = a.PublishAtom(context.Background(), sampleAuth(), "")
	if err != nil {
		t.Fatalf("PublishAtom empty: %v", err)
	}
	if resp.Status != http.StatusNotFound {
		t.Errorf("PublishAtom empty id status = %d; want 404", resp.Status)
	}
}

// ---------------------------------------------------------------------------
// Read-led passthroughs
// ---------------------------------------------------------------------------

func TestGBF_ReadLedPassthroughs(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[]}`)
	a := newGBF(gatewayproxy.Config{
		ConsumptionURL: cb.srv.URL,
		DeliveryURL:    cb.srv.URL,
		IdentityURL:    cb.srv.URL,
	})

	cases := []struct {
		name string
		call func() (gatewayproxy.Response, error)
	}{
		{"ListProofingTests", func() (gatewayproxy.Response, error) {
			return a.ListProofingTests(context.Background(), sampleAuth(), "goal_id=g1")
		}},
		{"ListMyEnrolments", func() (gatewayproxy.Response, error) {
			return a.ListMyEnrolments(context.Background(), sampleAuth(), "page=1")
		}},
		{"SearchTenantMembers", func() (gatewayproxy.Response, error) {
			return a.SearchTenantMembers(context.Background(), sampleAuth(), "q=a")
		}},
		{"MeLearningPaths", func() (gatewayproxy.Response, error) {
			return a.MeLearningPaths(context.Background(), sampleAuth(), "course_id=c1")
		}},
	}
	wantPaths := map[string]string{
		"ListProofingTests":   "/v1/me/proofing-tests",
		"ListMyEnrolments":    "/v1/me/enrolments",
		"SearchTenantMembers": "/api/v1/admin/tenant-members",
		"MeLearningPaths":     "/v1/me/learning-paths",
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			resp, err := tc.call()
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if resp.Status != http.StatusOK {
				t.Errorf("%s status = %d; want 200", tc.name, resp.Status)
			}
			if cb.path != wantPaths[tc.name] {
				t.Errorf("%s path = %s; want %s", tc.name, cb.path, wantPaths[tc.name])
			}
			if cb.rawQ == "" {
				t.Errorf("%s: raw query not forwarded", tc.name)
			}
		})
	}
}

func TestGBF_ProxyCourses_Maps_Search(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `[]`)
	a := newGBF(gatewayproxy.Config{DeliveryURL: cb.srv.URL, ConsumptionURL: cb.srv.URL, CreationURL: cb.srv.URL})

	// ProxyCourses — pure passthrough to delivery.
	if _, err := a.ProxyCourses(context.Background(), sampleAuth(), http.MethodGet, "/v1/courses", "page=1", nil, ""); err != nil {
		t.Fatalf("ProxyCourses: %v", err)
	}
	if cb.path != "/v1/courses" || cb.rawQ != "page=1" {
		t.Errorf("ProxyCourses → %s?%s", cb.path, cb.rawQ)
	}

	// ProxyQuestionSearch — /api prefix preserved on creation.
	if _, err := a.ProxyQuestionSearch(context.Background(), sampleAuth(), "q=math"); err != nil {
		t.Fatalf("ProxyQuestionSearch: %v", err)
	}
	if cb.path != "/api/atoms/questions/search" {
		t.Errorf("ProxyQuestionSearch path = %s", cb.path)
	}
}

func TestGBF_ProxyMapsAndAITransparency_StripAPIPrefix(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"ok":true}`)
	a := newGBF(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, GovernanceURL: cb.srv.URL})

	for _, tc := range []struct {
		name     string
		wantPath string
		call     func() (gatewayproxy.Response, error)
	}{
		{"ProxyMaps", "/v1/me/maps", func() (gatewayproxy.Response, error) {
			return a.ProxyMaps(context.Background(), sampleAuth(), http.MethodGet, "/api/v1/me/maps", "limit=10", nil, "")
		}},
		{"ProxyDosePreferences", "/v1/me/dose-preferences", func() (gatewayproxy.Response, error) {
			return a.ProxyDosePreferences(context.Background(), sampleAuth(), http.MethodGet, "/api/v1/me/dose-preferences", "", nil, "")
		}},
		{"ProxyAITransparency", "/v1/me/ai-transparency", func() (gatewayproxy.Response, error) {
			return a.ProxyAITransparency(context.Background(), sampleAuth(), http.MethodGet, "/api/v1/me/ai-transparency", "", nil, "")
		}},
		{"ProxyConceptGraph", "/v1/me/concept-graph", func() (gatewayproxy.Response, error) {
			return a.ProxyConceptGraph(context.Background(), sampleAuth(), http.MethodGet, "/api/v1/me/concept-graph", "", nil, "")
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			resp, err := tc.call()
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if resp.Status != http.StatusOK {
				t.Errorf("%s status = %d; want 200", tc.name, resp.Status)
			}
			if cb.path != tc.wantPath {
				t.Errorf("%s path = %s; want %s (api prefix stripped)", tc.name, cb.path, tc.wantPath)
			}
		})
	}
}

func TestGBF_ConvertCollectionToStudyList(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"study_list_id":"s-1"}`)
	a := newGBF(gatewayproxy.Config{CreationURL: cb.srv.URL})

	resp, err := a.ConvertCollectionToStudyList(context.Background(), sampleAuth(), "col-1", []byte(`{}`))
	if err != nil {
		t.Fatalf("ConvertCollectionToStudyList: %v", err)
	}
	if resp.Status != http.StatusOK || cb.path != "/api/v1/collections/col-1/convert-to-study-list" {
		t.Errorf("convert → %s status=%d", cb.path, resp.Status)
	}

	resp, err = a.ConvertCollectionToStudyList(context.Background(), sampleAuth(), "", nil)
	if err != nil {
		t.Fatalf("convert empty id: %v", err)
	}
	if resp.Status != http.StatusNotFound {
		t.Errorf("convert empty id status = %d; want 404", resp.Status)
	}
}

// ---------------------------------------------------------------------------
// Streaming mana-ledger export
// ---------------------------------------------------------------------------

func TestGBF_ExportManaLedger_HappyAndErrors(t *testing.T) {
	// Happy path: downstream 200 + CSV content-type flows through.
	var gotPath, gotRawQ string
	cb := &captureBackend{}
	cb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRawQ = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", `attachment; filename="ledger.csv"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("gcid,amount\nx,10"))
	}))
	t.Cleanup(cb.srv.Close)

	rec := httptest.NewRecorder()
	a := newGBF(gatewayproxy.Config{IdentityURL: cb.srv.URL})
	err := a.ExportManaLedger(context.Background(), sampleAuth(), "from=2026-01-01", rec)
	if err != nil {
		t.Fatalf("ExportManaLedger: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/csv" {
		t.Errorf("Content-Type = %q; want text/csv preserved", ct)
	}
	if !strings.Contains(rec.Body.String(), "gcid,amount") {
		t.Errorf("body not streamed: %q", rec.Body.String())
	}
	if gotPath != "/api/v1/me/mana/ledger/export" {
		t.Errorf("path = %q; want /api/v1/me/mana/ledger/export", gotPath)
	}
	if gotRawQ != "from=2026-01-01" {
		t.Errorf("rawQuery not forwarded: %q", gotRawQ)
	}

	// Unconfigured → 502 streaming error. (New() returns nil when every URL
	// is unset, so exercise the unconfigured branch via a different URL.)
	rec2 := httptest.NewRecorder()
	a2 := newGBF(gatewayproxy.Config{CreationURL: cb.srv.URL}) // non-nil, but IdentityURL empty
	if err := a2.ExportManaLedger(context.Background(), sampleAuth(), "", rec2); err != nil {
		t.Fatalf("unconfigured export: %v", err)
	}
	if rec2.Code != http.StatusBadGateway {
		t.Errorf("unconfigured status = %d; want 502", rec2.Code)
	}

	// 5xx → 502.
	cb5 := newCaptureBackend(t, http.StatusInternalServerError, "boom")
	rec3 := httptest.NewRecorder()
	a3 := newGBF(gatewayproxy.Config{IdentityURL: cb5.srv.URL})
	if err := a3.ExportManaLedger(context.Background(), sampleAuth(), "", rec3); err != nil {
		t.Fatalf("5xx export: %v", err)
	}
	if rec3.Code != http.StatusBadGateway {
		t.Errorf("5xx status = %d; want 502", rec3.Code)
	}

	// 4xx passes through with downstream body.
	cb4 := newCaptureBackend(t, http.StatusForbidden, `{"error":"forbidden"}`)
	rec4 := httptest.NewRecorder()
	a4 := newGBF(gatewayproxy.Config{IdentityURL: cb4.srv.URL})
	if err := a4.ExportManaLedger(context.Background(), sampleAuth(), "", rec4); err != nil {
		t.Fatalf("4xx export: %v", err)
	}
	if rec4.Code != http.StatusForbidden {
		t.Errorf("4xx status = %d; want 403 passthrough", rec4.Code)
	}

	// Transport error → 502.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	rec5 := httptest.NewRecorder()
	a5 := newGBF(gatewayproxy.Config{IdentityURL: deadURL})
	if err := a5.ExportManaLedger(context.Background(), sampleAuth(), "", rec5); err != nil {
		t.Fatalf("transport export: %v", err)
	}
	if rec5.Code != http.StatusBadGateway {
		t.Errorf("transport status = %d; want 502", rec5.Code)
	}
}

// ---------------------------------------------------------------------------
// classify() passthrough nuances
// ---------------------------------------------------------------------------

// TestGBF_Classify_501And503Passthrough — 501/503 are intentional contract
// codes and must NOT be normalised to 502.
func TestGBF_Classify_501And503Passthrough(t *testing.T) {
	for _, status := range []int{http.StatusNotImplemented, http.StatusServiceUnavailable} {
		cb := newCaptureBackend(t, status, `{"code":"X"}`)
		a := newGBF(gatewayproxy.Config{CreationURL: cb.srv.URL})
		resp, err := a.CreateAtom(context.Background(), sampleAuth(), nil)
		if err != nil {
			t.Fatalf("status %d: %v", status, err)
		}
		if resp.Status != status {
			t.Errorf("status %d passthrough = %d", status, resp.Status)
		}
	}
}

// TestGBF_Classify_CanceledParent — request cancel maps to
// GATEWAY_REQUEST_CANCELED 504.
func TestGBF_Classify_CanceledParent(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer slow.Close()

	a := newGBF(gatewayproxy.Config{CreationURL: slow.URL, PerCallTimeout: 2 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	resp, err := a.CreateAtom(ctx, sampleAuth(), nil)
	if err != nil {
		t.Fatalf("CreateAtom: %v", err)
	}
	if resp.Status != http.StatusGatewayTimeout {
		t.Errorf("cancel status = %d; want 504", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_REQUEST_CANCELED") {
		t.Errorf("body missing GATEWAY_REQUEST_CANCELED: %s", resp.Body)
	}
}
func TestGBF_ChangeAtomReuseVisibility(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"visibility":"public"}`)
	a := newGBF(gatewayproxy.Config{CreationURL: cb.srv.URL})

	resp, err := a.ChangeAtomReuseVisibility(context.Background(), sampleAuth(), "atom-1", []byte(`{"visibility":"public"}`))
	if err != nil {
		t.Fatalf("ChangeAtomReuseVisibility: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.method != http.MethodPatch || cb.path != "/api/atoms/atom-1/reuse-visibility" {
		t.Errorf("→ %s %s; want PATCH /api/atoms/atom-1/reuse-visibility", cb.method, cb.path)
	}

	// Empty atom id → 404 pre-outbound.
	resp, err = a.ChangeAtomReuseVisibility(context.Background(), sampleAuth(), "", nil)
	if err != nil {
		t.Fatalf("ChangeAtomReuseVisibility empty: %v", err)
	}
	if resp.Status != http.StatusNotFound {
		t.Errorf("empty id status = %d; want 404", resp.Status)
	}
}
