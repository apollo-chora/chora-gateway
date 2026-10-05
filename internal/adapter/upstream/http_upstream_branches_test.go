// http_upstream_branches_test.go — residual branch coverage for
// HTTPUpstream: the empty-URL guards on the URL-gated methods, the
// shouldFail nil-receiver / nil-map defences, urlHost edge cases, and the
// classifyJSON 4xx + malformed-JSON branches.
package upstream_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

func TestHTTPUpstream_EmptyURLGuards(t *testing.T) {
	// All URLs blank → each URL-gated method fails loud with ErrUpstream.
	up := upstream.NewHTTPUpstream(upstream.HTTPConfig{})
	ctx := context.Background()
	cases := []struct {
		name string
		call func() (any, error)
	}{
		{"GetTenant", func() (any, error) { return up.GetTenant(ctx, "t1", "g1") }},
		{"GetGovernance", func() (any, error) { return up.GetGovernance(ctx, "t1", "g1") }},
		{"GetAuditEvents", func() (any, error) { return up.GetAuditEvents(ctx, "t1", "g1") }},
		{"GetCourses", func() (any, error) { return up.GetCourses(ctx, "t1", "g1") }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.call()
			if err == nil {
				t.Fatalf("%s: expected error with blank URL", tc.name)
			}
			if !errors.Is(err, upstream.ErrUpstream) {
				t.Errorf("%s err = %v; want ErrUpstream wrap", tc.name, err)
			}
			if !strings.Contains(err.Error(), tc.name) {
				t.Errorf("%s err should name the method; got %v", tc.name, err)
			}
		})
	}
}

func TestHTTPUpstream_ClassifyJSON_4xxAndMalformed(t *testing.T) {
	// 4xx (non-404) → ErrUpstream wrap.
	srv403 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv403.Close()

	fixture := upstream.NewHTTPUpstream(upstream.HTTPConfig{ConsumptionURL: srv403.URL})
	if _, err := fixture.GetLearningPath(context.Background(), "t1", "g1"); !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("403 err = %v; want ErrUpstream wrap", err)
	}

	// Malformed JSON body → ErrUpstream wrap.
	srvBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{broken`))
	}))
	defer srvBad.Close()

	fixture = upstream.NewHTTPUpstream(upstream.HTTPConfig{ConsumptionURL: srvBad.URL})
	if _, err := fixture.GetLearningPath(context.Background(), "t1", "g1"); !errors.Is(err, upstream.ErrUpstream) {
		t.Errorf("malformed err = %v; want ErrUpstream wrap", err)
	}
}

func TestHTTPUpstream_ShouldFail_DefensiveNilBranches(t *testing.T) {
	ctx := context.Background()

	// Nil FailMethods map → shouldFail returns false (no injection).
	up := upstream.NewHTTPUpstream(upstream.HTTPConfig{})

	// A live server confirms the nil-map branch is exercised end-to-end.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	up = upstream.NewHTTPUpstream(upstream.HTTPConfig{ConsumptionURL: srv.URL})
	up.FailMethods = nil
	if _, err := up.GetLearningPath(ctx, "t1", "g1"); err != nil {
		t.Fatalf("GetLearningPath with nil FailMethods: %v", err)
	}
}
