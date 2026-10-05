package route_test

import (
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/domain/route"
)

func TestNew_Valid(t *testing.T) {
	r, err := route.New(route.NewParams{
		Surface:        route.SurfaceAPlus,
		PathPattern:    "/bff/aplus/home",
		BackendService: "chora-consumption",
		AuthMode:       route.AuthModePublic,
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if r.Surface != route.SurfaceAPlus {
		t.Errorf("surface = %q, want aplus", r.Surface)
	}
	if r.AuthMode != route.AuthModePublic {
		t.Errorf("auth_mode = %q, want public", r.AuthMode)
	}
}

func TestNew_InvalidSurface(t *testing.T) {
	_, err := route.New(route.NewParams{
		Surface:        "zplus",
		PathPattern:    "/bff/zplus/home",
		BackendService: "chora-zplus",
		AuthMode:       route.AuthModePublic,
	})
	if err == nil {
		t.Fatal("expected error for invalid surface")
	}
}

func TestNew_EmptyPathPattern(t *testing.T) {
	_, err := route.New(route.NewParams{
		Surface:        route.SurfaceAPlus,
		PathPattern:    "  ",
		BackendService: "chora-consumption",
		AuthMode:       route.AuthModePublic,
	})
	if err == nil {
		t.Fatal("expected error for empty path_pattern")
	}
}

func TestNew_PathPatternMustStartWithSlash(t *testing.T) {
	_, err := route.New(route.NewParams{
		Surface:        route.SurfaceAPlus,
		PathPattern:    "bff/aplus/home",
		BackendService: "chora-consumption",
		AuthMode:       route.AuthModePublic,
	})
	if err == nil {
		t.Fatal("expected error for missing leading /")
	}
}

func TestNew_EmptyBackend(t *testing.T) {
	_, err := route.New(route.NewParams{
		Surface:        route.SurfaceAPlus,
		PathPattern:    "/bff/aplus/home",
		BackendService: "",
		AuthMode:       route.AuthModePublic,
	})
	if err == nil {
		t.Fatal("expected error for empty backend_service")
	}
}

func TestNew_InvalidAuthMode(t *testing.T) {
	_, err := route.New(route.NewParams{
		Surface:        route.SurfaceAPlus,
		PathPattern:    "/bff/aplus/home",
		BackendService: "chora-consumption",
		AuthMode:       "weird",
	})
	if err == nil {
		t.Fatal("expected error for invalid auth_mode")
	}
}

func TestSurfaceValid(t *testing.T) {
	cases := map[route.Surface]bool{
		route.SurfaceAPlus: true,
		route.SurfaceCPlus: true,
		route.SurfaceHPlus: true,
		route.SurfaceOPlus: true,
		route.SurfaceRPlus: true,
		route.SurfaceAPI:   true,
		"weird":            false,
		"":                 false,
	}
	for s, want := range cases {
		if got := s.Valid(); got != want {
			t.Errorf("Surface(%q).Valid()=%v want %v", s, got, want)
		}
	}
}

func TestAuthModeValid(t *testing.T) {
	cases := map[route.AuthMode]bool{
		route.AuthModePublic:        true,
		route.AuthModeAuthenticated: true,
		route.AuthModeAdmin:         true,
		"":                          false,
		"unknown":                   false,
	}
	for a, want := range cases {
		if got := a.Valid(); got != want {
			t.Errorf("AuthMode(%q).Valid()=%v want %v", a, got, want)
		}
	}
}

func TestMatches_Exact(t *testing.T) {
	r, _ := route.New(route.NewParams{
		Surface:        route.SurfaceAPlus,
		PathPattern:    "/bff/aplus/home",
		BackendService: "chora-consumption",
		AuthMode:       route.AuthModePublic,
	})
	if !r.Matches("/bff/aplus/home") {
		t.Error("expected exact match")
	}
	if r.Matches("/bff/aplus/feed") {
		t.Error("did not expect feed to match home")
	}
}

func TestMatches_Wildcard(t *testing.T) {
	r, _ := route.New(route.NewParams{
		Surface:        route.SurfaceAPI,
		PathPattern:    "/api/proxy/chora-creation/*",
		BackendService: "chora-creation",
		AuthMode:       route.AuthModeAuthenticated,
	})
	if !r.Matches("/api/proxy/chora-creation/atoms") {
		t.Error("expected /api/proxy/chora-creation/atoms to match")
	}
	if !r.Matches("/api/proxy/chora-creation/atoms/123/revisions") {
		t.Error("expected nested wildcard match")
	}
	if r.Matches("/api/proxy/chora-other/atoms") {
		t.Error("did not expect non-prefix to match")
	}
}
