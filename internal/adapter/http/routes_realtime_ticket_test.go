// routes_realtime_ticket_test.go — strict TDD RED for the W0 (rt) realtime
// stream-ticket minter (ADR-183, CHO-1661; contract
// chora-contracts/openapi/realtime.yaml mintRealtimeTicket).
//
// The minted ticket is consumed by chora-realtime's
// internal/adapter/ticket/hmac_validator.go — these specs pin the EXACT
// cross-service claim contract (HS256 / aud "chora-realtime" / gcid /
// tenant_id / jti / iat / exp≈+60s) so a drift on either side fails here
// before it fails on chora.site.
package httpadapter_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
)

const (
	rtTicketPath   = "/api/v1/realtime/ticket"
	rtTicketGCID   = "01935f12-0000-7000-8000-0000000000bb"
	rtTicketTenant = "01970000-0000-7000-8000-0000000000aa"
)

// rtTicketSigner is ≥32 bytes per the HS256 floor shared with the
// chora-realtime validator (RFC 7518 §3.2).
var rtTicketSigner = []byte("test-realtime-ticket-signer-key-32-bytes-min!!")

// rtTicketClaims mirrors chora-realtime's ticket.Claims wire shape.
type rtTicketClaims struct {
	Aud      string `json:"aud"`
	GCID     string `json:"gcid"`
	TenantID string `json:"tenant_id"`
	JTI      string `json:"jti"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
}

// rtTicketResponse mirrors the contract's RealtimeTicket schema.
type rtTicketResponse struct {
	Ticket    string `json:"ticket"`
	ExpiresAt string `json:"expires_at"`
}

func newRealtimeTicketBridge(t *testing.T) http.Handler {
	t.Helper()
	m, err := httpadapter.NewRealtimeTicketMinter(rtTicketSigner)
	if err != nil {
		t.Fatalf("NewRealtimeTicketMinter: %v", err)
	}
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	return httpadapter.WithRealtimeTicket(base, m)
}

func doRealtimeTicket(t *testing.T, h http.Handler, method, gcid, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), method, rtTicketPath, nil)
	r.Header.Set("Authorization", "Bearer rt-ticket-session-token")
	if gcid != "" {
		r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), gcid, tenant))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// decodeRTTicket structurally parses + HMAC-verifies a minted ticket the way
// chora-realtime's validator does (hand-rolled, no golang-jwt — codebase
// convention).
func decodeRTTicket(t *testing.T, raw string) rtTicketClaims {
	t.Helper()
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("ticket must be a 3-segment compact JWT, got %d segments", len(parts))
	}
	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var hdr struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		t.Fatalf("parse header: %v", err)
	}
	if hdr.Alg != "HS256" || hdr.Typ != "JWT" {
		t.Fatalf("header must be HS256/JWT, got %+v", hdr)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	mac := hmac.New(sha256.New, rtTicketSigner)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(mac.Sum(nil), sig) {
		t.Fatalf("HMAC signature does not verify with the shared signer")
	}
	clmJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var c rtTicketClaims
	if err := json.Unmarshal(clmJSON, &c); err != nil {
		t.Fatalf("parse claims: %v", err)
	}
	return c
}

func TestRealtimeTicket_MintsValidatorCompatibleTicket(t *testing.T) {
	h := newRealtimeTicketBridge(t)
	before := time.Now().UTC()
	w := doRealtimeTicket(t, h, http.MethodGet, rtTicketGCID, rtTicketTenant)
	after := time.Now().UTC()

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	// A ticket is a credential — it must never be cached.
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}

	var resp rtTicketResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if resp.Ticket == "" {
		t.Fatalf("response.ticket empty")
	}

	c := decodeRTTicket(t, resp.Ticket)
	if c.Aud != "chora-realtime" {
		t.Errorf("aud = %q, want chora-realtime", c.Aud)
	}
	if c.GCID != rtTicketGCID {
		t.Errorf("gcid = %q, want %q", c.GCID, rtTicketGCID)
	}
	if c.TenantID != rtTicketTenant {
		t.Errorf("tenant_id = %q, want %q", c.TenantID, rtTicketTenant)
	}
	jti, err := uuid.Parse(c.JTI)
	if err != nil {
		t.Errorf("jti %q is not a UUID: %v", c.JTI, err)
	} else if jti.Version() != 7 && jti.Version() != 4 {
		// UUIDv7 per the platform convention; v4 only as the documented
		// clock-backstep fallback (payments_handler.go precedent).
		t.Errorf("jti version = %d, want 7 (or fallback 4)", jti.Version())
	}
	if c.IssuedAt < before.Add(-2*time.Second).Unix() || c.IssuedAt > after.Add(2*time.Second).Unix() {
		t.Errorf("iat = %d not within mint window [%d, %d]", c.IssuedAt, before.Unix(), after.Unix())
	}
	if got := c.Expires - c.IssuedAt; got != 60 {
		t.Errorf("exp - iat = %d, want 60 (the ~60s ticket lifetime)", got)
	}

	expAt, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at %q is not RFC3339: %v", resp.ExpiresAt, err)
	}
	if expAt.Unix() != c.Expires {
		t.Errorf("expires_at (%d) != claim exp (%d)", expAt.Unix(), c.Expires)
	}
}

func TestRealtimeTicket_JTIsAreUniquePerMint(t *testing.T) {
	h := newRealtimeTicketBridge(t)
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		w := doRealtimeTicket(t, h, http.MethodGet, rtTicketGCID, rtTicketTenant)
		if w.Code != http.StatusOK {
			t.Fatalf("mint %d: status %d", i, w.Code)
		}
		var resp rtTicketResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("mint %d: bad json: %v", i, err)
		}
		c := decodeRTTicket(t, resp.Ticket)
		if seen[c.JTI] {
			t.Fatalf("jti %q repeated — tickets must be single-use nonces", c.JTI)
		}
		seen[c.JTI] = true
	}
}

func TestRealtimeTicket_EmptyTenantStillMints(t *testing.T) {
	// Bootstrap-mode session JWTs (CHO-1648/1653) carry tenant_id="" — the
	// chora-realtime validator only requires gcid, so the minter must not
	// reject them (the stream simply has no tenant-scoped frames to filter).
	h := newRealtimeTicketBridge(t)
	w := doRealtimeTicket(t, h, http.MethodGet, rtTicketGCID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp rtTicketResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if c := decodeRTTicket(t, resp.Ticket); c.TenantID != "" {
		t.Errorf("tenant_id = %q, want empty", c.TenantID)
	}
}

func TestRealtimeTicket_MissingMeshClaims401(t *testing.T) {
	h := newRealtimeTicketBridge(t)
	w := doRealtimeTicket(t, h, http.MethodGet, "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "REALTIME_UNAUTHENTICATED") {
		t.Fatalf("body %q missing contract code REALTIME_UNAUTHENTICATED", w.Body.String())
	}
}

func TestRealtimeTicket_MethodDiscipline405(t *testing.T) {
	h := newRealtimeTicketBridge(t)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		w := doRealtimeTicket(t, h, m, rtTicketGCID, rtTicketTenant)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", m, w.Code)
		}
	}
}

func TestRealtimeTicket_NilMinterFallsThrough(t *testing.T) {
	// Unconfigured env (no CHORA_REALTIME_TICKET_SIGNER_SECRET) → the bridge
	// passes through so the route 404s cleanly — the established nil-bridge
	// convention (kg-canvas / gatewayproxy / medashboard).
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := httpadapter.WithRealtimeTicket(base, nil)
	w := doRealtimeTicket(t, h, http.MethodGet, rtTicketGCID, rtTicketTenant)
	if w.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418 passthrough when minter is nil", w.Code)
	}
}

func TestRealtimeTicket_OtherPathsFallThrough(t *testing.T) {
	h := newRealtimeTicketBridge(t)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/realtime/stream", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418 passthrough for non-ticket paths", w.Code)
	}
}

func TestRealtimeTicket_RejectsShortSigner(t *testing.T) {
	if _, err := httpadapter.NewRealtimeTicketMinter([]byte("too-short")); err == nil {
		t.Fatalf("want error for <32-byte signer (RFC 7518 §3.2 floor shared with the validator)")
	}
}

func TestRealtimeTicket_PathIsJWTGated(t *testing.T) {
	// RequireChoraSessionJWT must stamp MeshClaims BEFORE the bridge fires —
	// without this entry the request reaches the minter claims-less and every
	// call 401s (the exact class of the CHO-1652 wizard-bootstrap gap).
	for _, p := range httpadapter.DefaultJWTGatedPrefixes {
		if p == rtTicketPath {
			return
		}
	}
	t.Fatalf("%s missing from DefaultJWTGatedPrefixes", rtTicketPath)
}

func TestNewRealtimeTicketMinterFromEnv(t *testing.T) {
	t.Run("unset env disables cleanly", func(t *testing.T) {
		t.Setenv(httpadapter.EnvRealtimeTicketSigner, "")
		m, err := httpadapter.NewRealtimeTicketMinterFromEnv()
		if err != nil || m != nil {
			t.Fatalf("want (nil, nil) for unset env, got (%v, %v)", m, err)
		}
	})
	t.Run("valid secret wires the minter", func(t *testing.T) {
		t.Setenv(httpadapter.EnvRealtimeTicketSigner, string(rtTicketSigner))
		m, err := httpadapter.NewRealtimeTicketMinterFromEnv()
		if err != nil || m == nil {
			t.Fatalf("want live minter, got (%v, %v)", m, err)
		}
	})
	t.Run("short secret fails loud", func(t *testing.T) {
		t.Setenv(httpadapter.EnvRealtimeTicketSigner, "short")
		if _, err := httpadapter.NewRealtimeTicketMinterFromEnv(); err == nil {
			t.Fatalf("want error for undersized env secret")
		}
	})
}
