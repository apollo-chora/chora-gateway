// Session repository port. Adapters: inmem (skeleton); Valkey/Memorystore
// in production.
package session

import "context"

// Repository is the Session persistence port.
type Repository interface {
	// Save persists the session keyed by Token. Idempotent.
	Save(ctx context.Context, s *Session) error

	// Get returns the session for the given token. Returns ErrSessionNotFound
	// when missing.
	Get(ctx context.Context, token string) (*Session, error)

	// Delete removes the session (sign-out). Idempotent.
	Delete(ctx context.Context, token string) error
}
