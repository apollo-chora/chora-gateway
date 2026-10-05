// tenancy_admin_loader_test.go — TDD specs for the ADR-217 Phase 2.2 (CHO-2008)
// operator sub-tenant route env wiring. The real-dial path is covered by the
// handler/client tests; here we pin the two graceful-degrade branches (the
// additive-surface posture that must NOT brick the gateway).
package main

import (
	"context"
	"testing"
)

func TestNewTenancyAdminHandlerFromEnv_Disabled(t *testing.T) {
	t.Setenv(EnvTenancyAdminDisabled, "true")
	t.Setenv(EnvTenancyGRPCAddr, "chora-tenancy.tenancy.svc.cluster.local:9090")
	h, cleanup, err := NewTenancyAdminHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want nil (disabled degrades)", err)
	}
	if h != nil {
		t.Error("handler must be nil when disabled")
	}
	if cleanup == nil {
		t.Fatal("cleanup must be non-nil (noop) even when disabled")
	}
	if err := cleanup(); err != nil {
		t.Errorf("noop cleanup err = %v", err)
	}
}

func TestNewTenancyAdminHandlerFromEnv_AddrUnset_Degrades(t *testing.T) {
	t.Setenv(EnvTenancyAdminDisabled, "")
	t.Setenv(EnvTenancyGRPCAddr, "")
	h, cleanup, err := NewTenancyAdminHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want nil (unset addr degrades, does not brick gateway)", err)
	}
	if h != nil {
		t.Error("handler must be nil when CHORA_TENANCY_GRPC_ADDR is unset")
	}
	if cleanup == nil {
		t.Fatal("cleanup must be non-nil (noop)")
	}
}
