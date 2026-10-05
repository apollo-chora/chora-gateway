// v1_topics_test.go — RED-phase TDD specs for CHO-2276 (topic-tree Sub-phase B).
//
// The A+/R+ topic-tree UI drives the content topic taxonomy through the public
// gateway on the /api/v1/topics prefix; chora-creation (Sub-phase A, CHO-2275)
// serves the tree at /api/topics — NO /v1/. That downstream prefix is the one
// the live mesh AuthorizationPolicy creation/allow-from-gateway allowlists (the
// /api/topics + /api/topics/* entries this sub-phase adds), so the aggregator
// TRANSLATES the gateway-facing /api/v1/topics{suffix} onto /api/topics{suffix}
// and forwards method + body + query verbatim.
//
// Reads (GET) are learner-safe (JWT + RLS, like atoms CHO-2261). Writes
// (POST / PUT / DELETE) are admin-gated: the gateway stamps x-mesh-user-roles
// from the validated JWT (call() → MarshalToHeaders) and forwards it; the
// downstream chora-creation topic handler enforces admin/owner (requireAdmin,
// defence-in-depth) and its 403 passes through verbatim.
//
// Strict TDD: these tests are written BEFORE Aggregator.ProxyTopicTree exists.
package gatewayproxy_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

// adminAuth is sampleAuth() carrying an admin role so the write specs can assert
// the x-mesh-user-roles propagation the downstream admin gate reads.
func adminAuth() gatewayproxy.AuthCtx {
	a := sampleAuth()
	a.Roles = []string{"admin"}
	return a
}

// -----------------------------------------------------------------------------
// Reads — GET /api/v1/topics[/{id}] → chora-creation /api/topics[/{id}]
// -----------------------------------------------------------------------------

// The KEY assertion is the path TRANSLATION (v1 stripped) so the request lands
// on the already-allowlisted downstream path, plus verbatim ?parent_id forward.
func TestProxyTopicTree_CollectionGET_TranslatesV1_ForwardsQuery(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK,
		`{"data":[{"id":"019f6baf-0a31-708e-8923-954f39602102","parent_id":null,"name":"Cycling","sort_order":0}]}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.ProxyTopicTree(context.Background(), sampleAuth(), http.MethodGet, "", "", "parent_id=019f6baf-0a31-708e-8923-954f39602103", nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/topics" {
		t.Errorf("downstream path = %q; want /api/topics (v1 stripped)", cb.path)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
	if cb.rawQ != "parent_id=019f6baf-0a31-708e-8923-954f39602103" {
		t.Errorf("downstream query = %q; want parent_id=... forwarded verbatim", cb.rawQ)
	}
	if !strings.Contains(string(resp.Body), `"Cycling"`) {
		t.Errorf("body = %q; want {data:[...]} forwarded verbatim", string(resp.Body))
	}
	// Mesh-trust headers — chora-creation topic list reads tenantFromContext (RLS).
	if cb.hdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
		t.Errorf("X-Tenant-Id = %q; want %q", cb.hdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("lowercase gcid = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
}

func TestProxyTopicTree_ItemGET_TranslatesV1(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK,
		`{"id":"019f6baf-0a31-708e-8923-954f39602102","parent_id":null,"name":"Cycling","sort_order":0}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	_, _ = a.ProxyTopicTree(context.Background(), sampleAuth(), http.MethodGet,
		"019f6baf-0a31-708e-8923-954f39602102", "", "", nil)
	if cb.path != "/api/topics/019f6baf-0a31-708e-8923-954f39602102" {
		t.Errorf("downstream path = %q; want /api/topics/{id} (v1 stripped)", cb.path)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
}

// -----------------------------------------------------------------------------
// Writes — admin-gated. Body forwarded verbatim; x-mesh-user-roles propagated.
// -----------------------------------------------------------------------------

func TestProxyTopicTree_CreatePOST_BodyVerbatim_RolesForwarded(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusCreated,
		`{"id":"019f6baf-0a31-708e-8923-954f39602199","parent_id":null,"name":"New topic","sort_order":0}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"name":"New topic","parent_id":null,"sort_order":0}`)
	resp, _ := a.ProxyTopicTree(context.Background(), adminAuth(), http.MethodPost, "", "", "", body)

	if resp.Status != http.StatusCreated {
		t.Errorf("status = %d; want 201", resp.Status)
	}
	if cb.path != "/api/topics" {
		t.Errorf("downstream path = %q; want /api/topics", cb.path)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %s; want POST", cb.method)
	}
	if cb.body != `{"name":"New topic","parent_id":null,"sort_order":0}` {
		t.Errorf("downstream body = %q; want the create body verbatim", cb.body)
	}
	// The authoritative admin signal — call() stamps this from the validated JWT.
	if !strings.Contains(cb.hdr.Get("x-mesh-user-roles"), "admin") {
		t.Errorf("x-mesh-user-roles = %q; want it to carry admin (downstream admin gate)", cb.hdr.Get("x-mesh-user-roles"))
	}
}

func TestProxyTopicTree_MovePOST_TranslatesV1_BodyVerbatim(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK,
		`{"id":"019f6baf-0a31-708e-8923-954f39602102","parent_id":"019f6baf-0a31-708e-8923-954f39602103","name":"Cycling","sort_order":0}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"parent_id":"019f6baf-0a31-708e-8923-954f39602103"}`)
	resp, _ := a.ProxyTopicTree(context.Background(), adminAuth(), http.MethodPost,
		"019f6baf-0a31-708e-8923-954f39602102", "move", "", body)
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/api/topics/019f6baf-0a31-708e-8923-954f39602102/move" {
		t.Errorf("downstream path = %q; want /api/topics/{id}/move", cb.path)
	}
	if cb.body != `{"parent_id":"019f6baf-0a31-708e-8923-954f39602103"}` {
		t.Errorf("downstream body = %q; want the move body verbatim", cb.body)
	}
}

func TestProxyTopicTree_AttachPOST_TranslatesV1(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"status":"attached"}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"atom_id":"019f6baf-0a31-708e-8923-954f39602104"}`)
	_, _ = a.ProxyTopicTree(context.Background(), adminAuth(), http.MethodPost,
		"019f6baf-0a31-708e-8923-954f39602102", "atoms", "", body)
	if cb.path != "/api/topics/019f6baf-0a31-708e-8923-954f39602102/atoms" {
		t.Errorf("downstream path = %q; want /api/topics/{id}/atoms", cb.path)
	}
	if cb.method != http.MethodPost {
		t.Errorf("method = %s; want POST", cb.method)
	}
}

func TestProxyTopicTree_PUT_TranslatesV1_BodyVerbatim(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK,
		`{"id":"019f6baf-0a31-708e-8923-954f39602102","parent_id":null,"name":"Renamed","sort_order":2}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	body := []byte(`{"name":"Renamed","sort_order":2}`)
	_, _ = a.ProxyTopicTree(context.Background(), adminAuth(), http.MethodPut,
		"019f6baf-0a31-708e-8923-954f39602102", "", "", body)
	if cb.path != "/api/topics/019f6baf-0a31-708e-8923-954f39602102" {
		t.Errorf("downstream path = %q; want /api/topics/{id}", cb.path)
	}
	if cb.method != http.MethodPut {
		t.Errorf("method = %s; want PUT", cb.method)
	}
	if cb.body != `{"name":"Renamed","sort_order":2}` {
		t.Errorf("downstream body = %q; want the update body verbatim", cb.body)
	}
}

func TestProxyTopicTree_DELETE_TranslatesV1(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusNoContent, ``)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.ProxyTopicTree(context.Background(), adminAuth(), http.MethodDelete,
		"019f6baf-0a31-708e-8923-954f39602102", "", "", nil)
	if resp.Status != http.StatusNoContent {
		t.Errorf("status = %d; want 204 verbatim", resp.Status)
	}
	if cb.path != "/api/topics/019f6baf-0a31-708e-8923-954f39602102" {
		t.Errorf("downstream path = %q; want /api/topics/{id}", cb.path)
	}
	if cb.method != http.MethodDelete {
		t.Errorf("method = %s; want DELETE", cb.method)
	}
}

// -----------------------------------------------------------------------------
// Status passthrough — the downstream verdict is authoritative.
// -----------------------------------------------------------------------------

// A non-admin write is refused DOWNSTREAM (chora-creation requireAdmin →
// 403 CREATION_TOPIC_ADMIN_REQUIRED). The gateway forwards it verbatim — it
// must NOT rewrite a 4xx into a 5xx (that would mask the admin gate).
func TestProxyTopicTree_Upstream403_PassThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusForbidden, `{"error":{"code":"CREATION_TOPIC_ADMIN_REQUIRED"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.ProxyTopicTree(context.Background(), sampleAuth(), http.MethodPost, "", "", "", []byte(`{"name":"X"}`))
	if resp.Status != http.StatusForbidden {
		t.Errorf("status = %d; want 403 verbatim from the downstream admin gate", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "CREATION_TOPIC_ADMIN_REQUIRED") {
		t.Errorf("body = %q; want the downstream admin-gate code verbatim", string(resp.Body))
	}
}

func TestProxyTopicTree_Upstream5xx_502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `boom`)
	a := gatewayproxy.New(gatewayproxy.Config{CreationURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, _ := a.ProxyTopicTree(context.Background(), sampleAuth(), http.MethodGet, "", "", "", nil)
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 on upstream 5xx", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_UPSTREAM_5XX") {
		t.Errorf("body = %q; want GATEWAY_UPSTREAM_5XX code in the 502 envelope", string(resp.Body))
	}
}
