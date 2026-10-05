// transcript_test.go — W6 StudentTranscript read-model: gateway proxy tests
// for the two new BFF routes. Mirrors dose_preferences_test.go / lane_d_test.go
// conventions.
//
//	GET /api/v1/me/transcript[?limit=]                       → chora-consumption
//	                                                            GET /v1/me/transcript
//	GET /api/v1/transcript/by-assessments?assessment_ids=... → chora-consumption
//	                                                            GET /v1/transcript/by-assessments
//
// Both are GET-only, path-translated the same way MeLearningPaths is — the
// downstream serves the bare /v1/... form (no /api prefix to strip; the
// gateway builds the downstream URL directly rather than trimming the
// inbound path). The by-assessments leaf is role-gated DOWNSTREAM via
// x-mesh-user-roles (instructor / admin / training-admin / tenant_admin);
// call() already stamps that header from auth.Roles — the same mesh-trust
// propagation ProxyMeAssessments relies on — so the gateway needs no bespoke
// role check of its own; a caller lacking the role gets a verbatim 403
// passed through from chora-consumption.
package gatewayproxy_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

// -----------------------------------------------------------------------------
// MeTranscript — GET /api/v1/me/transcript
// -----------------------------------------------------------------------------

func TestMeTranscript_GET_ProxiesToConsumption(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[{"entry_id":"e-1"}]}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.MeTranscript(context.Background(), sampleAuth(), "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/v1/me/transcript" {
		t.Errorf("downstream path = %q; want /v1/me/transcript", cb.path)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %s; want GET", cb.method)
	}
	// chora-consumption's extRequireContext reads X-Tenant-Id + lowercase gcid.
	if cb.hdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
		t.Errorf("X-Tenant-Id = %q; want %q", cb.hdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
	}
	if cb.hdr.Get("gcid") != sampleAuth().GCID {
		t.Errorf("gcid = %q; want %q", cb.hdr.Get("gcid"), sampleAuth().GCID)
	}
}

func TestMeTranscript_ForwardsLimitQuery(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[]}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	_, _ = a.MeTranscript(context.Background(), sampleAuth(), "limit=10")
	if cb.rawQ != "limit=10" {
		t.Errorf("downstream rawQuery = %q; want limit=10", cb.rawQ)
	}
}

func TestMeTranscript_Upstream5xx_502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `{"error":"boom"}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.MeTranscript(context.Background(), sampleAuth(), "")
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 on upstream 5xx", resp.Status)
	}
}

// TRANSCRIPT_NOT_WIRED — the downstream's fail-loud 503 when the pg pool
// isn't wired (per transcript_handler.go). Must pass through verbatim (not
// renormalised to 502) — mirrors classify()'s intentional-contract-code
// carve-out for 501/503 (e.g. CREATION_JOBS_NOT_WIRED).
func TestMeTranscript_Upstream503_PassThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusServiceUnavailable, `{"code":"TRANSCRIPT_NOT_WIRED"}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.MeTranscript(context.Background(), sampleAuth(), "")
	if resp.Status != http.StatusServiceUnavailable {
		t.Errorf("status = %d; want 503 passthrough", resp.Status)
	}
}

// -----------------------------------------------------------------------------
// TranscriptByAssessments — GET /api/v1/transcript/by-assessments
// -----------------------------------------------------------------------------

func TestTranscriptByAssessments_GET_ProxiesToConsumption(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[]}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	resp, err := a.TranscriptByAssessments(context.Background(), sampleAuth(), "assessment_ids=a-1,a-2")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	if cb.path != "/v1/transcript/by-assessments" {
		t.Errorf("downstream path = %q; want /v1/transcript/by-assessments", cb.path)
	}
	if cb.rawQ != "assessment_ids=a-1,a-2" {
		t.Errorf("downstream rawQuery = %q; want assessment_ids=a-1,a-2", cb.rawQ)
	}
	if cb.hdr.Get("X-Tenant-Id") != sampleAuth().TenantID {
		t.Errorf("X-Tenant-Id = %q; want %q", cb.hdr.Get("X-Tenant-Id"), sampleAuth().TenantID)
	}
}

// The downstream role gate (hasTranscriptAdminRole in transcript_handler.go)
// reads x-mesh-user-roles; call() stamps it from auth.Roles the same way
// ProxyMeAssessments's mesh-trust propagation does. Locks that the gateway
// keeps doing so for the new route — without it every caller (even a real
// instructor) would 403.
func TestTranscriptByAssessments_StampsMeshUserRoles(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"items":[]}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	auth := sampleAuth()
	auth.Roles = []string{"instructor"}
	_, _ = a.TranscriptByAssessments(context.Background(), auth, "assessment_ids=a-1")
	if got := cb.hdr.Get("x-mesh-user-roles"); got != "instructor" {
		t.Errorf("x-mesh-user-roles = %q; want instructor", got)
	}
}

// Caller lacks instructor/admin/training-admin/tenant_admin role — chora-
// consumption's fail-closed role gate 403s; the gateway must pass it through
// verbatim, never swallow or renormalise it.
func TestTranscriptByAssessments_Upstream403_PassThrough(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusForbidden,
		`{"code":"FORBIDDEN","message":"caller lacks instructor/admin/training-admin role"}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.TranscriptByAssessments(context.Background(), sampleAuth(), "assessment_ids=a-1")
	if resp.Status != http.StatusForbidden {
		t.Errorf("status = %d; want 403 passthrough", resp.Status)
	}
}

func TestTranscriptByAssessments_Upstream5xx_502(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusInternalServerError, `{"error":"boom"}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})
	resp, _ := a.TranscriptByAssessments(context.Background(), sampleAuth(), "assessment_ids=a-1")
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 on upstream 5xx", resp.Status)
	}
}
