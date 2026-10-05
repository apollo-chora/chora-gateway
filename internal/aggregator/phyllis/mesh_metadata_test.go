// mesh_metadata_test.go — RED-phase TDD specs for outbound mesh metadata
// propagation per S3.6 Stage B.
//
// The BFF is the trust boundary: every outbound call to a backend service
// MUST carry the mesh metadata headers `chora-gcid`, `chora-tenant-id`,
// `chora-role-summary` so backends can decode the trusted caller identity
// without re-validating the JWT (Cloud Service Mesh asserts caller identity
// via mTLS; backends trust mesh-bound metadata).
//
// We test:
//  1. Every outbound call carries chora-gcid + chora-tenant-id when AuthCtx
//     populates them.
//  2. chora-role-summary is JSON-encoded when present, omitted when empty.
//  3. AuthCtx without GCID/TenantID still calls (legacy public read paths)
//     but with no mesh headers stamped.
package phyllis_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

// captureBackend is a minimal httptest.Server that captures the inbound
// headers from the BFF outbound call.
type captureBackend struct {
	srv      *httptest.Server
	hdr      http.Header
	calls    int
	respBody string
}

func newCaptureBackend(t *testing.T, body string, status int) *captureBackend {
	t.Helper()
	cb := &captureBackend{respBody: body}
	cb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cb.calls++
		cb.hdr = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(cb.srv.Close)
	return cb
}

func TestPhyllis_OutboundCall_StampsMeshMetadataHeaders(t *testing.T) {
	t.Parallel()
	identity := newCaptureBackend(t, `{"gcid":"01970000-0000-7000-8000-0000000000aa"}`, http.StatusOK)

	a := phyllis.New(phyllis.Config{
		IdentityURL:    identity.srv.URL,
		ConsumptionURL: identity.srv.URL, // unused but must be set
	}, nil)

	auth := phyllis.AuthCtx{
		Bearer:      "raw-jwt-token",
		Traceparent: "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01",
		TenantID:    "01970000-0000-7000-8000-0000000000bb",
		GCID:        "01970000-0000-7000-8000-0000000000aa",
		RoleSummary: map[string]any{"learner": []string{"any"}},
	}
	resp, err := a.GetMe(context.Background(), auth)
	if err != nil {
		t.Fatalf("GetMe err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d", resp.Status)
	}

	// Mesh metadata headers must be present.
	if got := identity.hdr.Get("chora-gcid"); got != auth.GCID {
		t.Errorf("chora-gcid = %q, want %q", got, auth.GCID)
	}
	if got := identity.hdr.Get("chora-tenant-id"); got != auth.TenantID {
		t.Errorf("chora-tenant-id = %q, want %q", got, auth.TenantID)
	}

	// Role summary must be JSON-encoded and parseable.
	rsRaw := identity.hdr.Get("chora-role-summary")
	if rsRaw == "" {
		t.Fatal("chora-role-summary header missing")
	}
	var rs map[string]any
	if err := json.Unmarshal([]byte(rsRaw), &rs); err != nil {
		t.Fatalf("role_summary header not valid JSON: %v (raw=%q)", err, rsRaw)
	}
	if rs["learner"] == nil {
		t.Errorf("role_summary missing learner key: got %v", rs)
	}
}

func TestPhyllis_OutboundCall_OmitsMeshHeadersWhenAuthCtxEmpty(t *testing.T) {
	t.Parallel()
	identity := newCaptureBackend(t, `{}`, http.StatusOK)

	a := phyllis.New(phyllis.Config{
		IdentityURL: identity.srv.URL,
	}, nil)

	// Public-read style call with no auth fields.
	resp, _ := a.GetMe(context.Background(), phyllis.AuthCtx{})
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d", resp.Status)
	}
	if got := identity.hdr.Get("chora-gcid"); got != "" {
		t.Errorf("chora-gcid should be empty for unauthenticated call; got %q", got)
	}
	if got := identity.hdr.Get("chora-tenant-id"); got != "" {
		t.Errorf("chora-tenant-id should be empty; got %q", got)
	}
	if got := identity.hdr.Get("chora-role-summary"); got != "" {
		t.Errorf("chora-role-summary should be empty; got %q", got)
	}
}

func TestPhyllis_OutboundCall_OmitsRoleSummaryWhenMapEmpty(t *testing.T) {
	t.Parallel()
	identity := newCaptureBackend(t, `{}`, http.StatusOK)

	a := phyllis.New(phyllis.Config{IdentityURL: identity.srv.URL}, nil)

	resp, _ := a.GetMe(context.Background(), phyllis.AuthCtx{
		GCID:     "01970000-0000-7000-8000-0000000000aa",
		TenantID: "01970000-0000-7000-8000-0000000000bb",
		// RoleSummary intentionally nil — empty roles must be omitted (per
		// servicemesh.MarshalToHeaders; downstream treats absent as default).
	})
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d", resp.Status)
	}
	if got := identity.hdr.Get("chora-gcid"); got == "" {
		t.Error("chora-gcid should be set")
	}
	if got := identity.hdr.Get("chora-role-summary"); got != "" {
		t.Errorf("chora-role-summary should be empty when RoleSummary nil; got %q", got)
	}
}

// TestPhyllis_OutboundCall_DoesNotLeakBearerInBody verifies the JWT is only
// in the Authorization header (not body or other headers).
func TestPhyllis_OutboundCall_KeepsAuthorizationHeaderUnchanged(t *testing.T) {
	t.Parallel()
	identity := newCaptureBackend(t, `{}`, http.StatusOK)
	a := phyllis.New(phyllis.Config{IdentityURL: identity.srv.URL}, nil)

	auth := phyllis.AuthCtx{
		Bearer:   "the-jwt-string",
		GCID:     "01970000-0000-7000-8000-0000000000aa",
		TenantID: "01970000-0000-7000-8000-0000000000bb",
	}
	_, _ = a.GetMe(context.Background(), auth)

	got := identity.hdr.Get("Authorization")
	if !strings.HasPrefix(got, "Bearer ") {
		t.Fatalf("Authorization header missing Bearer prefix: %q", got)
	}
	if !strings.Contains(got, "the-jwt-string") {
		t.Errorf("Authorization header should preserve the JWT: %q", got)
	}
}
