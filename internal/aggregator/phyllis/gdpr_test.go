// PE-14 — GDPR consent center BFF aggregator tests.
//
// Wraps three downstream endpoints into the A+ surface BFF:
//
//	GET  /api/me/consents          → chora-identity:/me/consents (list + status)
//	POST /api/me/consents/grant    → chora-identity:/me/consents (per-purpose toggle)
//	POST /api/me/data-export       → chora-identity:/me/portability/export (Art.15)
//
// GDPR Art. 15/20 + IMDA D4 transparency. Account closure (Art. 17) moved to
// the canonical closure saga me-route POST /api/v1/me/account/close (CHO-1719);
// the legacy TriggerAccountClosure → orchestrator:/sagas wrapper was dead and
// is removed (CHO-1790 D12).
package phyllis_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

// -----------------------------------------------------------------------------
// GetConsents — fan-out to chora-identity:/me/consents
// -----------------------------------------------------------------------------

func TestGetConsents_HappyPath(t *testing.T) {
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/consents" {
			t.Errorf("path = %s; want /me/consents", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %s; want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"gcid":"phyllis-gcid","consents":[{"consent_type":"companion_memory_consent","granted":true,"version":"2026-05-07"}]}`))
	})

	cfg := phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: 1 * time.Second}
	a := phyllis.New(cfg, nil)

	res, err := a.GetConsents(context.Background(), phyllis.AuthCtx{Bearer: "phyllis-gcid", Traceparent: "00-aaa-bbb-01"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", res.Status)
	}
	if !strings.Contains(string(res.Body), "companion_memory_consent") {
		t.Errorf("body did not pass-through consents: %s", string(res.Body))
	}
	if identity.lastAuth != "Bearer phyllis-gcid" {
		t.Errorf("auth = %q; want pass-through", identity.lastAuth)
	}
	if identity.lastTP != "00-aaa-bbb-01" {
		t.Errorf("traceparent = %q; want pass-through", identity.lastTP)
	}
}

func TestGetConsents_Unauthorized(t *testing.T) {
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHORIZED"}}`))
	})

	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GetConsents(context.Background(), phyllis.AuthCtx{Bearer: "bad"})
	if res.Status != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", res.Status)
	}
}

func TestGetConsents_UpstreamTimeout(t *testing.T) {
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	})

	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: 50 * time.Millisecond}, nil)
	res, _ := a.GetConsents(context.Background(), phyllis.AuthCtx{Bearer: "x"})
	if res.Status != http.StatusGatewayTimeout {
		t.Errorf("status = %d; want 504", res.Status)
	}
}

// -----------------------------------------------------------------------------
// GrantConsent — POST chora-identity:/me/consents
// -----------------------------------------------------------------------------

func TestGrantConsent_HappyPath(t *testing.T) {
	var lastBody string
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/consents" {
			t.Errorf("path = %s; want /me/consents", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s; want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %s; want application/json", ct)
		}
		buf := make([]byte, 256)
		n, _ := r.Body.Read(buf)
		lastBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"consent_type":"marketing_consent","granted":true,"granted_at":"2026-05-09T00:00:00Z"}`))
	})

	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: 1 * time.Second}, nil)
	body := []byte(`{"consent_type":"marketing_consent","granted":true,"version":"2026-05-07"}`)
	res, _ := a.GrantConsent(context.Background(), phyllis.AuthCtx{Bearer: "phyllis"}, body)
	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", res.Status)
	}
	if !strings.Contains(lastBody, "marketing_consent") {
		t.Errorf("upstream did not receive body; got %q", lastBody)
	}
}

func TestGrantConsent_BadRequest(t *testing.T) {
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"INVALID_CONSENT_TYPE"}}`))
	})

	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.GrantConsent(context.Background(), phyllis.AuthCtx{Bearer: "x"}, []byte(`{}`))
	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", res.Status)
	}
}

// -----------------------------------------------------------------------------
// RequestDataExport — POST chora-identity:/me/portability/export
// -----------------------------------------------------------------------------

func TestRequestDataExport_HappyPath(t *testing.T) {
	identity := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/portability/export" {
			t.Errorf("path = %s; want /me/portability/export", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s; want POST", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"export_id":"01900-export","status":"queued","sla_days":30}`))
	})

	a := phyllis.New(phyllis.Config{IdentityURL: identity.URL, PerCallTimeout: 1 * time.Second}, nil)
	res, _ := a.RequestDataExport(context.Background(), phyllis.AuthCtx{Bearer: "phyllis"}, []byte(`{"format":"portable_json"}`))
	if res.Status != http.StatusAccepted {
		t.Errorf("status = %d; want 202", res.Status)
	}
	if !strings.Contains(string(res.Body), "01900-export") {
		t.Errorf("body did not pass-through export_id: %s", string(res.Body))
	}
}

// guard the test file is wired.
var _ = httptest.NewServer
