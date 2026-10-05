// reorder_clone_test.go — RED-phase TDD specs for the Wave 2 BFF proxy
// aggregator methods: QuestionBank question reorder + Atom clone. Both are pure
// verbatim proxies to chora-creation at the SAME path the FE hits, mirroring the
// W3.B.1 QuestionBank (AssembleQuestionBankTestSet) + P7 atom-question
// (CreateQuestion/EditQuestion) passthroughs. Reuses the captureBackend +
// sampleAuth helpers in gatewayproxy_test.go.
//
//	POST /api/v1/question-banks/{id}/reorder   ReorderQuestionBank
//	POST /api/atoms/{atom_id}/clone            CloneAtom
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
// POST /api/v1/question-banks/{id}/reorder → chora-creation (verbatim proxy)
// -----------------------------------------------------------------------------

func TestReorderQuestionBank_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"id":"qb-1"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"question_ids":["q-2","q-1","q-3"]}`)
	resp, err := a.ReorderQuestionBank(context.Background(), sampleAuth(), "qb-1", body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/v1/question-banks/qb-1/reorder" {
		t.Errorf("downstream path = %q; want /api/v1/question-banks/qb-1/reorder", cb.path)
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

func TestReorderQuestionBank_EmptyID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.ReorderQuestionBank(context.Background(), sampleAuth(), "", []byte(`{}`))
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty question bank id", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_QUESTION_BANK_ID_REQUIRED") {
		t.Errorf("body = %q; want GATEWAY_QUESTION_BANK_ID_REQUIRED code", string(resp.Body))
	}
}

func TestReorderQuestionBank_422_PassesThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusUnprocessableEntity, `{"error":{"code":"CREATION_QUESTION_BANK_INVALID"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.ReorderQuestionBank(context.Background(), sampleAuth(), "qb-1", []byte(`{"question_ids":[]}`))
	if resp.Status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d; want 422 verbatim from upstream", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// POST /api/atoms/{atom_id}/clone → chora-creation (verbatim proxy)
// -----------------------------------------------------------------------------

func TestCloneAtom_ProxiesToCreation(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusCreated, `{"atom_id":"atom-2","cloned_from":"atom-1"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"title":"Copy of My Atom"}`)
	resp, err := a.CloneAtom(context.Background(), sampleAuth(), "atom-1", body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.Status)
	}
	if cb.path != "/api/atoms/atom-1/clone" {
		t.Errorf("downstream path = %q; want /api/atoms/atom-1/clone", cb.path)
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

func TestCloneAtom_EmptyID_404(t *testing.T) {
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: "http://unused", PerCallTimeout: time.Second})
	resp, _ := a.CloneAtom(context.Background(), sampleAuth(), "", []byte(`{}`))
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 on empty atom id", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_ATOM_ID_REQUIRED") {
		t.Errorf("body = %q; want GATEWAY_ATOM_ID_REQUIRED code", string(resp.Body))
	}
}

func TestCloneAtom_Upstream404_PassThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusNotFound, `{"error":{"code":"CREATION_ATOM_NOT_FOUND"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.CloneAtom(context.Background(), sampleAuth(), "ghost", []byte(`{}`))
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 verbatim from upstream", resp.Status)
	}
}

func TestCloneAtom_Upstream5xx_502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `boom`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.CloneAtom(context.Background(), sampleAuth(), "atom-1", []byte(`{}`))
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 on upstream 5xx", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_UPSTREAM_5XX") {
		t.Errorf("body = %q; want GATEWAY_UPSTREAM_5XX code in 502 envelope", string(resp.Body))
	}
}
