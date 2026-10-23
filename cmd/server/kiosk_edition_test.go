// kiosk_edition_test.go: TDD specs (RED first) for the kiosk edition's boot of the edge
// gateway (ADR-260 D1, D2; INTERFACES.md 2, 3.2; TRACKER T1.7).
//
// With CHORA_EDITION unset every test here expects the cloud behaviour unchanged: that is
// the negative control for each kiosk behaviour.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	kioskTestSigner   = "6b696f736b2d746573742d7369676e65722d6e6f742d612d7265616c2d6b6579"
	kioskTestIssuer   = "chora-kiosk-session"
	kioskTestAudience = "chora-kiosk-edge"
)

// writeInternalEnv writes a dotenv file like the install script's (mode 600).
func writeInternalEnv(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "internal.env")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// clearCloudEnv blanks every variable the cloud gate or a cloud loader reads, so a test
// sees only what it sets.
func clearCloudEnv(t *testing.T) {
	t.Helper()
	for _, k := range append(append([]string{}, RequiredScalarEnvVars...), RequiredSVCEnvVars...) {
		t.Setenv(k, "")
	}
	for _, k := range []string{
		EnvEdition, EnvKioskInternalSecretsFile, EnvKioskAbsentUpstreams, EnvKioskConductorURL,
		EnvIDPEmailAllowlistSec, EnvIDPGoogleEmailAllowlistSec, EnvMintDisabled, EnvStripeConfigDisabled,
		EnvPaymentsDisabled, EnvPaymentsGRPCAddr, "CHORA_PAYMENTS_HTTP_ADDR", EnvTenancyGRPCAddr,
		EnvGovernanceGRPCAddr, EnvGovernanceHTTPAddr, EnvObservabilityHTTPAddr, EnvA2AHTTPAddr,
		EnvClosureOrchestratorURL, EnvRealtimeTicketSignerSecretID, "CHORA_SHARING_HTTP_ADDR",
		"CHORA_NOTIFICATIONS_HTTP_ADDR", "CHORA_GATEWAY_UPSTREAM_FAKE",
	} {
		t.Setenv(k, "")
	}
}

// setKioskEnv sets the kiosk compose file's environment for the edge gateway.
func setKioskEnv(t *testing.T) {
	t.Helper()
	clearCloudEnv(t)
	t.Setenv(EnvEdition, "kiosk")
	t.Setenv(EnvChoraSessionJWTSignerSecret, "CHORA_KIOSK_SESSION_SIGNER")
	t.Setenv(EnvChoraSessionIssuerURL, kioskTestIssuer)
	t.Setenv(EnvChoraSessionAudienceURL, kioskTestAudience)
	t.Setenv(EnvKioskInternalSecretsFile, writeInternalEnv(t,
		"CHORA_KIOSK_SESSION_SIGNER="+kioskTestSigner+"\nCHORA_KIOSK_OPERATOR_SECRET="+strings.Repeat("ab", 32)+"\n"))
	t.Setenv(EnvKioskAbsentUpstreams, "sharing,notifications,tenancy,governance,payments,closure,realtime")
	t.Setenv(EnvKioskConductorURL, "http://chora-kiosk-conductor:8080")
	t.Setenv("SVC_IDENTITY_URL", "http://chora-identity:8080")
	t.Setenv("SVC_CREATION_URL", "http://chora-creation:8080")
	t.Setenv("SVC_CONSUMPTION_URL", "http://chora-consumption:8080")
	t.Setenv("SVC_DELIVERY_URL", "http://chora-delivery:8080")
	t.Setenv("SVC_OBSERVABILITY_URL", "http://chora-observability:8080")
	t.Setenv(EnvObservabilityHTTPAddr, "http://chora-observability:8080")
	t.Setenv(EnvMintDisabled, "true")
	t.Setenv(EnvStripeConfigDisabled, "true")
	t.Setenv(EnvPaymentsDisabled, "true")
	t.Setenv(EnvAIKernelOrchestratorURL, "http://chora-ai-kernel-orchestrator:8080")
}

// --- The switch -------------------------------------------------------------------------

func TestKioskEdition_UnsetIsTheCloudEdition(t *testing.T) {
	clearCloudEnv(t)
	k, err := loadKioskEdition()
	if err != nil || k != nil {
		t.Fatalf("CHORA_EDITION unset: want (nil, nil), got (%v, %v)", k, err)
	}
}

func TestKioskEdition_UnknownEditionIsRefused(t *testing.T) {
	setKioskEnv(t)
	t.Setenv(EnvEdition, "kiosks")
	if _, err := loadKioskEdition(); !errors.Is(err, ErrKioskConfig) {
		t.Fatalf("CHORA_EDITION=kiosks: want ErrKioskConfig, got %v", err)
	}
}

// --- The boot gate ----------------------------------------------------------------------

func TestKioskEdition_BootsWithNoAbsentUpstreamAddress(t *testing.T) {
	setKioskEnv(t)
	k, err := loadKioskEdition()
	if err != nil {
		t.Fatalf("kiosk env: want a clean boot, got %v", err)
	}
	if k == nil {
		t.Fatal("kiosk env: want a kiosk edition, got nil")
	}
}

// Negative control: the same environment without the switch still fails the cloud gate.
func TestKioskEdition_CloudGateStillRefusesTheKioskEnvironment(t *testing.T) {
	setKioskEnv(t)
	t.Setenv(EnvEdition, "")
	if err := CheckBootEnv(); !errors.Is(err, ErrBootEnvMissing) {
		t.Fatalf("kiosk env without CHORA_EDITION: the cloud gate must refuse, got %v", err)
	}
	// Satisfy this repo's scalar block so the refusal lands on an absent
	// upstream's address. The CR source enumerates its GCP/Identity-Platform
	// scalars here; this gateway's cloud contract is CHORA_SESSION_SIGNER based,
	// so the equivalent step is that one var (issuer/audience come from
	// setKioskEnv).
	t.Setenv(EnvChoraSessionSigner, strings.Repeat("k", 32))
	err := CheckBootEnv()
	if !errors.Is(err, ErrBootEnvMissing) || !strings.Contains(err.Error(), "SVC_TENANCY_URL") {
		t.Fatalf("an absent upstream's address must still be required by the cloud gate, got %v", err)
	}
}

func TestKioskEdition_BootRefusals(t *testing.T) {
	cases := []struct {
		name string
		mod  func(t *testing.T)
		want string
	}{
		{"no absent-upstream declaration", func(t *testing.T) { t.Setenv(EnvKioskAbsentUpstreams, "") }, EnvKioskAbsentUpstreams},
		{"an unknown upstream name", func(t *testing.T) {
			t.Setenv(EnvKioskAbsentUpstreams, "sharing,notifications,tenancy,governance,payments,closure,realtime,typo")
		}, "typo"},
		{"a needed upstream declared absent", func(t *testing.T) {
			t.Setenv(EnvKioskAbsentUpstreams, "sharing,notifications,tenancy,governance,payments,closure,realtime,consumption")
		}, "consumption"},
		{"an absent upstream left out of the declaration", func(t *testing.T) {
			t.Setenv(EnvKioskAbsentUpstreams, "sharing,notifications,tenancy,governance,payments,closure")
		}, "realtime"},
		{"an absent upstream with an address", func(t *testing.T) { t.Setenv("SVC_SHARING_URL", "http://chora-sharing:8080") }, "SVC_SHARING_URL"},
		{"governance with a gRPC address", func(t *testing.T) { t.Setenv(EnvGovernanceGRPCAddr, "chora-governance:9090") }, EnvGovernanceGRPCAddr},
		{"closure with an address", func(t *testing.T) { t.Setenv(EnvClosureOrchestratorURL, "http://x:8080") }, EnvClosureOrchestratorURL},
		{"realtime with a signer", func(t *testing.T) { t.Setenv(EnvRealtimeTicketSignerSecretID, "s") }, EnvRealtimeTicketSignerSecretID},
		{"a present upstream without an address", func(t *testing.T) { t.Setenv("SVC_CONSUMPTION_URL", "") }, "SVC_CONSUMPTION_URL"},
		{"a present upstream with a bad address", func(t *testing.T) { t.Setenv("SVC_DELIVERY_URL", "chora-delivery:8080") }, "SVC_DELIVERY_URL"},
		{"the mint left on", func(t *testing.T) { t.Setenv(EnvMintDisabled, "") }, EnvMintDisabled},
		{"stripe config left on", func(t *testing.T) { t.Setenv(EnvStripeConfigDisabled, "false") }, EnvStripeConfigDisabled},
		{"payments left on", func(t *testing.T) { t.Setenv(EnvPaymentsDisabled, "") }, EnvPaymentsDisabled},
		{"a Google project set", func(t *testing.T) { t.Setenv("GCP_PROJECT", "chora-489812") }, "GCP_PROJECT"},
		{"an IdP issuer set", func(t *testing.T) { t.Setenv("IDP_ISSUER_URL", "https://securetoken.google.com/x") }, "IDP_ISSUER_URL"},
		{"no internal secrets file", func(t *testing.T) { t.Setenv(EnvKioskInternalSecretsFile, "") }, EnvKioskInternalSecretsFile},
		{"no conductor", func(t *testing.T) { t.Setenv(EnvKioskConductorURL, "") }, EnvKioskConductorURL},
		{"a conductor that is not a URL", func(t *testing.T) { t.Setenv(EnvKioskConductorURL, "chora-kiosk-conductor") }, EnvKioskConductorURL},
		{"no session issuer", func(t *testing.T) { t.Setenv(EnvChoraSessionIssuerURL, "") }, EnvChoraSessionIssuerURL},
		{"no observability address for O+", func(t *testing.T) { t.Setenv(EnvObservabilityHTTPAddr, "") }, EnvObservabilityHTTPAddr},
		// W20 (K158): the weakness review resumes on the orchestrator, plainly.
		{"no orchestrator for the review resume", func(t *testing.T) { t.Setenv(EnvAIKernelOrchestratorURL, "") }, EnvAIKernelOrchestratorURL},
		{"an orchestrator that is not a URL", func(t *testing.T) { t.Setenv(EnvAIKernelOrchestratorURL, "chora-ai-kernel-orchestrator") }, EnvAIKernelOrchestratorURL},
		{"a Google ID token for the orchestrator", func(t *testing.T) {
			t.Setenv(EnvAIKernelIDTokenAudience, "auto")
		}, EnvAIKernelIDTokenAudience},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setKioskEnv(t)
			c.mod(t)
			_, err := loadKioskEdition()
			if !errors.Is(err, ErrKioskConfig) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want ErrKioskConfig naming %q, got %v", c.want, err)
			}
		})
	}
}

// --- The session signer from internal.env -------------------------------------------------

func TestKioskSigner_ReadThroughTheExistingLoader(t *testing.T) {
	setKioskEnv(t)
	k, err := loadKioskEdition()
	if err != nil {
		t.Fatal(err)
	}
	v, cleanup, err := NewChoraSessionValidatorFromEnvWith(context.Background(), k.secretReader())
	if err != nil {
		t.Fatalf("validator from internal.env: %v", err)
	}
	defer func() { _ = cleanup() }()
	token := signKioskToken(t, []byte(kioskTestSigner), nil)
	if _, err := v.Validate(token); err != nil {
		t.Fatalf("a token signed with the bytes of CHORA_KIOSK_SESSION_SIGNER must validate: %v", err)
	}
	other := signKioskToken(t, []byte(strings.Repeat("cd", 32)), nil)
	if _, err := v.Validate(other); err == nil {
		t.Fatal("a token from another signer must be refused")
	}
}

func TestKioskSigner_ShortSignerIsRefused(t *testing.T) {
	setKioskEnv(t)
	t.Setenv(EnvKioskInternalSecretsFile, writeInternalEnv(t, "CHORA_KIOSK_SESSION_SIGNER=0123456789abcdef0123456789abcde\n"))
	k, err := loadKioskEdition()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewChoraSessionValidatorFromEnvWith(context.Background(), k.secretReader()); err == nil ||
		!strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("a 31-byte signer must be refused at boot, got %v", err)
	}
}

func TestDotenvSecretReader(t *testing.T) {
	cases := []struct {
		name, body, key, want string
		ok                    bool
	}{
		{"a value", "A=1\nCHORA_KIOSK_SESSION_SIGNER=abc\n", "CHORA_KIOSK_SESSION_SIGNER", "abc", true},
		{"surrounding whitespace is trimmed", "CHORA_KIOSK_SESSION_SIGNER=  abc  \n", "CHORA_KIOSK_SESSION_SIGNER", "abc", true},
		{"blank lines and comments", "\n# note\nK=v\n", "K", "v", true},
		{"a missing key", "A=1\n", "B", "", false},
		{"a duplicated key", "K=1\nK=2\n", "K", "", false},
		{"a line that is not KEY=value", "K\n", "K", "", false},
		{"an empty value", "K=\n", "K", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newDotenvSecretReader(writeInternalEnv(t, c.body))
			got, err := r.GetSecret(context.Background(), c.key)
			if c.ok && (err != nil || got != c.want) {
				t.Fatalf("want %q, got %q, %v", c.want, got, err)
			}
			if !c.ok && err == nil {
				t.Fatalf("want an error, got %q", got)
			}
			if err != nil && strings.Contains(err.Error(), "abc") {
				t.Fatalf("an error must never carry a value: %v", err)
			}
		})
	}
	if _, err := newDotenvSecretReader(filepath.Join(t.TempDir(), "absent.env")).GetSecret(context.Background(), "K"); err == nil {
		t.Fatal("a missing file must be an error")
	}
}

// --- The routes: default deny, the conductor, the operator endpoints ---------------------

type kioskRig struct {
	handler   http.Handler
	next      []string // paths that reached the product chain
	conductor []*http.Request
	bodies    []string
}

func newKioskRig(t *testing.T) *kioskRig {
	t.Helper()
	setKioskEnv(t)
	rig := &kioskRig{}
	conductor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rig.conductor = append(rig.conductor, r)
		rig.bodies = append(rig.bodies, string(b))
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(conductor.Close)
	t.Setenv(EnvKioskConductorURL, conductor.URL)
	k, err := loadKioskEdition()
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := NewChoraSessionValidatorFromEnvWith(context.Background(), k.secretReader())
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.next = append(rig.next, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	})
	rig.handler = k.routes(v, next)
	return rig
}

func (rig *kioskRig) do(t *testing.T, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	rig.handler.ServeHTTP(rec, req)
	return rec
}

func TestKioskRoutes_SessionIsOpenAndForwardedUnchanged(t *testing.T) {
	rig := newKioskRig(t)
	rec := rig.do(t, http.MethodPost, "/api/v1/kiosk/session", "")
	if rec.Code != http.StatusTeapot || len(rig.conductor) != 1 {
		t.Fatalf("POST /api/v1/kiosk/session: want the conductor's answer, got %d (%d forwarded)", rec.Code, len(rig.conductor))
	}
	if got := rig.conductor[0].URL.Path; got != "/api/v1/kiosk/session" {
		t.Fatalf("the path must reach the conductor unchanged, got %q", got)
	}
	if rig.bodies[0] != `{}` {
		t.Fatalf("the body must reach the conductor, got %q", rig.bodies[0])
	}
}

func TestKioskRoutes_OtherKioskRoutesNeedTheSession(t *testing.T) {
	rig := newKioskRig(t)
	for _, p := range []string{"/api/v1/kiosk/visits", "/api/v1/kiosk/readiness", "/api/v1/kiosk/prewarm/manifest", "/api/v1/kiosk/xray/run-1"} {
		if rec := rig.do(t, http.MethodGet, p, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s without a session: want 401, got %d", p, rec.Code)
		}
	}
	if len(rig.conductor) != 0 {
		t.Fatalf("nothing without a session may reach the conductor, %d did", len(rig.conductor))
	}
	token := signKioskToken(t, []byte(kioskTestSigner), nil)
	for _, p := range []string{"/api/v1/kiosk/visits", "/api/v1/kiosk/prewarm/manifest", "/api/v1/kiosk/xray/run-1"} {
		if rec := rig.do(t, http.MethodGet, p, token); rec.Code != http.StatusTeapot {
			t.Fatalf("GET %s with a session: want the conductor's answer, got %d", p, rec.Code)
		}
	}
	if rec := rig.do(t, http.MethodPost, "/api/v1/kiosk/reset", token); rec.Code != http.StatusTeapot {
		t.Fatalf("POST /api/v1/kiosk/reset with a session: want the conductor's answer, got %d", rec.Code)
	}
	last := rig.conductor[len(rig.conductor)-1]
	if last.URL.Path != "/api/v1/kiosk/reset" || last.Header.Get("Authorization") != "Bearer "+token {
		t.Fatalf("the conductor must see the path and the session unchanged, got %q %q", last.URL.Path, last.Header.Get("Authorization"))
	}
	if len(rig.next) != 0 {
		t.Fatalf("kiosk routes must not reach the product chain, %v did", rig.next)
	}
}

func TestKioskRoutes_PrewarmIsRefusedAtTheEdge(t *testing.T) {
	rig := newKioskRig(t)
	token := signKioskToken(t, []byte(kioskTestSigner), nil)
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		for _, p := range []string{"/api/v1/kiosk/prewarm", "/api/v1/kiosk/prewarm/"} {
			rec := rig.do(t, m, p, token)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s %s: want 403, got %d", m, p, rec.Code)
			}
			if code := errorCode(t, rec); code != "KIOSK_OPERATOR_ONLY" {
				t.Fatalf("%s %s: want KIOSK_OPERATOR_ONLY, got %q", m, p, code)
			}
		}
	}
	if len(rig.conductor) != 0 {
		t.Fatalf("the operator endpoint must never be forwarded, %d were", len(rig.conductor))
	}
}

func TestKioskRoutes_TheKiosksProductRoutesPass(t *testing.T) {
	rig := newKioskRig(t)
	for _, c := range [][2]string{
		{http.MethodGet, "/api/v1/me/goals"},
		{http.MethodGet, "/api/v1/me/goals/g1/campaign/questions"},
		{http.MethodGet, "/api/v1/me/maps/m1/graph"},
		{http.MethodGet, "/api/v1/me/concept-graph/suggestions"},
		{http.MethodPost, "/api/atoms/a1/question-jobs"},
		{http.MethodPost, "/api/atoms/a1/question-jobs/j1/accept"},
		{http.MethodPost, "/api/atoms/a1/session"},
		{http.MethodPost, "/api/atoms/a1/session/submit"},
		{http.MethodGet, "/api/companion/daily-dose"},
		{http.MethodGet, "/api/companion/daily-dose/ai"},
		{http.MethodPost, "/api/companion/greeting"},
		{http.MethodPost, "/api/v1/me/assessments/x1/submissions/s1/request-grading"},
		{http.MethodGet, "/api/v1/me/concept-graph/suggestions/requests/r1"},
		{http.MethodGet, "/bff/oplus/governance?trace_id=0af7651916cd43dd8448eb211c80319c"},
		{http.MethodGet, "/api/v1/me/companions/c1/growth"},
		{http.MethodGet, "/api/v1/question-banks/b1/questions"},
		{http.MethodGet, "/api/v1/me/question-banks"},
		{http.MethodPost, "/api/v1/test-sets/t1/publish"},
		{http.MethodPost, "/api/v1/assessments"},
		{http.MethodGet, "/api/v1/me/assessments/x1/submissions/s1/result"},
		{http.MethodGet, "/bff/oplus/governance"},
		// W20 (K158, D9): the weakness analyser's door, its poll, its review
		// resume and the Growth Edge list.
		{http.MethodPost, "/api/v1/me/growth-edges/uploads"},
		{http.MethodGet, "/api/v1/me/growth-edges/uploads/u1"},
		{http.MethodPost, "/api/v1/me/growth-edges/uploads/u1/resume"},
		{http.MethodGet, "/api/v1/me/growth-edges"},
		{http.MethodGet, "/healthz"},
	} {
		if rec := rig.do(t, c[0], c[1], ""); rec.Code != http.StatusNoContent {
			t.Fatalf("%s %s: want the product chain, got %d", c[0], c[1], rec.Code)
		}
	}
}

func TestKioskRoutes_EverythingElseIsNotPartOfTheKiosk(t *testing.T) {
	rig := newKioskRig(t)
	token := signKioskToken(t, []byte(kioskTestSigner), nil)
	for _, c := range [][2]string{
		{http.MethodGet, "/bff/cplus/feed"},                     // sharing
		{http.MethodGet, "/api/v1/notifications"},               // notifications
		{http.MethodGet, "/api/v1/companion-eggs/catalog"},      // tenancy
		{http.MethodGet, "/api/v1/tenants/me"},                  // tenancy
		{http.MethodGet, "/bff/oplus/governance/hitl"},          // governance
		{http.MethodPost, "/api/v1/checkout/course"},            // payments
		{http.MethodPost, "/v1/webhooks/stripe"},                // payments
		{http.MethodPost, "/api/v1/me/account/close"},           // closure
		{http.MethodGet, "/api/v1/realtime/ticket"},             // realtime
		{http.MethodPost, "/api/v1/auth/session/mint"},          // the cloud mint
		{http.MethodPost, "/api/internal/pubsub/grading-inbox"}, // pushes go to the owner directly
		{http.MethodPost, "/api/v1/me/mana/topup"},              // present upstream, not a kiosk route
		{http.MethodGet, "/graphql"},
		{http.MethodGet, "/api/v1/me/goalsx"},
		{http.MethodGet, "/api/v1/me/growth-edgesx"},
		{http.MethodGet, "/api/companion/greeting"}, // the greeting is POST only
	} {
		rec := rig.do(t, c[0], c[1], token)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: want 503, got %d", c[0], c[1], rec.Code)
		}
		if code := errorCode(t, rec); code != "NOT_PART_OF_KIOSK_EDITION" {
			t.Fatalf("%s %s: want NOT_PART_OF_KIOSK_EDITION, got %q", c[0], c[1], code)
		}
	}
	if len(rig.next) != 0 || len(rig.conductor) != 0 {
		t.Fatalf("a refused route must reach nothing: product %v, conductor %d", rig.next, len(rig.conductor))
	}
}

func TestKioskEdition_BootLineNamesEveryBehaviour(t *testing.T) {
	setKioskEnv(t)
	k, err := loadKioskEdition()
	if err != nil {
		t.Fatal(err)
	}
	line := k.bootLine()
	for _, want := range []string{"KIOSK EDITION: ", "internal.env", "sharing", "realtime", "default deny", "/api/v1/kiosk/", "prewarm refused",
		"weakness review resume to http://chora-ai-kernel-orchestrator:8080, no ID token"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the boot line %q must name %q", line, want)
		}
	}
}

// --- O+ without governance --------------------------------------------------------------

func TestKioskOPlus_BuildsWithoutGovernance(t *testing.T) {
	setKioskEnv(t)
	h, cleanup, resolver, err := newOPlusHandler(context.Background(), true)
	if err != nil || h == nil || resolver == nil {
		t.Fatalf("O+ without governance: want a handler, got %v, %v", h, err)
	}
	defer func() { _ = cleanup() }()
	if h.Governance != nil {
		t.Fatal("O+ without governance must hold no governance client")
	}
	if h.Observability == nil {
		t.Fatal("O+ without governance must still read observability")
	}
}

// Negative control: the cloud loader still refuses a missing governance address.
func TestKioskOPlus_CloudLoaderStillNeedsGovernance(t *testing.T) {
	setKioskEnv(t)
	if _, _, _, err := NewOPlusHandlerFromEnv(context.Background()); !errors.Is(err, ErrOPlusBackendMissing) {
		t.Fatalf("the cloud O+ loader must refuse a missing governance address, got %v", err)
	}
}

// --- helpers ---------------------------------------------------------------------------

func signKioskToken(t *testing.T, key []byte, overrides map[string]any) string {
	t.Helper()
	now := time.Now()
	claims := map[string]any{
		"iss": kioskTestIssuer, "aud": kioskTestAudience,
		"sub": "01a1411f-7f22-76c6-ab1d-362dddc20413", "gcid": "01a1411f-7f22-76c6-ab1d-362dddc20413",
		"tenant_id": "01a1411f-7f20-7834-bdd5-6f80318297bf", "email": "visitor@apollo.kiosk.invalid",
		"roles": []string{"learner", "author", "instructor", "auditor"}, "role_summary": "learner+author+instructor+auditor",
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	for k, v := range overrides {
		claims[k] = v
	}
	hb, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	cb, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("not the gateway's error envelope: %q", rec.Body.String())
	}
	return env.Error.Code
}

// T1.11 (ruled 18 Oct): under the switch the health probes write no span; the
// cloud edition (no kiosk edition) traces every path (the negative control).
func TestKioskEdition_HealthProbesWriteNoSpan(t *testing.T) {
	var cloud *kioskEdition
	if p := cloud.untracedPaths(); len(p) != 0 {
		t.Fatalf("the cloud edition skips spans for %v", p)
	}
	setKioskEnv(t)
	k, err := loadKioskEdition()
	if err != nil {
		t.Fatal(err)
	}
	if p := strings.Join(k.untracedPaths(), " "); p != "/healthz /readyz" {
		t.Errorf("kiosk paths = %q, want /healthz /readyz", p)
	}
	if want := "no span for /healthz or /readyz (T1.11)"; !strings.Contains(k.bootLine(), want) {
		t.Errorf("boot line %q misses %q", k.bootLine(), want)
	}
}
