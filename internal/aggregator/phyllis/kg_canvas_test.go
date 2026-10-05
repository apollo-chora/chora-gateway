// kg_canvas_test.go — aggregator-level specs for the KG hexagon-canvas
// fan-outs (kg_canvas.go; ADR-143, learner-knowledge-graph.yaml v1.1).
// Pins downstream path + method + verbatim body + id guards + the sibling
// classify conventions (4xx pass-through, 5xx → 502). The HTTP route layer
// (dispatch, identity-header forwarding through authCtxFromRequest, JWT
// gate) is covered in internal/adapter/http/routes_kg_canvas_test.go.
package phyllis_test

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

const (
	kgcTestCluster  = "01970000-0000-7000-8000-00000000c222"
	kgcTestExplore  = "01970000-0000-7000-8000-00000000e222"
	kgcTestJunction = "01970000-0000-7000-8000-00000000d222"
	kgcTestTenant   = "01970000-0000-7000-8000-0000000000cc"
)

func kgcAuth() phyllis.AuthCtx {
	return phyllis.AuthCtx{Bearer: "x", TenantID: kgcTestTenant, GCID: "gcid-kgc"}
}

// TestKGCanvas_Aggregator_FanOutTable pins every canvas method's downstream
// path + HTTP method + verbatim body forwarding against the chora-consumption
// EXT mounts (kg_canvas_handler.go).
func TestKGCanvas_Aggregator_FanOutTable(t *testing.T) {
	type invoke func(a *phyllis.Aggregator) (phyllis.Response, error)
	cases := []struct {
		name       string
		call       invoke
		wantMethod string
		wantPath   string
		wantBody   string
	}{
		{
			name: "hexagon",
			call: func(a *phyllis.Aggregator) (phyllis.Response, error) {
				return a.GetKGCanvasHexagon(context.Background(), kgcAuth(), kgcTestCluster, kgcTestExplore)
			},
			wantMethod: http.MethodGet,
			wantPath:   "/v1/me/knowledge-graph/clusters/" + kgcTestCluster + "/explorations/" + kgcTestExplore + "/hexagon",
		},
		{
			name: "focal move keeps the colon segment literal",
			call: func(a *phyllis.Aggregator) (phyllis.Response, error) {
				return a.MoveKGCanvasFocal(context.Background(), kgcAuth(), kgcTestCluster, kgcTestExplore, []byte(`{"targetAtomId":"atom-7"}`))
			},
			wantMethod: http.MethodPost,
			wantPath:   "/v1/me/knowledge-graph/clusters/" + kgcTestCluster + "/explorations/" + kgcTestExplore + "/focal:move",
			wantBody:   `{"targetAtomId":"atom-7"}`,
		},
		{
			name: "archive",
			call: func(a *phyllis.Aggregator) (phyllis.Response, error) {
				return a.ArchiveKGCanvasCluster(context.Background(), kgcAuth(), kgcTestCluster, []byte(`{}`))
			},
			wantMethod: http.MethodPost,
			wantPath:   "/v1/me/knowledge-graph/clusters/" + kgcTestCluster + "/archive",
			wantBody:   `{}`,
		},
		{
			name: "convert (ADR-223 MapCluster→Goal projection)",
			call: func(a *phyllis.Aggregator) (phyllis.Response, error) {
				return a.ConvertKGCanvasCluster(context.Background(), kgcAuth(), kgcTestCluster, []byte(`{}`))
			},
			wantMethod: http.MethodPost,
			wantPath:   "/v1/me/knowledge-graph/clusters/" + kgcTestCluster + "/convert",
			wantBody:   `{}`,
		},
		{
			name: "junction decide",
			call: func(a *phyllis.Aggregator) (phyllis.Response, error) {
				return a.DecideKGCanvasJunction(context.Background(), kgcAuth(), kgcTestJunction, []byte(`{"decision":"decline"}`))
			},
			wantMethod: http.MethodPost,
			wantPath:   "/v1/me/knowledge-graph/junctions/" + kgcTestJunction + "/decide",
			wantBody:   `{"decision":"decline"}`,
		},
		{
			name: "management list",
			call: func(a *phyllis.Aggregator) (phyllis.Response, error) {
				return a.ListKGCanvasManagement(context.Background(), kgcAuth())
			},
			wantMethod: http.MethodGet,
			wantPath:   "/v1/me/knowledge-graph/clusters/management",
		},
		{
			name: "rename",
			call: func(a *phyllis.Aggregator) (phyllis.Response, error) {
				return a.RenameKGCanvasCluster(context.Background(), kgcAuth(), kgcTestCluster, []byte(`{"displayName":"Maps"}`))
			},
			wantMethod: http.MethodPatch,
			wantPath:   "/v1/me/knowledge-graph/clusters/" + kgcTestCluster,
			wantBody:   `{"displayName":"Maps"}`,
		},
		{
			name: "tenant config get",
			call: func(a *phyllis.Aggregator) (phyllis.Response, error) {
				return a.GetTenantKGConfig(context.Background(), kgcAuth(), kgcTestTenant)
			},
			wantMethod: http.MethodGet,
			wantPath:   "/v1/tenants/" + kgcTestTenant + "/knowledge-graph/config",
		},
		{
			name: "tenant config patch",
			call: func(a *phyllis.Aggregator) (phyllis.Response, error) {
				return a.PatchTenantKGConfig(context.Background(), kgcAuth(), kgcTestTenant, []byte(`{"kgFogInvalidationGraceSeconds":600}`))
			},
			wantMethod: http.MethodPatch,
			wantPath:   "/v1/tenants/" + kgcTestTenant + "/knowledge-graph/config",
			wantBody:   `{"kgFogInvalidationGraceSeconds":600}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath, gotBody, gotGcid, gotTenant string
			consumption := newStubUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotPath = r.URL.Path
				gotGcid = r.Header.Get("gcid")
				gotTenant = r.Header.Get("X-Tenant-Id")
				if r.Body != nil {
					b, _ := io.ReadAll(r.Body)
					gotBody = string(b)
				}
				_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
			})
			a := phyllis.New(phyllis.Config{ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)

			res, err := tc.call(a)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if res.Status != http.StatusOK {
				t.Errorf("status = %d; want 200 (body %s)", res.Status, res.Body)
			}
			if gotMethod != tc.wantMethod {
				t.Errorf("downstream method = %s; want %s", gotMethod, tc.wantMethod)
			}
			if gotPath != tc.wantPath {
				t.Errorf("downstream path = %q; want %q", gotPath, tc.wantPath)
			}
			if tc.wantBody != "" && gotBody != tc.wantBody {
				t.Errorf("downstream body = %q; want verbatim %q", gotBody, tc.wantBody)
			}
			// D1.5 invariant — every canvas fan-out stamps the lowercase gcid
			// + X-Tenant-Id pair chora-consumption's extRequireContext reads.
			if gotGcid != "gcid-kgc" {
				t.Errorf("lowercase gcid header = %q; want gcid-kgc", gotGcid)
			}
			if gotTenant != kgcTestTenant {
				t.Errorf("X-Tenant-Id header = %q; want %q", gotTenant, kgcTestTenant)
			}
		})
	}
}

// TestKGCanvas_Aggregator_EmptyIDGuards pins the defensive 404 envelopes:
// the aggregator must never build a downstream URL from an empty id segment.
func TestKGCanvas_Aggregator_EmptyIDGuards(t *testing.T) {
	consumption := newStubUpstream(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("downstream must not be called when an id is empty")
	})
	a := phyllis.New(phyllis.Config{ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
	ctx := context.Background()

	checks := []struct {
		name string
		res  func() (phyllis.Response, error)
	}{
		{"hexagon empty cluster", func() (phyllis.Response, error) {
			return a.GetKGCanvasHexagon(ctx, kgcAuth(), "", kgcTestExplore)
		}},
		{"hexagon empty exploration", func() (phyllis.Response, error) {
			return a.GetKGCanvasHexagon(ctx, kgcAuth(), kgcTestCluster, "")
		}},
		{"focal move empty exploration", func() (phyllis.Response, error) {
			return a.MoveKGCanvasFocal(ctx, kgcAuth(), kgcTestCluster, "", nil)
		}},
		{"archive empty cluster", func() (phyllis.Response, error) {
			return a.ArchiveKGCanvasCluster(ctx, kgcAuth(), "", nil)
		}},
		{"convert empty cluster", func() (phyllis.Response, error) {
			return a.ConvertKGCanvasCluster(ctx, kgcAuth(), "", nil)
		}},
		{"decide empty junction", func() (phyllis.Response, error) {
			return a.DecideKGCanvasJunction(ctx, kgcAuth(), "", nil)
		}},
		{"rename empty cluster", func() (phyllis.Response, error) {
			return a.RenameKGCanvasCluster(ctx, kgcAuth(), "", nil)
		}},
		{"config get empty tenant", func() (phyllis.Response, error) {
			return a.GetTenantKGConfig(ctx, kgcAuth(), "")
		}},
		{"config patch empty tenant", func() (phyllis.Response, error) {
			return a.PatchTenantKGConfig(ctx, kgcAuth(), "", nil)
		}},
	}
	for _, c := range checks {
		res, err := c.res()
		if err != nil {
			t.Errorf("%s: err = %v", c.name, err)
			continue
		}
		if res.Status != http.StatusNotFound {
			t.Errorf("%s: status = %d; want 404 guard", c.name, res.Status)
		}
	}
	if consumption.calls.Load() != 0 {
		t.Errorf("downstream calls = %d; want 0", consumption.calls.Load())
	}
}

// TestKGCanvas_Aggregator_ClassifyConventions pins the sibling error
// translation on a canvas method: downstream 404 (FOG_CACHE_MISS) passes
// through verbatim; downstream 503 normalises to 502 GATEWAY_UPSTREAM_5XX.
func TestKGCanvas_Aggregator_ClassifyConventions(t *testing.T) {
	t.Run("404 passes through", func(t *testing.T) {
		consumption := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"FOG_CACHE_MISS","message":"hexagon not cached"}`))
		})
		a := phyllis.New(phyllis.Config{ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
		res, _ := a.GetKGCanvasHexagon(context.Background(), kgcAuth(), kgcTestCluster, kgcTestExplore)
		if res.Status != http.StatusNotFound {
			t.Errorf("status = %d; want 404 pass-through", res.Status)
		}
		if string(res.Body) != `{"code":"FOG_CACHE_MISS","message":"hexagon not cached"}` {
			t.Errorf("body = %s; want verbatim", res.Body)
		}
	})
	t.Run("503 becomes 502", func(t *testing.T) {
		consumption := newStubUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		a := phyllis.New(phyllis.Config{ConsumptionURL: consumption.URL, PerCallTimeout: 1 * time.Second}, nil)
		res, _ := a.DecideKGCanvasJunction(context.Background(), kgcAuth(), kgcTestJunction, []byte(`{"decision":"accept"}`))
		if res.Status != http.StatusBadGateway {
			t.Errorf("status = %d; want 502", res.Status)
		}
	})
}
