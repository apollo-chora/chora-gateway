// oplus_loader_test.go — smoke unit tests for the env-driven O+ BFF
// handler bootstrap.
//
// Verifies:
//   - fake-mode (CHORA_GATEWAY_UPSTREAM_FAKE=true): loader returns a non-nil
//     handler + nil-cleanup-safe + nil-resolver-safe + nil error.
//   - real mode with all 3 required envs set: loader returns a non-nil
//     handler + closeable cleanup + role-resolver + nil error.
//   - real mode with ANY required env empty: loader returns
//     ErrOPlusBackendMissing with the missing var name.
//
// Per `feedback_no_local_cicd_run`: tests gate compile + intent only;
// Cloud Build is the test execution venue.
package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// clearOPlusEnvs blanks every Phase-C env var so each subtest starts from a
// clean slate.
func clearOPlusEnvs(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		EnvGovernanceGRPCAddr,
		EnvGovernanceHTTPAddr,
		EnvObservabilityHTTPAddr,
		EnvA2AHTTPAddr,
		EnvGatewayUpstreamFake,
		EnvGatewayDataLineagePath,
	} {
		t.Setenv(k, "")
	}
}

// setAllOPlusBackendEnvs populates the 3 required Phase-C backend envs with
// realistic mesh-internal placeholders so happy-path tests can exercise the
// real-wire branch without dialling a live backend.
func setAllOPlusBackendEnvs(t *testing.T) {
	t.Helper()
	t.Setenv(EnvGovernanceGRPCAddr, "chora-governance.governance.svc.cluster.local:9090")
	t.Setenv(EnvGovernanceHTTPAddr, "http://chora-governance.governance.svc.cluster.local:8080")
	t.Setenv(EnvObservabilityHTTPAddr, "http://chora-observability.observability.svc.cluster.local:8080")
	// A2A intentionally left empty — pending mode is canonical.
	t.Setenv(EnvA2AHTTPAddr, "")
}

// TestNewOPlusHandlerFromEnv_FakeModeSucceeds verifies the smoke fallback —
// when CHORA_GATEWAY_UPSTREAM_FAKE=true the loader skips all dial logic and
// returns a working handler with nil clients (each call emits an error
// envelope from the nil-safe code paths in handlers_oplus.go).
func TestNewOPlusHandlerFromEnv_FakeModeSucceeds(t *testing.T) {
	clearOPlusEnvs(t)
	t.Setenv(EnvGatewayUpstreamFake, "true")

	h, cleanup, resolver, err := NewOPlusHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("fake-mode loader returned err: %v", err)
	}
	if h == nil {
		t.Fatal("fake-mode loader returned nil handler")
	}
	if cleanup == nil {
		t.Error("fake-mode loader returned nil cleanup")
	} else if err := cleanup(); err != nil {
		t.Errorf("fake-mode cleanup err: %v", err)
	}
	if resolver == nil {
		t.Error("fake-mode loader returned nil resolver")
	}
}

// TestNewOPlusHandlerFromEnv_RealModeSucceedsWithAllEnvs verifies the real-
// wire happy path. Note: this DOES create a gRPC channel via grpc.NewClient
// but does NOT dial — grpc.NewClient is lazy. The conn is closed via cleanup.
func TestNewOPlusHandlerFromEnv_RealModeSucceedsWithAllEnvs(t *testing.T) {
	clearOPlusEnvs(t)
	setAllOPlusBackendEnvs(t)

	h, cleanup, resolver, err := NewOPlusHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("real-mode loader err: %v", err)
	}
	if h == nil {
		t.Fatal("real-mode loader returned nil handler")
	}
	if resolver == nil {
		t.Error("real-mode loader returned nil resolver")
	}
	if cleanup == nil {
		t.Fatal("real-mode loader returned nil cleanup")
	}
	// Close the underlying gRPC conn so the test doesn't leak.
	if err := cleanup(); err != nil {
		t.Errorf("real-mode cleanup err: %v", err)
	}
}

// TestNewOPlusHandlerFromEnv_FailsLoudOnMissingRequiredEnvs verifies the
// fail-loud branch — every required backend env (governance gRPC, governance
// HTTP, observability HTTP) MUST surface ErrOPlusBackendMissing with the
// missing var name in the error string. A2A is NOT in this list because
// empty A2A addr is the explicit pending-deployment signal.
func TestNewOPlusHandlerFromEnv_FailsLoudOnMissingRequiredEnvs(t *testing.T) {
	requiredEnvs := []string{
		EnvGovernanceGRPCAddr,
		EnvGovernanceHTTPAddr,
		EnvObservabilityHTTPAddr,
	}
	for _, missing := range requiredEnvs {
		missing := missing
		t.Run(missing, func(t *testing.T) {
			clearOPlusEnvs(t)
			setAllOPlusBackendEnvs(t)
			t.Setenv(missing, "")

			h, cleanup, resolver, err := NewOPlusHandlerFromEnv(context.Background())
			if err == nil {
				t.Fatalf("loader with %s blank: nil err", missing)
			}
			if !errors.Is(err, ErrOPlusBackendMissing) {
				t.Errorf("err should wrap ErrOPlusBackendMissing; got %v", err)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("err should name %s; got %q", missing, err)
			}
			if h != nil {
				t.Errorf("expected nil handler on error; got %T", h)
			}
			if resolver != nil {
				t.Errorf("expected nil resolver on error; got non-nil")
			}
			if cleanup != nil {
				// Even on error the loader hands back a callable cleanup
				// so caller defer is safe.
				_ = cleanup()
			}
		})
	}
}

// TestNewOPlusHandlerFromEnv_A2AEmptyAddrIsLegal verifies the pending-mode
// signal — empty CHORA_A2A_HTTP_ADDR does NOT fail the loader. The handler
// renders {mode:'pending', mock:{...}} per Phase C plan §B6.
func TestNewOPlusHandlerFromEnv_A2AEmptyAddrIsLegal(t *testing.T) {
	clearOPlusEnvs(t)
	setAllOPlusBackendEnvs(t)
	t.Setenv(EnvA2AHTTPAddr, "") // explicit

	h, cleanup, _, err := NewOPlusHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("loader with empty A2A addr: %v", err)
	}
	if h == nil {
		t.Fatal("nil handler with empty A2A addr — pending mode should still construct")
	}
	if cleanup != nil {
		_ = cleanup()
	}
}

// TestNewOPlusHandlerFromEnv_FakeModeIgnoresMissingBackendEnvs verifies that
// when fake-mode is enabled, missing backend envs are silently ignored —
// the loader does NOT consult them.
func TestNewOPlusHandlerFromEnv_FakeModeIgnoresMissingBackendEnvs(t *testing.T) {
	clearOPlusEnvs(t)
	t.Setenv(EnvGatewayUpstreamFake, "true")
	// Intentionally leave EnvGovernanceGRPCAddr / EnvGovernanceHTTPAddr /
	// EnvObservabilityHTTPAddr unset — fake-mode skips the gate.

	h, cleanup, _, err := NewOPlusHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("fake-mode loader err with missing backends: %v", err)
	}
	if h == nil {
		t.Fatal("fake-mode handler should be non-nil")
	}
	if cleanup != nil {
		_ = cleanup()
	}
}
