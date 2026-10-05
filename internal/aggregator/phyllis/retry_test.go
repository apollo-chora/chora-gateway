package phyllis_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

// These tests pin the idempotent-GET retry that absorbs the transient mesh-level
// 503 / connection reset chora-consumption emits during a single-replica rollout
// or idle-connection-reset window (the daily-dose 502 flakiness). A GET retries
// once on 503; a non-503/4xx is passed through unretried; a POST is NEVER retried.

func TestCallGet_Retries503ThenSucceeds(t *testing.T) {
	var hits int32
	up := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // mesh "no healthy upstream"
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"gcid":"x"}`))
	})
	a := phyllis.New(phyllis.Config{IdentityURL: up.URL, PerCallTimeout: 1 * time.Second}, nil)

	res, err := a.GetMe(context.Background(), phyllis.AuthCtx{Bearer: "x"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d; want 200 after one retry", res.Status)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Errorf("upstream hits = %d; want 2 (original + 1 retry)", n)
	}
}

func TestCallGet_503PersistsReturns502_AfterMaxAttempts(t *testing.T) {
	var hits int32
	up := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	a := phyllis.New(phyllis.Config{IdentityURL: up.URL, PerCallTimeout: 1 * time.Second}, nil)

	res, err := a.GetMe(context.Background(), phyllis.AuthCtx{Bearer: "x"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502 (persistent 503 normalised)", res.Status)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Errorf("upstream hits = %d; want 2 (bounded retry)", n)
	}
}

func TestCallGet_404NotRetried(t *testing.T) {
	var hits int32
	up := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound)
	})
	a := phyllis.New(phyllis.Config{IdentityURL: up.URL, PerCallTimeout: 1 * time.Second}, nil)

	res, err := a.GetMe(context.Background(), phyllis.AuthCtx{Bearer: "x"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusNotFound {
		t.Errorf("status = %d; want 404 pass-through", res.Status)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("upstream hits = %d; want 1 (4xx not retried)", n)
	}
}

func TestCallPost_NotRetriedOn503(t *testing.T) {
	var hits int32
	up := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	a := phyllis.New(phyllis.Config{DeliveryURL: up.URL, PerCallTimeout: 1 * time.Second}, nil)

	// CreateEnrollment is a POST — must NOT retry even on 503 (non-idempotent).
	res, err := a.CreateEnrollment(context.Background(), phyllis.AuthCtx{Bearer: "x"}, []byte(`{}`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Status != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", res.Status)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("upstream hits = %d; want 1 (POST never retried)", n)
	}
}
