// loader_coverage_test.go — env-gate coverage for the cmd/server loaders
// whose fail-loud / disabled / degrade branches are not pinned elsewhere.
//
//	transaction_history  — disabled + addr-unset degrade + lazy happy path
//	tenancy_admin        — lazy happy path
//	weakness_resume      — unset-URL degrade + plain-HTTP happy path
//	realtime_ticket      — SECRET_ID-set-but-unresolvable + literal fallback
//	payments             — malformed CHORA_MANA_PACKS fail-loud
//	stripe_config        — disabled + missing-key fail-loud + happy path
//
// grpc.NewClient is lazy (no network IO at dial time), so the gRPC-dial
// loaders' happy paths run against an arbitrary address.
package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
)

// ---------------------------------------------------------------------------
// stripe_config_loader.go
// ---------------------------------------------------------------------------

func TestNewStripeConfigHandlerFromEnv_DisabledReturnsNil(t *testing.T) {
	t.Setenv(EnvStripeConfigDisabled, "true")
	t.Setenv(EnvStripePublishableKeySecret, "")
	h, cleanup, err := NewStripeConfigHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h != nil {
		t.Error("expected nil handler when STRIPE_CONFIG_DISABLED=true")
	}
	if cleanup == nil {
		t.Error("expected non-nil cleanup")
	}
	_ = cleanup()
}

func TestNewStripeConfigHandlerFromEnv_MissingKeyFailsLoud(t *testing.T) {
	t.Setenv(EnvStripeConfigDisabled, "")
	t.Setenv(EnvStripePublishableKeySecret, "")
	h, _, err := NewStripeConfigHandlerFromEnv(context.Background())
	if err == nil {
		t.Fatal("expected error when publishable-key secret id blank")
	}
	if !errors.Is(err, ErrStripeConfigMissing) {
		t.Errorf("err should wrap ErrStripeConfigMissing; got %v", err)
	}
	if h != nil {
		t.Error("expected nil handler on error")
	}
}

func TestNewStripeConfigHandlerFromEnv_HappyPath(t *testing.T) {
	t.Setenv(EnvStripeConfigDisabled, "")
	t.Setenv(EnvStripePublishableKeySecret, "chora-stripe-publishable-key")
	t.Setenv("SECRET_CHORA_STRIPE_PUBLISHABLE_KEY", "pk_test_123")
	h, cleanup, err := NewStripeConfigHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h == nil {
		t.Fatal("expected non-nil handler")
	}
	_ = cleanup()
}

func TestParseSecondsOrDefault_EnvAndFallbacks(t *testing.T) {
	t.Setenv(EnvStripeConfigCacheTTL, "120")
	if got := parseSecondsOrDefault(EnvStripeConfigCacheTTL, 5*time.Minute); got != 120*time.Second {
		t.Errorf("env 120 → %v; want 120s", got)
	}
	for _, bad := range []string{"", "abc", "0"} {
		t.Setenv(EnvStripeConfigCacheTTL, bad)
		if got := parseSecondsOrDefault(EnvStripeConfigCacheTTL, 5*time.Minute); got != 5*time.Minute {
			t.Errorf("env %q → %v; want default 5m", bad, got)
		}
	}
}

// ---------------------------------------------------------------------------
// transaction_history_loader.go
// ---------------------------------------------------------------------------

func TestNewTransactionHistoryHandlerFromEnv_DisabledDegrades(t *testing.T) {
	t.Setenv(EnvTransactionHistoryDisabled, "true")
	t.Setenv(EnvTenancyGRPCAddr, "")
	h, cleanup, err := NewTransactionHistoryHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want nil (disabled degrades)", err)
	}
	if h != nil {
		t.Error("handler must be nil when disabled")
	}
	if cleanup == nil {
		t.Fatal("cleanup must be non-nil (noop)")
	}
	if err := cleanup(); err != nil {
		t.Errorf("noop cleanup err = %v", err)
	}
}

func TestNewTransactionHistoryHandlerFromEnv_AddrUnsetDegrades(t *testing.T) {
	t.Setenv(EnvTransactionHistoryDisabled, "")
	t.Setenv(EnvTenancyGRPCAddr, "")
	h, cleanup, err := NewTransactionHistoryHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want nil (unset addr degrades)", err)
	}
	if h != nil {
		t.Error("handler must be nil when CHORA_TENANCY_GRPC_ADDR unset")
	}
	if cleanup == nil {
		t.Fatal("cleanup must be non-nil (noop)")
	}
}

func TestNewTransactionHistoryHandlerFromEnv_HappyPath(t *testing.T) {
	t.Setenv(EnvTransactionHistoryDisabled, "")
	t.Setenv(EnvTenancyGRPCAddr, "localhost:9090")
	h, cleanup, err := NewTransactionHistoryHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h == nil {
		t.Fatal("expected non-nil handler")
	}
	if cleanup == nil {
		t.Fatal("expected non-nil cleanup closing the gRPC conn")
	}
	if err := cleanup(); err != nil {
		t.Errorf("cleanup err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// tenancy_admin_loader.go
// ---------------------------------------------------------------------------

func TestNewTenancyAdminHandlerFromEnv_HappyPath(t *testing.T) {
	t.Setenv(EnvTenancyAdminDisabled, "")
	t.Setenv(EnvTenancyGRPCAddr, "localhost:9090")
	h, cleanup, err := NewTenancyAdminHandlerFromEnv(context.Background())
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
		t.Errorf("cleanup err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// weakness_resume_loader.go
// ---------------------------------------------------------------------------

func TestNewWeaknessResumeHandlerFromEnv_UnsetURLDegrades(t *testing.T) {
	t.Setenv(EnvAIKernelOrchestratorURL, "")
	h, err := NewWeaknessResumeHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want nil (unset URL degrades)", err)
	}
	if h != nil {
		t.Error("handler must be nil when AI_KERNEL_ORCHESTRATOR_URL unset")
	}
}

func TestNewWeaknessResumeHandlerFromEnv_PlainHTTPHappyPath(t *testing.T) {
	t.Setenv(EnvAIKernelOrchestratorURL, "http://chora-ai-kernel-orchestrator.internal:8080")
	h, err := NewWeaknessResumeHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if h == nil {
		t.Fatal("handler must be wired when the URL is set (plain HTTP)")
	}
}

// ---------------------------------------------------------------------------
// realtime_ticket_loader.go
// ---------------------------------------------------------------------------

func TestNewRealtimeTicketMinterFromEnvSM_UnresolvableSecretFailsLoud(t *testing.T) {
	t.Setenv(EnvRealtimeTicketSignerSecretID, "chora-realtime-ticket-signer")
	t.Setenv("SECRET_CHORA_REALTIME_TICKET_SIGNER", "")
	t.Setenv("CHORA_REALTIME_TICKET_SIGNER", "")
	m, cleanup, err := NewRealtimeTicketMinterFromEnvSM(context.Background())
	if err == nil {
		t.Fatal("expected error when the signer secret cannot be resolved from env")
	}
	if !strings.Contains(err.Error(), "chora-realtime-ticket-signer") {
		t.Errorf("err should name the secret; got %v", err)
	}
	if m != nil {
		t.Error("expected nil minter on error")
	}
	if cleanup == nil {
		t.Error("expected non-nil noop cleanup")
	} else if err := cleanup(); err != nil {
		t.Errorf("noop cleanup err = %v", err)
	}
}

func TestNewRealtimeTicketMinterFromEnvSM_LiteralEnvFallback(t *testing.T) {
	t.Setenv(EnvRealtimeTicketSignerSecretID, "")
	t.Setenv(httpadapter.EnvRealtimeTicketSigner, strings.Repeat("k", 32))
	m, cleanup, err := NewRealtimeTicketMinterFromEnvSM(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m == nil {
		t.Fatal("literal-env signer must construct a minter")
	}
	if cleanup != nil {
		_ = cleanup()
	}
}

// ---------------------------------------------------------------------------
// payments_loader.go
// ---------------------------------------------------------------------------

func TestNewPaymentsCheckoutHandlerFromEnv_MalformedManaPacksFailsLoud(t *testing.T) {
	t.Setenv(EnvPaymentsDisabled, "")
	t.Setenv(EnvPaymentsGRPCAddr, "localhost:9090")
	t.Setenv(EnvManaPacks, "not-json")
	h, cleanup, err := NewPaymentsCheckoutHandlerFromEnv(context.Background())
	if err == nil {
		t.Fatal("expected error when CHORA_MANA_PACKS is malformed")
	}
	if !strings.Contains(err.Error(), "mana packs") {
		t.Errorf("err should mention mana packs; got %v", err)
	}
	if h != nil {
		t.Error("expected nil handler on error")
	}
	if cleanup != nil {
		_ = cleanup()
	}
}

func TestNewPaymentsCheckoutHandlerFromEnv_ValidManaPacks(t *testing.T) {
	t.Setenv(EnvPaymentsDisabled, "")
	t.Setenv(EnvPaymentsGRPCAddr, "localhost:9090")
	t.Setenv(EnvManaPacks, `[{"sku":"mana_pack_1000","mana_units":1000,"amount_cents":199,"currency":"USD"}]`)
	h, cleanup, err := NewPaymentsCheckoutHandlerFromEnv(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h == nil {
		t.Fatal("expected non-nil handler")
	}
	_ = cleanup()
}

// ---------------------------------------------------------------------------
// main.go pure helpers + request shims
// ---------------------------------------------------------------------------

func TestValidateAbsHTTPURL_UnparseableURL(t *testing.T) {
	for _, bad := range []string{"http://[::1", "http://exa mple.com"} {
		if err := validateAbsHTTPURL(bad); err == nil {
			t.Errorf("validateAbsHTTPURL(%q): want parse error, got nil", bad)
		}
	}
}

func TestResolveSourceProject(t *testing.T) {
	t.Setenv(EnvSourceProject, "custom-project")
	if got := resolveSourceProject(); got != "custom-project" {
		t.Errorf("got %q want custom-project", got)
	}
	t.Setenv(EnvSourceProject, "")
	if got := resolveSourceProject(); got != DefaultSourceProject {
		t.Errorf("got %q want %q", got, DefaultSourceProject)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty(); got != "" {
		t.Errorf("no args → %q, want empty", got)
	}
	if got := firstNonEmpty("  ", "x"); got != "x" {
		t.Errorf("whitespace first → %q, want x", got)
	}
	if got := firstNonEmpty("a", "b"); got != "a" {
		t.Errorf("a,b → %q, want a", got)
	}
	if got := firstNonEmpty("  ", "  "); got != "" {
		t.Errorf("all blank → %q, want empty", got)
	}
}

func TestClaimShims_NilRequestReturnsFalse(t *testing.T) {
	if c, ok := choraSessionClaimsFromRequest(nil); c != nil || ok {
		t.Errorf("choraSessionClaimsFromRequest(nil) = (%v, %v), want (nil, false)", c, ok)
	}
	if c, ok := meshClaimsFromRequest(nil); c != nil || ok {
		t.Errorf("meshClaimsFromRequest(nil) = (%v, %v), want (nil, false)", c, ok)
	}
}

func TestClaimShims_RequestWithoutClaimsReturnsFalse(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://gateway.internal/v1/x", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if c, ok := choraSessionClaimsFromRequest(req); c != nil || ok {
		t.Errorf("choraSessionClaimsFromRequest(no-claims) = (%v, %v), want (nil, false)", c, ok)
	}
	if c, ok := meshClaimsFromRequest(req); c != nil || ok {
		t.Errorf("meshClaimsFromRequest(no-claims) = (%v, %v), want (nil, false)", c, ok)
	}
}
