// Repository — the BFFRoute lookup port.
//
// In the M10 skeleton, the in-memory implementation is seeded at startup with
// a deterministic table of routes. Future iterations may load from Cloud SQL
// or a config-driven table.
package route

import (
	"context"
	"errors"
)

// ErrNotFound is the canonical sentinel for an unmatched path.
var ErrNotFound = errors.New("route not found")

// Repository is the BFFRoute lookup port.
type Repository interface {
	// Match returns the first BFFRoute whose pattern matches the given path.
	// Returns ErrNotFound when no route matches.
	Match(ctx context.Context, path string) (*BFFRoute, error)

	// All returns the full route table (for diagnostics + the /readyz response).
	All(ctx context.Context) ([]*BFFRoute, error)
}
