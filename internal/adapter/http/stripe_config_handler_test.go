package httpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- StubSecretReader -------------------------------------------------

type stubSecretReader struct {
	mu      sync.Mutex
	values  map[string]string
	calls   int32
	errOnce error
	err     error
}

func (s *stubSecretReader) GetSecret(_ context.Context, name string) (string, error) {
	atomic.AddInt32(&s.calls, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errOnce != nil {
		err := s.errOnce
		s.errOnce = nil
		return "", err
	}
	if s.err != nil {
		return "", s.err
	}
	v, ok := s.values[name]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}

func (s *stubSecretReader) callCount() int { return int(atomic.LoadInt32(&s.calls)) }

// ---------- NewStripeConfigHandler validation --------------------------------

func TestNewStripeConfigHandler_RequiresSecrets(t *testing.T) {
	_, err := NewStripeConfigHandler(StripeConfigHandlerConfig{
		PublishableKeyName: "chora-stripe-publishable-key",
	})
	if err == nil {
		t.Fatal("want error when Secrets nil")
	}
}

func TestNewStripeConfigHandler_RequiresSecretName(t *testing.T) {
	_, err := NewStripeConfigHandler(StripeConfigHandlerConfig{
		Secrets: &stubSecretReader{},
	})
	if err == nil {
		t.Fatal("want error when PublishableKeyName empty")
	}
}

func TestNewStripeConfigHandler_AppliesDefaults(t *testing.T) {
	h, err := NewStripeConfigHandler(StripeConfigHandlerConfig{
		Secrets:            &stubSecretReader{},
		PublishableKeyName: "chora-stripe-publishable-key",
		// CacheTTL omitted on purpose
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if h.cacheTTL != 5*time.Minute {
		t.Errorf("cacheTTL default: got %v want 5m", h.cacheTTL)
	}
	if h.now == nil {
		t.Error("now should be set to default")
	}
}

func TestNewStripeConfigHandler_ClampsTinyCacheTTLToDefault(t *testing.T) {
	h, err := NewStripeConfigHandler(StripeConfigHandlerConfig{
		Secrets:            &stubSecretReader{},
		PublishableKeyName: "chora-stripe-publishable-key",
		CacheTTL:           50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if h.cacheTTL != 5*time.Minute {
		t.Errorf("tiny TTL should clamp to default; got %v", h.cacheTTL)
	}
}

// ---------- ServeHTTP --------------------------------------------------------

func TestServeHTTP_HappyPath(t *testing.T) {
	stub := &stubSecretReader{
		values: map[string]string{"chora-stripe-publishable-key": "pk_test_abc123"},
	}
	h, _ := NewStripeConfigHandler(StripeConfigHandlerConfig{
		Secrets:                  stub,
		PublishableKeyName:       "chora-stripe-publishable-key",
		CheckoutSuccessURLPrefix: "https://chora.site/a/companion/hatching/",
		CheckoutCancelURL:        "https://chora.site/a/companion/marketplace",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stripe-config", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	var resp StripeConfigResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.PublishableKey != "pk_test_abc123" {
		t.Errorf("publishableKey: got %q", resp.PublishableKey)
	}
	if resp.CheckoutSuccessURLPrefix != "https://chora.site/a/companion/hatching/" {
		t.Errorf("success url: got %q", resp.CheckoutSuccessURLPrefix)
	}
	if resp.CheckoutCancelURL != "https://chora.site/a/companion/marketplace" {
		t.Errorf("cancel url: got %q", resp.CheckoutCancelURL)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type: got %q", ct)
	}
}

func TestServeHTTP_OmitsURLFieldsWhenEmpty(t *testing.T) {
	stub := &stubSecretReader{
		values: map[string]string{"chora-stripe-publishable-key": "pk_test_x"},
	}
	h, _ := NewStripeConfigHandler(StripeConfigHandlerConfig{
		Secrets:            stub,
		PublishableKeyName: "chora-stripe-publishable-key",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stripe-config", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "checkoutSuccessUrlPrefix") {
		t.Errorf("response should omit checkoutSuccessUrlPrefix when empty; body=%s", body)
	}
	if strings.Contains(body, "checkoutCancelUrl") {
		t.Errorf("response should omit checkoutCancelUrl when empty; body=%s", body)
	}
}

func TestServeHTTP_RejectsNonGet(t *testing.T) {
	stub := &stubSecretReader{values: map[string]string{"k": "v"}}
	h, _ := NewStripeConfigHandler(StripeConfigHandlerConfig{
		Secrets: stub, PublishableKeyName: "k",
	})

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(m, func(t *testing.T) {
			req := httptest.NewRequest(m, "/api/v1/stripe-config", nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("method %s: got %d want 405", m, w.Code)
			}
			if w.Header().Get("Allow") != "GET" {
				t.Errorf("Allow header missing or wrong on 405: %q", w.Header().Get("Allow"))
			}
		})
	}
}

// ---------- cache behaviour --------------------------------------------------

func TestServeHTTP_CacheHitAvoidsSecondGSMCall(t *testing.T) {
	stub := &stubSecretReader{
		values: map[string]string{"chora-stripe-publishable-key": "pk_test_x"},
	}
	h, _ := NewStripeConfigHandler(StripeConfigHandlerConfig{
		Secrets: stub, PublishableKeyName: "chora-stripe-publishable-key",
	})

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/stripe-config", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("iter %d status %d", i, w.Code)
		}
	}
	if got := stub.callCount(); got != 1 {
		t.Errorf("GSM should be called once across 5 hits; got %d", got)
	}
}

func TestServeHTTP_RefetchesAfterCacheTTL(t *testing.T) {
	stub := &stubSecretReader{
		values: map[string]string{"chora-stripe-publishable-key": "pk_test_x"},
	}
	clock := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	h, _ := NewStripeConfigHandler(StripeConfigHandlerConfig{
		Secrets:            stub,
		PublishableKeyName: "chora-stripe-publishable-key",
		CacheTTL:           5 * time.Minute, // explicit
		Now:                func() time.Time { return clock },
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stripe-config", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if stub.callCount() != 1 {
		t.Fatalf("initial fetch should call GSM once; got %d", stub.callCount())
	}

	// Advance clock past the TTL.
	clock = clock.Add(6 * time.Minute)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if stub.callCount() != 2 {
		t.Errorf("refetch after TTL should call GSM again; got %d", stub.callCount())
	}
}

// ---------- failure paths ----------------------------------------------------

func TestServeHTTP_Returns503WhenGSMFailsAndCacheEmpty(t *testing.T) {
	stub := &stubSecretReader{err: errors.New("transient gsm error")}
	h, _ := NewStripeConfigHandler(StripeConfigHandlerConfig{
		Secrets: stub, PublishableKeyName: "chora-stripe-publishable-key",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stripe-config", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d want 503; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "STRIPE_CONFIG_UNAVAILABLE") {
		t.Errorf("body should include error code; got %s", w.Body.String())
	}
}

func TestServeHTTP_FallsBackToStaleCacheOnGSMFailure(t *testing.T) {
	stub := &stubSecretReader{
		values: map[string]string{"chora-stripe-publishable-key": "pk_test_x"},
	}
	clock := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	h, _ := NewStripeConfigHandler(StripeConfigHandlerConfig{
		Secrets:            stub,
		PublishableKeyName: "chora-stripe-publishable-key",
		CacheTTL:           5 * time.Minute,
		Now:                func() time.Time { return clock },
	})

	// First call seeds the cache.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/stripe-config", nil))
	if w.Code != 200 {
		t.Fatalf("seed status: %d", w.Code)
	}

	// Cache expires + GSM fails on next refetch.
	clock = clock.Add(10 * time.Minute)
	stub.mu.Lock()
	stub.err = errors.New("transient gsm error")
	stub.mu.Unlock()

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/stripe-config", nil))
	if w.Code != 200 {
		t.Errorf("stale-cache fallback should still 200; got %d body=%s", w.Code, w.Body.String())
	}
	var resp StripeConfigResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.PublishableKey != "pk_test_x" {
		t.Errorf("publishableKey: got %q want pk_test_x", resp.PublishableKey)
	}
}

func TestServeHTTP_Returns503OnEmptySecretValue(t *testing.T) {
	stub := &stubSecretReader{
		values: map[string]string{"chora-stripe-publishable-key": "   "},
	}
	h, _ := NewStripeConfigHandler(StripeConfigHandlerConfig{
		Secrets: stub, PublishableKeyName: "chora-stripe-publishable-key",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stripe-config", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d want 503", w.Code)
	}
}

// ---------- WithStripeConfigRoute composition -------------------------------

func TestWithStripeConfigRoute_PassthroughForOtherPaths(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h, _ := NewStripeConfigHandler(StripeConfigHandlerConfig{
		Secrets:            &stubSecretReader{values: map[string]string{"k": "v"}},
		PublishableKeyName: "k",
	})
	composed := WithStripeConfigRoute(base, h)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/companions", nil)
	w := httptest.NewRecorder()
	composed.ServeHTTP(w, req)
	if w.Code != http.StatusTeapot {
		t.Errorf("non-route should pass through to base (418); got %d", w.Code)
	}
}

func TestWithStripeConfigRoute_RoutesGetStripeConfig(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h, _ := NewStripeConfigHandler(StripeConfigHandlerConfig{
		Secrets: &stubSecretReader{
			values: map[string]string{"chora-stripe-publishable-key": "pk_test_y"},
		},
		PublishableKeyName: "chora-stripe-publishable-key",
	})
	composed := WithStripeConfigRoute(base, h)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stripe-config", nil)
	w := httptest.NewRecorder()
	composed.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("route should hit handler (200); got %d", w.Code)
	}
}

func TestWithStripeConfigRoute_NilHandlerPassthrough(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	composed := WithStripeConfigRoute(base, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stripe-config", nil)
	w := httptest.NewRecorder()
	composed.ServeHTTP(w, req)
	if w.Code != http.StatusTeapot {
		t.Errorf("nil handler should be passthrough; got %d", w.Code)
	}
}
