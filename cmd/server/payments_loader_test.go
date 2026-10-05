// payments_loader_test.go — TDD specs for the env-driven chora-payments
// gRPC client bootstrap (Stage B of ADR-164).
//
// We focus on the env-gate behaviour (the only branch testable without a
// live gRPC backend). The downstream gRPC dial uses `grpc.NewClient` which
// is lazy (no network IO at dial time) so a syntactically valid address is
// accepted at this layer; the actual upstream-not-reachable failure is the
// caller's runtime concern.
package main

import (
	"context"
	"errors"
	"testing"
)

func TestNewPaymentsCheckoutHandlerFromEnv_DisabledReturnsNilHandler(t *testing.T) {
	t.Setenv(EnvPaymentsDisabled, "true")
	t.Setenv(EnvPaymentsGRPCAddr, "")

	h, cleanup, err := NewPaymentsCheckoutHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h != nil {
		t.Error("expected nil handler when disabled")
	}
	if cleanup == nil {
		t.Error("expected non-nil cleanup")
	}
	if err := cleanup(); err != nil {
		t.Errorf("cleanup returned error: %v", err)
	}
}

func TestNewPaymentsCheckoutHandlerFromEnv_MissingAddrFailsLoud(t *testing.T) {
	t.Setenv(EnvPaymentsDisabled, "")
	t.Setenv(EnvPaymentsGRPCAddr, "")

	_, _, err := NewPaymentsCheckoutHandlerFromEnv(context.Background())
	if err == nil {
		t.Fatal("expected error when CHORA_PAYMENTS_GRPC_ADDR is empty")
	}
	if !errors.Is(err, ErrPaymentsAddrMissing) {
		t.Errorf("err should wrap ErrPaymentsAddrMissing; got %v", err)
	}
}

func TestNewPaymentsCheckoutHandlerFromEnv_HappyPath(t *testing.T) {
	t.Setenv(EnvPaymentsDisabled, "")
	t.Setenv(EnvPaymentsGRPCAddr, "chora-payments.payments.svc.cluster.local:9090")

	h, cleanup, err := NewPaymentsCheckoutHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h == nil {
		t.Fatal("expected non-nil handler")
	}
	if cleanup == nil {
		t.Fatal("expected non-nil cleanup")
	}
	if err := cleanup(); err != nil {
		t.Errorf("cleanup returned error: %v", err)
	}
}

func TestNewPaymentsCheckoutHandlerFromEnv_LocalhostAddr(t *testing.T) {
	t.Setenv(EnvPaymentsDisabled, "")
	t.Setenv(EnvPaymentsGRPCAddr, "localhost:9090")

	h, cleanup, err := NewPaymentsCheckoutHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h == nil {
		t.Fatal("expected non-nil handler")
	}
	_ = cleanup()
}
