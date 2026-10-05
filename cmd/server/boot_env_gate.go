// boot_env_gate.go — fail-loud boot-time env-var validation for chora-gateway.
//
// Per the 2026-05-14 arch-gap directive (Bucket 4): the gateway MUST EXIT 1
// with a clear log on any missing required env var. There is no silent
// fallback. Auth is non-negotiable in prod posture per the user's "fail
// loudly" directive — IDP_JWT_DISABLED was removed in the same commit.
//
// CheckBootEnv is called from main() BEFORE any handler is registered.
// On failure, main() log.Fatalfs the wrapped error so ops sees the missing
// var name in pod logs.
//
// Env contract (every key MUST be non-blank):
//
//	CHORA_SESSION_SIGNER              — HS256 session signing key (shared
//	                                     with chora-identity)
//	CHORA_SESSION_ISSUER              — chora-session iss claim
//	CHORA_SESSION_AUDIENCE            — chora-session aud claim
//	SVC_TENANCY_URL                   — chora-tenancy base URL
//	SVC_CREATION_URL                  — chora-creation base URL
//	SVC_CONSUMPTION_URL               — chora-consumption base URL
//	SVC_SHARING_URL                   — chora-sharing base URL
//	SVC_DELIVERY_URL                  — chora-delivery base URL
//	SVC_GOVERNANCE_URL                — chora-governance base URL
//	SVC_OBSERVABILITY_URL             — chora-observability base URL
//	SVC_NOTIFICATIONS_URL             — chora-notifications base URL
//
// chora-identity is addressed via CHORA_IDENTITY_URL (default
// http://identity:8080), so it is not a required scalar.
//
// SVC_* URLs are additionally parsed with net/url and rejected unless they
// resolve to an absolute http(s) URL with a non-empty host.
package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// ErrBootEnvMissing is the package sentinel for missing-env-var failures.
// Tests use errors.Is to match without parsing strings.
var ErrBootEnvMissing = errors.New("boot env: required var missing")

// RequiredScalarEnvVars is the canonical list of scalar (non-URL) env vars
// the boot-time gate must enforce. Order matters for log readability.
var RequiredScalarEnvVars = []string{
	"CHORA_SESSION_SIGNER",
	"CHORA_SESSION_ISSUER",
	"CHORA_SESSION_AUDIENCE",
}

// RequiredSVCEnvVars is the canonical list of upstream SVC_*_URL env vars
// that must be non-empty AND parse as absolute http(s) URLs. SVC_IDENTITY_URL
// is intentionally absent — chora-identity is addressed via
// CHORA_IDENTITY_URL (with a local default).
var RequiredSVCEnvVars = []string{
	"SVC_TENANCY_URL",
	"SVC_CREATION_URL",
	"SVC_CONSUMPTION_URL",
	"SVC_SHARING_URL",
	"SVC_DELIVERY_URL",
	"SVC_GOVERNANCE_URL",
	"SVC_OBSERVABILITY_URL",
	"SVC_NOTIFICATIONS_URL",
}

// CheckBootEnv runs the fail-loud env-var gate at process boot. Returns
// the FIRST missing/malformed env var as a wrapped ErrBootEnvMissing — ops
// fixes one at a time so the first miss is enough to surface.
//
// Caller pattern in main():
//
//	if err := CheckBootEnv(); err != nil {
//		log.Fatalf("boot env gate failed: %v", err)
//	}
//
// Note: this does NOT mutate state — it is purely a read of os.Getenv.
func CheckBootEnv() error {
	for _, k := range RequiredScalarEnvVars {
		if strings.TrimSpace(os.Getenv(k)) == "" {
			return fmt.Errorf("%w: %s", ErrBootEnvMissing, k)
		}
	}
	for _, k := range RequiredSVCEnvVars {
		v := strings.TrimSpace(os.Getenv(k))
		if v == "" {
			return fmt.Errorf("%w: %s", ErrBootEnvMissing, k)
		}
		if err := validateAbsHTTPURL(v); err != nil {
			return fmt.Errorf("boot env %s: %w", k, err)
		}
	}
	return nil
}

// validateAbsHTTPURL returns nil iff `v` parses as an absolute URL with
// scheme=http|https and a non-empty host.
func validateAbsHTTPURL(v string) error {
	u, err := url.Parse(v)
	if err != nil {
		return fmt.Errorf("parse url: %v", err)
	}
	if !u.IsAbs() {
		return fmt.Errorf("not an absolute url: %q", v)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported scheme %q (want http or https)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("empty host in url: %q", v)
	}
	return nil
}

// EnvSourceProject names the deployment/logical project. It replaced the
// cloud-specific cloud-project env vars.
const EnvSourceProject = "CHORA_SOURCE_PROJECT"

// DefaultSourceProject is the local-stack default when CHORA_SOURCE_PROJECT
// is unset.
const DefaultSourceProject = "chora-local"

// resolveSourceProject returns CHORA_SOURCE_PROJECT, falling back to the
// local default.
func resolveSourceProject() string {
	if v := strings.TrimSpace(os.Getenv(EnvSourceProject)); v != "" {
		return v
	}
	return DefaultSourceProject
}
