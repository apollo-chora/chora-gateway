package session

import (
	"context"
	"errors"
	"time"
)

// ErrRefreshNotFound is returned when no refresh record exists for a token.
var ErrRefreshNotFound = errors.New("refresh token not found")

// RefreshRecord is the durable half of a Chora session. The access JWT is
// stateless and short-lived (CHORA_SESSION_TTL_SECONDS, default 1h); the
// refresh record is the opaque, rotatable credential that survives reloads
// and lets the gateway mint a fresh access JWT without a password.
//
// Rotation: every refresh revokes the presented record and stores a new
// one, so a stolen record cannot be replayed after its first use.
type RefreshRecord struct {
	Token     string    `json:"token"`      // opaque, server-issued
	Gcid      string    `json:"gcid"`       // session owner
	Email     string    `json:"email"`
	TenantID  string    `json:"tenant_id"`
	Roles     []string  `json:"roles"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Revoked   bool      `json:"revoked"`
}

// IsExpired reports whether the record has passed its expiry at now.
func (r *RefreshRecord) IsExpired(now time.Time) bool {
	return now.After(r.ExpiresAt)
}

// RefreshStore persists refresh records. Adapters: inmem (skeleton);
// Valkey/Memorystore in production.
type RefreshStore interface {
	// SaveRefresh persists the record keyed by Token. Idempotent.
	SaveRefresh(ctx context.Context, r *RefreshRecord) error

	// GetRefresh returns the record for the given token. Returns
	// ErrRefreshNotFound when missing.
	GetRefresh(ctx context.Context, token string) (*RefreshRecord, error)

	// RevokeRefresh marks the record revoked (sign-out / rotation).
	// Idempotent — a missing token is not an error.
	RevokeRefresh(ctx context.Context, token string) error
}
