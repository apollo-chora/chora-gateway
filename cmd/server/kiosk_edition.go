// kiosk_edition.go: the kiosk edition's boot of the edge gateway (ADR-259 D2, ADR-260 D1,
// D2; INTERFACES.md 2 and 3.2; TRACKER T1.7). Kiosk branch only.
//
// CHORA_EDITION=kiosk is the one switch, read once here. Unset, nothing in this file is
// wired and the gateway boots exactly as the cloud edition does. Set, it:
//
//   - replaces the cloud boot gate: the IdP, Firebase and Secret Manager settings are not
//     required and are REFUSED if set (the kiosk has no IdP and no Google project); the
//     mint, Stripe config and payments must be switched off by their own flags;
//   - declares the upstreams the laptop does not run (CHORA_KIOSK_ABSENT_UPSTREAMS, which
//     must name exactly sharing, notifications, tenancy, governance, payments, closure and
//     realtime) and refuses any address for one of them, instead of faking addresses that
//     nothing serves;
//   - reads the session signer from the internal secrets file through the existing loader
//     (CHORA_SESSION_JWT_SIGNER_SECRET names the key, CHORA_KIOSK_INTERNAL_SECRETS_FILE the
//     dotenv file), never from Secret Manager;
//   - routes by default deny: only the kiosk's product routes (API-CONTRACT.md) reach the
//     product chain, /api/v1/kiosk/* goes to the conductor with the path unchanged (POST
//     session open, every other route behind the session), the operator's pre-warm is
//     refused at the edge, and everything else answers 503 NOT_PART_OF_KIOSK_EDITION.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/apollo-chora/chora-common/auth/chorasession"
	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
	"github.com/apollo-chora/chora-gateway/internal/middleware"
)

const (
	EnvEdition                  = "CHORA_EDITION"
	EditionKiosk                = "kiosk"
	EnvKioskInternalSecretsFile = "CHORA_KIOSK_INTERNAL_SECRETS_FILE" // #nosec G101 -- env-var name, not a credential.
	EnvKioskAbsentUpstreams     = "CHORA_KIOSK_ABSENT_UPSTREAMS"
	EnvKioskConductorURL        = "CHORA_KIOSK_CONDUCTOR_URL"
	// EnvKioskSeedRoutes = "on" opens the golden run's seed routes (T6.2 part 2) on the
	// owner's Mac; the handover compose leaves it unset. Any other value refuses the boot.
	EnvKioskSeedRoutes = "CHORA_KIOSK_SEED_ROUTES"
)

// ErrKioskConfig is the sentinel for every kiosk boot refusal.
var ErrKioskConfig = errors.New("kiosk edition: configuration refused")

// kioskAbsentUpstreams maps each upstream the laptop does not run to the settings that would
// address it. All of them must be blank in kiosk mode (ADR-260 D1).
var kioskAbsentUpstreams = map[string][]string{
	"sharing":       {"SVC_SHARING_URL", "CHORA_SHARING_HTTP_ADDR"},
	"notifications": {"SVC_NOTIFICATIONS_URL", "CHORA_NOTIFICATIONS_HTTP_ADDR"},
	"tenancy":       {"SVC_TENANCY_URL", EnvTenancyGRPCAddr},
	"governance":    {"SVC_GOVERNANCE_URL", EnvGovernanceGRPCAddr, EnvGovernanceHTTPAddr},
	"payments":      {EnvPaymentsGRPCAddr, "CHORA_PAYMENTS_HTTP_ADDR"},
	"closure":       {EnvClosureOrchestratorURL},
	"realtime":      {EnvRealtimeTicketSignerSecretID},
}

// kioskCloudOnlySettings configure the cloud's IdP, mint and Secret Manager. The kiosk has
// none of them, so a value here is a mistake, never something to ignore.
var kioskCloudOnlySettings = []string{
	"GCP_PROJECT", "IDP_ISSUER_URL", "IDP_AUDIENCE", EnvFirebaseIssuerURL, EnvFirebaseAudience,
	EnvIDPEmailAllowlistSec, EnvIDPGoogleEmailAllowlistSec, "CHORA_GATEWAY_UPSTREAM_FAKE",
	// W20: a Google ID token for the orchestrator's review resume (weakness_resume_loader.go).
	EnvAIKernelIDTokenAudience,
}

// kioskOffFlags are the cloud features the kiosk turns off by their own existing flags.
var kioskOffFlags = []string{EnvMintDisabled, EnvStripeConfigDisabled, EnvPaymentsDisabled}

// kioskProductRoutes are the product routes kiosk-web calls (API-CONTRACT.md section 1 and
// the scene ledger), as ServeMux patterns. A family "/x" with "/x/" covers its subtree.
// Adding a route is a change to this list, through the T1 track (INTERFACES.md 1).
var kioskProductRoutes = []string{
	"/api/v1/me/goals", "/api/v1/me/goals/",
	"/api/v1/me/maps", "/api/v1/me/maps/",
	"/api/v1/me/concept-graph", "/api/v1/me/concept-graph/",
	"GET /api/atoms/{atomID}",
	"/api/atoms/{atomID}/question-jobs", "/api/atoms/{atomID}/question-jobs/",
	"/api/atoms/{atomID}/session", "/api/atoms/{atomID}/session/submit",
	"/api/companion/daily-dose", "/api/companion/daily-dose/ai",
	"POST /api/companion/greeting",
	"/api/v1/me/companions/",
	"/api/v1/question-banks", "/api/v1/question-banks/",
	"/api/v1/me/question-banks",
	"/api/v1/test-sets", "/api/v1/test-sets/",
	"/api/v1/assessments", "/api/v1/assessments/",
	"/api/v1/me/assessments", "/api/v1/me/assessments/",
	"/bff/oplus/governance",
	// T3.9 (K131): the dashboard's today strip, through production's own edge reads, GET
	// only. The streak reaches a browser only through the composing dashboard read (its
	// fan-out is reads only, on identity, consumption and delivery); the mana balance is a
	// pass-through to chora-identity.
	"GET /api/me/dashboard",
	"GET /api/v1/me/mana",
	// W20 (K158): the weakness analyser. The upload door (consumption takes only the prepared
	// papers under the switch), the job poll, the review resume (the edge's own route to the
	// orchestrator) and the Growth Edge list.
	"/api/v1/me/growth-edges", "/api/v1/me/growth-edges/",
	"/healthz", "/readyz", "/health",
}

// kioskSeedRoutes are the seed tool's atom writes for the golden run (T6.2 part 2), allowed
// only with CHORA_KIOSK_SEED_ROUTES=on.
var kioskSeedRoutes = []string{
	"POST /api/atoms",
	"POST /api/atoms/{atomID}/questions",
	"POST /api/atoms/{atomID}/publish",
}

const (
	kioskConductorPrefix = "/api/v1/kiosk/"
	kioskSessionPath     = "/api/v1/kiosk/session"
	kioskPrewarmPath     = "/api/v1/kiosk/prewarm"
)

type kioskEdition struct {
	absent       []string
	secretsFile  string
	conductor    *url.URL
	seedRoutes   bool
	orchestrator string
}

// loadKioskEdition reads CHORA_EDITION once. Unset: (nil, nil), the cloud edition. "kiosk":
// the kiosk boot gate runs and every refusal wraps ErrKioskConfig. Anything else is refused.
func loadKioskEdition() (*kioskEdition, error) {
	edition := strings.TrimSpace(os.Getenv(EnvEdition))
	if edition == "" {
		return nil, nil
	}
	if edition != EditionKiosk {
		return nil, fmt.Errorf("%w: %s=%q (the only edition is %q)", ErrKioskConfig, EnvEdition, edition, EditionKiosk)
	}
	set := func(k string) bool { return strings.TrimSpace(os.Getenv(k)) != "" }
	refuse := func(format string, a ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrKioskConfig}, a...)...)
	}

	for _, k := range kioskCloudOnlySettings {
		if set(k) {
			return nil, refuse("%s is set; the kiosk has no IdP, mint or Google project", k)
		}
	}
	for _, k := range kioskOffFlags {
		if !strings.EqualFold(strings.TrimSpace(os.Getenv(k)), "true") {
			return nil, refuse("%s must be true in the kiosk edition", k)
		}
	}
	for _, k := range []string{EnvChoraSessionJWTSignerSecret, EnvChoraSessionIssuerURL, EnvChoraSessionAudienceURL,
		EnvKioskInternalSecretsFile, EnvObservabilityHTTPAddr} {
		if !set(k) {
			return nil, refuse("%s is required", k)
		}
	}

	declared := map[string]bool{}
	for _, name := range strings.Split(os.Getenv(EnvKioskAbsentUpstreams), ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := kioskAbsentUpstreams[name]; !ok {
			return nil, refuse("%s names %q, which is not an upstream the kiosk can run without", EnvKioskAbsentUpstreams, name)
		}
		declared[name] = true
	}
	if len(declared) == 0 {
		return nil, refuse("%s is required", EnvKioskAbsentUpstreams)
	}
	absent := make([]string, 0, len(kioskAbsentUpstreams))
	for name := range kioskAbsentUpstreams {
		absent = append(absent, name)
	}
	sort.Strings(absent)
	for _, name := range absent {
		if !declared[name] {
			return nil, refuse("%s must also declare %q: the kiosk runs no %s", EnvKioskAbsentUpstreams, name, name)
		}
		for _, k := range kioskAbsentUpstreams[name] {
			if set(k) {
				return nil, refuse("%s is set, but %s is declared absent", k, name)
			}
		}
	}

	for _, k := range RequiredSVCEnvVars {
		if absentSetting(k) {
			continue
		}
		v := strings.TrimSpace(os.Getenv(k))
		if v == "" {
			return nil, refuse("%s is required (its upstream runs on the laptop)", k)
		}
		if err := validateAbsHTTPURL(v); err != nil {
			return nil, refuse("%s: %v", k, err)
		}
	}

	// W20 (K158): the weakness review resumes on the orchestrator (the edge's own route,
	// weakness_resume_handler.go). Without it the visitor's confirm answers 503.
	orchestrator := strings.TrimSpace(os.Getenv(EnvAIKernelOrchestratorURL))
	if orchestrator == "" {
		return nil, refuse("%s is required (the weakness review resumes on the orchestrator)", EnvAIKernelOrchestratorURL)
	}
	if err := validateAbsHTTPURL(orchestrator); err != nil {
		return nil, refuse("%s: %v", EnvAIKernelOrchestratorURL, err)
	}

	raw := strings.TrimSpace(os.Getenv(EnvKioskConductorURL))
	if raw == "" {
		return nil, refuse("%s is required", EnvKioskConductorURL)
	}
	if err := validateAbsHTTPURL(raw); err != nil {
		return nil, refuse("%s: %v", EnvKioskConductorURL, err)
	}
	conductor, _ := url.Parse(raw)
	if conductor.Path != "" && conductor.Path != "/" {
		return nil, refuse("%s must be a bare origin, without a path", EnvKioskConductorURL)
	}
	conductor.Path = ""

	var seedRoutes bool
	switch v := strings.TrimSpace(os.Getenv(EnvKioskSeedRoutes)); v {
	case "":
	case "on":
		seedRoutes = true
	default:
		return nil, refuse("%s=%q: the only value is \"on\" (or unset)", EnvKioskSeedRoutes, v)
	}

	return &kioskEdition{
		absent:       absent,
		secretsFile:  strings.TrimSpace(os.Getenv(EnvKioskInternalSecretsFile)),
		conductor:    conductor,
		seedRoutes:   seedRoutes,
		orchestrator: orchestrator,
	}, nil
}

func absentSetting(k string) bool {
	for _, keys := range kioskAbsentUpstreams {
		for _, a := range keys {
			if a == k {
				return true
			}
		}
	}
	return false
}

// secretReader reads the session signer from the internal secrets file.
func (k *kioskEdition) secretReader() secretReader {
	return newDotenvSecretReader(k.secretsFile)
}

// bootLine is the one WARN line naming every kiosk behaviour (INTERFACES.md 2).
func (k *kioskEdition) bootLine() string {
	return fmt.Sprintf("KIOSK EDITION: session signer from internal.env (%s), absent upstreams %s, "+
		"no mint, Stripe config or payments, routes default deny (%d product patterns allowed, the rest 503), "+
		"%s* to the conductor %s (POST session open, the rest behind the session), prewarm refused at the edge, %s, "+
		"weakness review resume to %s, no ID token, no span for %s (T1.11)",
		k.secretsFile, strings.Join(k.absent, "/"), len(kioskProductRoutes), kioskConductorPrefix, k.conductor, k.seedLine(),
		k.orchestrator, strings.Join(kioskUntracedPaths, " or "))
}

// kioskUntracedPaths are the container health probes, served with no span
// under the switch (T1.11, ruled 18 Oct): the stdout exporter printed one
// per probe every few seconds and rotated the boot lines out of the logs.
var kioskUntracedPaths = []string{"/healthz", "/readyz"}

// untracedPaths is nil in the cloud edition (a nil kiosk edition), which
// traces every path.
func (k *kioskEdition) untracedPaths() []string {
	if k == nil {
		return nil
	}
	return kioskUntracedPaths
}

func (k *kioskEdition) seedLine() string {
	if !k.seedRoutes {
		return "seed routes off"
	}
	return "seed routes on (" + strings.Join(kioskSeedRoutes, ", ") + ")"
}

// routes wraps the product chain: it sits inside CORS and the security headers and in front
// of the cloud chain's JWT gate, so a refused route reaches no aggregator and no stub.
func (k *kioskEdition) routes(v *chorasession.Validator, product http.Handler) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(k.conductor)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		writeKioskError(w, r, http.StatusBadGateway, "CONDUCTOR_UNAVAILABLE", "the kiosk conductor did not answer")
	}
	gatedProxy := httpadapter.RequireChoraSessionJWT(v)(proxy)

	allowed := http.NewServeMux()
	for _, p := range kioskProductRoutes {
		allowed.Handle(p, product)
	}
	if k.seedRoutes {
		for _, p := range kioskSeedRoutes {
			allowed.Handle(p, product)
		}
	}
	allowed.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeKioskError(w, r, http.StatusServiceUnavailable, "NOT_PART_OF_KIOSK_EDITION",
			"this route is not part of the kiosk edition")
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == kioskPrewarmPath || path == kioskPrewarmPath+"/":
			writeKioskError(w, r, http.StatusForbidden, "KIOSK_OPERATOR_ONLY",
				"the pre-warm is called on the conductor's loopback, never through the edge")
		case path == kioskSessionPath && r.Method == http.MethodPost:
			proxy.ServeHTTP(w, r)
		case strings.HasPrefix(path, kioskConductorPrefix):
			gatedProxy.ServeHTTP(w, r)
		default:
			allowed.ServeHTTP(w, r)
		}
	})
}

// writeKioskError writes the gateway's error envelope.
func writeKioskError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
		"code": code, "message": message, "correlation_id": middleware.CorrelationIDFromContext(r.Context()),
	}})
}

// dotenvSecretReader is the kiosk's secretReader: KEY=value lines in one file (the install
// script's internal.env). A value is returned with surrounding whitespace trimmed, as the
// signer contract says (INTERFACES.md 3.2). Errors name the file and the key, never a value.
type dotenvSecretReader struct{ path string }

func newDotenvSecretReader(path string) secretReader { return dotenvSecretReader{path: path} }

func (d dotenvSecretReader) GetSecret(_ context.Context, name string) (string, error) {
	f, err := os.Open(d.path)
	if err != nil {
		return "", fmt.Errorf("internal secrets: open %s: %w", d.path, err)
	}
	defer f.Close()
	found, value := false, ""
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, val, ok := strings.Cut(text, "=")
		if !ok {
			return "", fmt.Errorf("internal secrets: %s line %d is not KEY=value", d.path, line)
		}
		if strings.TrimSpace(key) != name {
			continue
		}
		if found {
			return "", fmt.Errorf("internal secrets: %s holds %s twice", d.path, name)
		}
		found, value = true, strings.TrimSpace(val)
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("internal secrets: read %s: %w", d.path, err)
	}
	if !found {
		return "", fmt.Errorf("internal secrets: %s has no %s", d.path, name)
	}
	if value == "" {
		return "", fmt.Errorf("internal secrets: %s has an empty %s", d.path, name)
	}
	return value, nil
}
