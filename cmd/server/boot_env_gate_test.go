// boot_env_gate_test.go — specs for the boot-time fail-loud env var gate that
// runs BEFORE any handler registration in chora-gateway.
//
// Required envs:
//
//	CHORA_SESSION_SIGNER   — chora-session HS256 signer (shared secret)
//	CHORA_SESSION_ISSUER   — chora-session iss claim
//	CHORA_SESSION_AUDIENCE — chora-session aud claim
//	SVC_*_URL × 8          — upstream service URLs, each must parse as an
//	                         absolute http(s) URL (chora-identity is addressed
//	                         via CHORA_IDENTITY_URL with a local default).
package main

import (
	"errors"
	"strings"
	"testing"
)

// allSvcEnvVars mirrors RequiredSVCEnvVars (source of truth in boot_env_gate.go).
var allSvcEnvVars = []string{
	"SVC_TENANCY_URL",
	"SVC_CREATION_URL",
	"SVC_CONSUMPTION_URL",
	"SVC_SHARING_URL",
	"SVC_DELIVERY_URL",
	"SVC_GOVERNANCE_URL",
	"SVC_OBSERVABILITY_URL",
	"SVC_NOTIFICATIONS_URL",
}

// allChoraSessionEnvVars is the chora-session validator env contract.
var allChoraSessionEnvVars = []string{
	"CHORA_SESSION_SIGNER",
	"CHORA_SESSION_ISSUER",
	"CHORA_SESSION_AUDIENCE",
}

// allRequiredEnvVars combines every required env var the boot-time gate must
// enforce.
func allRequiredEnvVars() []string {
	out := append([]string{}, allChoraSessionEnvVars...)
	out = append(out, allSvcEnvVars...)
	return out
}

// setAllRequiredEnv populates every required env var with a non-empty
// placeholder.
func setAllRequiredEnv(t *testing.T) {
	t.Helper()
	defaults := map[string]string{
		"CHORA_SESSION_SIGNER":   "test-mint-session-signer-0123456789abcdef",
		"CHORA_SESSION_ISSUER":   "https://api.chora.site",
		"CHORA_SESSION_AUDIENCE": "chora-local",
		"SVC_TENANCY_URL":        "http://chora-tenancy.svc.cluster.local:8080",
		"SVC_CREATION_URL":       "http://chora-creation.svc.cluster.local:8080",
		"SVC_CONSUMPTION_URL":    "http://chora-consumption.svc.cluster.local:8080",
		"SVC_SHARING_URL":        "http://chora-sharing.svc.cluster.local:8080",
		"SVC_DELIVERY_URL":       "http://chora-delivery.svc.cluster.local:8080",
		"SVC_GOVERNANCE_URL":     "http://chora-governance.svc.cluster.local:8080",
		"SVC_OBSERVABILITY_URL":  "http://chora-observability.svc.cluster.local:8080",
		"SVC_NOTIFICATIONS_URL":  "http://chora-notifications.svc.cluster.local:8080",
	}
	for k, v := range defaults {
		t.Setenv(k, v)
	}
}

func TestCheckBootEnv_AllPresent(t *testing.T) {
	setAllRequiredEnv(t)
	if err := CheckBootEnv(); err != nil {
		t.Fatalf("CheckBootEnv with all required envs set: %v", err)
	}
}

func TestCheckBootEnv_FailsLoudOnMissingScalars(t *testing.T) {
	for _, k := range allRequiredEnvVars() {
		k := k
		t.Run(k, func(t *testing.T) {
			setAllRequiredEnv(t)
			t.Setenv(k, "")
			err := CheckBootEnv()
			if err == nil {
				t.Fatalf("CheckBootEnv with %s blank: nil err", k)
			}
			if !strings.Contains(err.Error(), k) {
				t.Errorf("err should mention %s; got %q", k, err)
			}
			if !errors.Is(err, ErrBootEnvMissing) {
				t.Errorf("err should wrap ErrBootEnvMissing; got %v", err)
			}
		})
	}
}

func TestCheckBootEnv_RejectsMalformedSVCURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"relative_path", "/chora-tenancy"},
		{"missing_scheme", "chora-tenancy.svc.cluster.local:8080"},
		{"unsupported_scheme", "ftp://chora-tenancy.svc.cluster.local"},
		{"only_scheme", "http://"},
		{"garbage", "not a url"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			setAllRequiredEnv(t)
			t.Setenv("SVC_TENANCY_URL", c.url)
			err := CheckBootEnv()
			if err == nil {
				t.Fatalf("CheckBootEnv with SVC_TENANCY_URL=%q: nil err", c.url)
			}
			if !strings.Contains(err.Error(), "SVC_TENANCY_URL") {
				t.Errorf("err should mention SVC_TENANCY_URL; got %q", err)
			}
		})
	}
}

func TestCheckBootEnv_AcceptsHTTPSURLs(t *testing.T) {
	setAllRequiredEnv(t)
	t.Setenv("SVC_TENANCY_URL", "https://chora-tenancy.chora.site")
	if err := CheckBootEnv(); err != nil {
		t.Fatalf("CheckBootEnv with https URL: %v", err)
	}
}

func TestRequiredEnvVars_HasNoDuplicates(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range allRequiredEnvVars() {
		if seen[k] {
			t.Errorf("duplicate env var in required list: %s", k)
		}
		seen[k] = true
	}
}
