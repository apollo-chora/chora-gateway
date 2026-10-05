// questionbank_test.go — RED-phase TDD specs for the QuestionBank BFF proxy
// aggregator methods (W3.B.1). These mirror the CHO-1618 Personal Collections
// proxy tests EXACTLY (same captureBackend + sampleAuth helpers in
// gatewayproxy_test.go): chora-creation serves the QuestionBank route table at
// the SAME path the FE hits, so each method is a pure verbatim proxy —
// method + path + body forwarded unchanged, mesh-trust headers stamped, empty
// path ids short-circuit 404 BEFORE any outbound call.
//
//	POST   /api/v1/question-banks                            CreateQuestionBank
//	GET    /api/v1/me/question-banks                         ListMyQuestionBanks
//	GET    /api/v1/question-banks/{id}                       GetQuestionBank
//	PATCH  /api/v1/question-banks/{id}                       PatchQuestionBank
//	DELETE /api/v1/question-banks/{id}                       DeleteQuestionBank
//	POST   /api/v1/question-banks/{id}/questions             AddQuestionBankQuestion
//	GET    /api/v1/question-banks/{id}/questions             ListQuestionBankQuestions
//	DELETE /api/v1/question-banks/{id}/questions/{qid}       RemoveQuestionBankQuestion
//	POST   /api/v1/question-banks/{id}/assemble-test-set     AssembleQuestionBankTestSet
//
// Strict TDD: tests written BEFORE the aggregator methods.
package gatewayproxy_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

// -----------------------------------------------------------------------------
// POST /api/v1/question-banks → chora-creation (same path, verbatim proxy)
// -----------------------------------------------------------------------------

func TestCreateQuestionBank_POST_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusCreated, `{"id":"qb-1","name":"My Bank"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"name":"My Bank","visibility":"PRIVATE"}`)
	resp, err := a.CreateQuestionBank(context.Background(), sampleAuth(), body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.Status)
	}
	if cb.path != "/api/v1/question-banks" {
		t.Errorf("downstream path = %q; want /api/v1/question-banks", cb.path)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %s; want POST", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("body forwarded = %q; want verbatim", cb.body)
	}
	// Mesh-trust headers — chora-creation tenantContext middleware reads these.
	if cb.hdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
		t.Errorf("X-Tenant-Id = %q; want %q", cb.hdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid header = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
}

func TestCreateQuestionBank_400_PassesThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusBadRequest, `{"error":{"code":"CREATION_QUESTION_BANK_INVALID"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.CreateQuestionBank(context.Background(), sampleAuth(), []byte(`{}`))
	if resp.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 verbatim from upstream", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// GET /api/v1/me/question-banks → chora-creation (caller-scoped list)
// -----------------------------------------------------------------------------

func TestListMyQuestionBanks_GET_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[],"total":0}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.ListMyQuestionBanks(context.Background(), sampleAuth())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/v1/me/question-banks" {
		t.Errorf("downstream path = %q; want /api/v1/me/question-banks", cb.path)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
}

// -----------------------------------------------------------------------------
// GET /api/v1/question-banks/{id}
// -----------------------------------------------------------------------------

func TestGetQuestionBank_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"id":"qb-1"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.GetQuestionBank(context.Background(), sampleAuth(), "qb-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/v1/question-banks/qb-1" {
		t.Errorf("downstream path = %q; want /api/v1/question-banks/qb-1", cb.path)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
}

func TestGetQuestionBank_EmptyID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.GetQuestionBank(context.Background(), sampleAuth(), "")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty question bank id", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_QUESTION_BANK_ID_REQUIRED") {
		t.Errorf("body = %q; want GATEWAY_QUESTION_BANK_ID_REQUIRED code", string(resp.Body))
	}
}

func TestGetQuestionBank_Upstream404_PassThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusNotFound, `{"error":{"code":"CREATION_QUESTION_BANK_NOT_FOUND"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.GetQuestionBank(context.Background(), sampleAuth(), "ghost")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 verbatim from upstream", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// PATCH /api/v1/question-banks/{id}
// -----------------------------------------------------------------------------

func TestPatchQuestionBank_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"id":"qb-1","name":"Renamed"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"name":"Renamed"}`)
	resp, err := a.PatchQuestionBank(context.Background(), sampleAuth(), "qb-1", body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/v1/question-banks/qb-1" {
		t.Errorf("downstream path = %q; want /api/v1/question-banks/qb-1", cb.path)
	}
	if cb.method != http.MethodPatch {
		t.Errorf("method = %s; want PATCH", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("body forwarded = %q; want verbatim", cb.body)
	}
}

func TestPatchQuestionBank_EmptyID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.PatchQuestionBank(context.Background(), sampleAuth(), "", []byte(`{}`))
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty question bank id", resp.Status)
	}
}

func TestPatchQuestionBank_Forbidden_PassThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusForbidden, `{"error":{"code":"CREATION_QUESTION_BANK_FORBIDDEN"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.PatchQuestionBank(context.Background(), sampleAuth(), "qb-1", []byte(`{"name":"x"}`))
	if resp.Status != http.StatusForbidden {
		t.Errorf("status = %d; want 403 verbatim from upstream", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// DELETE /api/v1/question-banks/{id}
// -----------------------------------------------------------------------------

func TestDeleteQuestionBank_204_NoBody(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusNoContent, ``)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.DeleteQuestionBank(context.Background(), sampleAuth(), "qb-1")
	if resp.Status != http.StatusNoContent {
		t.Errorf("status = %d; want 204", resp.Status)
	}
	if len(resp.Body) != 0 {
		t.Errorf("body length = %d; want empty body on 204 passthrough", len(resp.Body))
	}
	if cb.path != "/api/v1/question-banks/qb-1" {
		t.Errorf("downstream path = %q; want /api/v1/question-banks/qb-1", cb.path)
	}
	if cb.method != http.MethodDelete {
		t.Errorf("method = %s; want DELETE", cb.method)
	}
}

func TestDeleteQuestionBank_EmptyID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.DeleteQuestionBank(context.Background(), sampleAuth(), "")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty question bank id", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// POST /api/v1/question-banks/{id}/questions
// -----------------------------------------------------------------------------

func TestAddQuestionBankQuestion_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusCreated, `{"id":"qb-1","items":[{"question_id":"q-9"}]}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"question_id":"q-9"}`)
	resp, err := a.AddQuestionBankQuestion(context.Background(), sampleAuth(), "qb-1", body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.Status)
	}
	if cb.path != "/api/v1/question-banks/qb-1/questions" {
		t.Errorf("downstream path = %q; want /api/v1/question-banks/qb-1/questions", cb.path)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %s; want POST", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("body forwarded = %q; want verbatim", cb.body)
	}
}

func TestAddQuestionBankQuestion_EmptyID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.AddQuestionBankQuestion(context.Background(), sampleAuth(), "", []byte(`{"question_id":"x"}`))
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty question bank id", resp.Status)
	}
}

func TestAddQuestionBankQuestion_409_PassesThrough_DuplicateQuestion(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusConflict, `{"error":{"code":"CREATION_QUESTION_BANK_DUPLICATE_QUESTION"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.AddQuestionBankQuestion(context.Background(), sampleAuth(), "qb-1", []byte(`{"question_id":"dup"}`))
	if resp.Status != http.StatusConflict {
		t.Errorf("status = %d; want 409 verbatim from upstream", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// GET /api/v1/question-banks/{id}/questions
// -----------------------------------------------------------------------------

func TestListQuestionBankQuestions_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[],"total":0}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	// Server-side filter/sort/page params MUST reach chora-creation verbatim —
	// the workbench list relies on them (CHO-1926). Dropping the query string
	// makes the bank list ignore search/type/sort/pagination.
	resp, err := a.ListQuestionBankQuestions(context.Background(), sampleAuth(), "qb-1",
		"q=Scrum&question_type=mcq&sort=prompt:asc&page=2&page_size=20")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/v1/question-banks/qb-1/questions" {
		t.Errorf("downstream path = %q; want /api/v1/question-banks/qb-1/questions", cb.path)
	}
	if cb.rawQ != "q=Scrum&question_type=mcq&sort=prompt:asc&page=2&page_size=20" {
		t.Errorf("downstream rawQuery = %q; want the filter/sort/page params forwarded verbatim", cb.rawQ)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
}

func TestListQuestionBankQuestions_EmptyID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.ListQuestionBankQuestions(context.Background(), sampleAuth(), "", "")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty question bank id", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// DELETE /api/v1/question-banks/{id}/questions/{qid}
// -----------------------------------------------------------------------------

func TestRemoveQuestionBankQuestion_204_NoBody(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusNoContent, ``)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.RemoveQuestionBankQuestion(context.Background(), sampleAuth(), "qb-1", "q-9")
	if resp.Status != http.StatusNoContent {
		t.Errorf("status = %d; want 204", resp.Status)
	}
	if len(resp.Body) != 0 {
		t.Errorf("body length = %d; want empty body on 204 passthrough", len(resp.Body))
	}
	if cb.path != "/api/v1/question-banks/qb-1/questions/q-9" {
		t.Errorf("downstream path = %q; want /api/v1/question-banks/qb-1/questions/q-9", cb.path)
	}
	if cb.method != http.MethodDelete {
		t.Errorf("method = %s; want DELETE", cb.method)
	}
}

func TestRemoveQuestionBankQuestion_EmptyBankID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.RemoveQuestionBankQuestion(context.Background(), sampleAuth(), "", "q-9")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty question bank id", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_QUESTION_BANK_ID_REQUIRED") {
		t.Errorf("body = %q; want GATEWAY_QUESTION_BANK_ID_REQUIRED code", string(resp.Body))
	}
}

func TestRemoveQuestionBankQuestion_EmptyQuestionID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.RemoveQuestionBankQuestion(context.Background(), sampleAuth(), "qb-1", "")
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty question id", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_QUESTION_ID_REQUIRED") {
		t.Errorf("body = %q; want GATEWAY_QUESTION_ID_REQUIRED code", string(resp.Body))
	}
}

// -----------------------------------------------------------------------------
// POST /api/v1/question-banks/{id}/assemble-test-set
// -----------------------------------------------------------------------------

func TestAssembleQuestionBankTestSet_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusAccepted, `{"job_id":"job-1","test_set_status":"assembling"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"title":"Midterm"}`)
	resp, err := a.AssembleQuestionBankTestSet(context.Background(), sampleAuth(), "qb-1", body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusAccepted {
		t.Errorf("status = %d; want 202", resp.Status)
	}
	if cb.path != "/api/v1/question-banks/qb-1/assemble-test-set" {
		t.Errorf("downstream path = %q; want /api/v1/question-banks/qb-1/assemble-test-set", cb.path)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %s; want POST", cb.method)
	}
	if cb.body != string(body) {
		t.Errorf("body forwarded = %q; want verbatim", cb.body)
	}
}

func TestAssembleQuestionBankTestSet_EmptyID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.AssembleQuestionBankTestSet(context.Background(), sampleAuth(), "", []byte(`{"title":"x"}`))
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty question bank id", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// Upstream 5xx normalisation (shared classify() path)
// -----------------------------------------------------------------------------

func TestQuestionBanks_Upstream5xx_502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `{"error":"boom"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.ListMyQuestionBanks(context.Background(), sampleAuth())
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 on upstream 5xx", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_UPSTREAM_5XX") {
		t.Errorf("body = %q; want GATEWAY_UPSTREAM_5XX code in 502 envelope", string(resp.Body))
	}
}
