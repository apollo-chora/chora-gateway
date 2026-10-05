// Package route is the BFFRoute aggregate.
//
// BFFRoute is a declarative route configuration that decides which upstream
// domain service handles a given (surface, path_pattern) pair, and what auth
// mode protects it. It's a small, value-object-heavy aggregate — the
// runtime BFF reads a Routes table at startup and matches incoming requests.
//
// Hexagonal: domain has no infra imports. The adapter layer wires this to a
// Repository (in-memory in the skeleton).
package route

import (
	"errors"
	"fmt"
	"strings"
)

// Surface is the CHORA surface code. Letters spell the platform: C+, H+, O+,
// R+, A+. The pseudo-surface "api" is for cross-surface API endpoints (auth,
// proxy) that aren't tied to a specific surface.
type Surface string

const (
	SurfaceAPlus Surface = "aplus"
	SurfaceCPlus Surface = "cplus"
	SurfaceHPlus Surface = "hplus"
	SurfaceOPlus Surface = "oplus"
	SurfaceRPlus Surface = "rplus"
	SurfaceAPI   Surface = "api"
)

// Valid returns true for one of the 6 known surface codes.
func (s Surface) Valid() bool {
	switch s {
	case SurfaceAPlus, SurfaceCPlus, SurfaceHPlus, SurfaceOPlus, SurfaceRPlus, SurfaceAPI:
		return true
	}
	return false
}

// AuthMode declares the authentication required to traverse a route.
//
//   - Public: no token required (e.g. /healthz, /bff/aplus/home for guest preview)
//   - Authenticated: opaque session token required; gcid + tenant_id resolved
//   - Admin: same as Authenticated + caller must hold an admin role on tenant
type AuthMode string

const (
	AuthModePublic        AuthMode = "public"
	AuthModeAuthenticated AuthMode = "authenticated"
	AuthModeAdmin         AuthMode = "admin"
)

// Valid returns true for one of the 3 known auth modes.
func (a AuthMode) Valid() bool {
	switch a {
	case AuthModePublic, AuthModeAuthenticated, AuthModeAdmin:
		return true
	}
	return false
}

// BFFRoute is the aggregate root for a single declarative route. The aggregate
// is immutable post-construction; the BFF reads the route table at startup.
type BFFRoute struct {
	Surface        Surface  `json:"surface"`
	PathPattern    string   `json:"path_pattern"`
	BackendService string   `json:"backend_service"`
	AuthMode       AuthMode `json:"auth_mode"`
}

// NewParams is the constructor input for New.
type NewParams struct {
	Surface        Surface
	PathPattern    string
	BackendService string
	AuthMode       AuthMode
}

// New constructs a BFFRoute and validates invariants.
func New(p NewParams) (*BFFRoute, error) {
	if !p.Surface.Valid() {
		return nil, fmt.Errorf("invalid surface: %q", string(p.Surface))
	}
	pattern := strings.TrimSpace(p.PathPattern)
	if pattern == "" {
		return nil, errors.New("path_pattern is required")
	}
	if !strings.HasPrefix(pattern, "/") {
		return nil, fmt.Errorf("path_pattern must start with /: %q", pattern)
	}
	backend := strings.TrimSpace(p.BackendService)
	if backend == "" {
		return nil, errors.New("backend_service is required")
	}
	if !p.AuthMode.Valid() {
		return nil, fmt.Errorf("invalid auth_mode: %q", string(p.AuthMode))
	}
	return &BFFRoute{
		Surface:        p.Surface,
		PathPattern:    pattern,
		BackendService: backend,
		AuthMode:       p.AuthMode,
	}, nil
}

// Matches returns true iff the given request path matches this route's pattern.
// Supports trailing /* wildcard.
func (r *BFFRoute) Matches(path string) bool {
	if r.PathPattern == path {
		return true
	}
	if strings.HasSuffix(r.PathPattern, "/*") {
		prefix := strings.TrimSuffix(r.PathPattern, "/*")
		return strings.HasPrefix(path, prefix+"/") || path == prefix
	}
	return false
}
