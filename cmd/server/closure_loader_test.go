// closure_loader_test.go — env-contract specs for the account-closure saga
// trigger bootstrap (CHO-1719, ADR-181 D5).
package main

import (
	"context"
	"testing"
)

// testTenancyGRPCAddr is a mesh DNS name that is never dialled: grpc.NewClient
// is lazy, so the loader specs exercise the wiring without a live tenancy.
const testTenancyGRPCAddr = "chora-tenancy.tenancy.svc.cluster.local:9090"

// closeLoaderConn releases the tenancy conn a wired loader opened.
func closeLoaderConn(t *testing.T, cleanup func() error) {
	t.Helper()
	if cleanup == nil {
		t.Fatal("cleanup must be returned by a wired loader")
	}
	if err := cleanup(); err != nil {
		t.Errorf("cleanup: %v", err)
	}
}

// Absent CLOSURE_ORCHESTRATOR_URL is a DEGRADED boot, not a fatal one: the
// loader returns (nil, nil) and WithClosureRoutes answers 503
// CLOSURE_UNAVAILABLE on the closure routes.
func TestNewClosureHandlerFromEnv_UnsetURLReturnsNilHandler(t *testing.T) {
	t.Setenv(EnvClosureOrchestratorURL, "")
	h, _, err := NewClosureHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if h != nil {
		t.Fatal("handler must be nil when CLOSURE_ORCHESTRATOR_URL is unset")
	}
}

func TestNewClosureHandlerFromEnv_PlainHTTPHappyPath(t *testing.T) {
	t.Setenv(EnvClosureOrchestratorURL, "http://chora-closure-orchestrator.internal:8080")
	t.Setenv(EnvClosureGraceDays, "")
	t.Setenv(EnvTenancyGRPCAddr, testTenancyGRPCAddr)
	h, cleanup, err := NewClosureHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if h == nil {
		t.Fatal("handler must be wired when the URL is set")
	}
	closeLoaderConn(t, cleanup)
}

// A mis-set grace period must fail LOUD at boot (CrashLoopBackOff → ops fixes
// config), never silently default.
func TestNewClosureHandlerFromEnv_InvalidGraceDaysFailsLoud(t *testing.T) {
	for _, bad := range []string{"abc", "0", "-3", "366"} {
		t.Setenv(EnvClosureOrchestratorURL, "http://closure.internal:8080")
		t.Setenv(EnvClosureGraceDays, bad)
		t.Setenv(EnvTenancyGRPCAddr, testTenancyGRPCAddr)
		if _, _, err := NewClosureHandlerFromEnv(context.Background()); err == nil {
			t.Fatalf("grace days %q: want error, got nil", bad)
		}
	}
}

func TestNewClosureHandlerFromEnv_ValidGraceDays(t *testing.T) {
	t.Setenv(EnvClosureOrchestratorURL, "http://closure.internal:8080")
	t.Setenv(EnvClosureGraceDays, "14")
	t.Setenv(EnvTenancyGRPCAddr, testTenancyGRPCAddr)
	h, cleanup, err := NewClosureHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if h == nil {
		t.Fatal("handler must be wired")
	}
	closeLoaderConn(t, cleanup)
}

// --- ownership pre-flight dependency (E3, first-launch spec 13.7.2) ---------

// The closure routes cannot uphold the ownership guarantee without a tenancy
// membership read. Configuring closure WITHOUT it is a deployment error, so it
// must fail LOUD at boot rather than serve a close route that silently skips
// the check. Both env vars are set on the live gateway deployment.
func TestNewClosureHandlerFromEnv_MissingTenancyAddrFailsLoud(t *testing.T) {
	t.Setenv(EnvClosureOrchestratorURL, "http://closure.internal:8080")
	t.Setenv(EnvClosureGraceDays, "")
	t.Setenv(EnvTenancyGRPCAddr, "")

	h, cleanup, err := NewClosureHandlerFromEnv(context.Background())
	if err == nil {
		t.Fatal("want a boot error when CHORA_TENANCY_GRPC_ADDR is unset while closure is configured")
	}
	if h != nil {
		t.Error("handler must not be returned alongside the error")
	}
	if cleanup != nil {
		if cerr := cleanup(); cerr != nil {
			t.Errorf("cleanup: %v", cerr)
		}
	}
}

// An unset orchestrator URL still degrades rather than failing, and it must not
// require the tenancy address to do so: all four routes answer 503.
func TestNewClosureHandlerFromEnv_UnsetURLDegradesEvenWithoutTenancyAddr(t *testing.T) {
	t.Setenv(EnvClosureOrchestratorURL, "")
	t.Setenv(EnvTenancyGRPCAddr, "")

	h, cleanup, err := NewClosureHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if h != nil {
		t.Fatal("handler must be nil when CLOSURE_ORCHESTRATOR_URL is unset")
	}
	if cleanup != nil {
		if cerr := cleanup(); cerr != nil {
			t.Errorf("cleanup: %v", cerr)
		}
	}
}

func TestNewClosureHandlerFromEnv_WiresOwnershipAndReturnsCleanup(t *testing.T) {
	t.Setenv(EnvClosureOrchestratorURL, "http://closure.internal:8080")
	t.Setenv(EnvClosureGraceDays, "")
	t.Setenv(EnvTenancyGRPCAddr, "chora-tenancy.tenancy.svc.cluster.local:9090")

	h, cleanup, err := NewClosureHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if h == nil {
		t.Fatal("handler must be wired when both the URL and the tenancy addr are set")
	}
	if cleanup == nil {
		t.Fatal("cleanup must be returned so the gRPC conn is closed on shutdown")
	}
	if cerr := cleanup(); cerr != nil {
		t.Errorf("cleanup: %v", cerr)
	}
}
