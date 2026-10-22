package session

import (
	"context"
	"errors"
	"time"
)

// ErrRefreshNotFound is returned when no refresh record exists for a token.
var ErrRefreshNotFound = errors.New("refresh token not found")

// ErrRefreshAlreadyConsumed is returned when a token has already been spent by
// a prior refresh. Refresh tokens are single-use: a second consume of the same
// token (a concurrent request, a network retry, or a replayed stolen token)
// must fail rather than mint another live session.
var ErrRefreshAlreadyConsumed = errors.New("refresh token already consumed")

// ErrRefreshExpired is returned when a token has passed its expiry.
var ErrRefreshExpired = errors.New("refresh token expired")

// Membership is the tenant-scoped membership snapshot stored with a refresh
// record. A refresh must re-emit the same envelope shape as a mint, and the BFF
// seeds its surface rail from memberships[].surfaces — so the snapshot is what
// keeps a surface the caller already holds visible across a token rotation.
//
// It is deliberately a domain type, not the adapter's wire DTO: the session
// package must not import the HTTP client layer.
type Membership struct {
	TenantID   string   `json:"tenant_id"`
	TenantSlug string   `json:"tenant_slug"`
	Roles      []string `json:"roles"`
	Surfaces   []string `json:"surfaces"`
	IsDefault  bool     `json:"is_default"`
}

// RefreshRecord is the durable half of a Chora session. The access JWT is
// stateless and short-lived (CHORA_SESSION_TTL_SECONDS, default 1h); the
// refresh record is the opaque, rotatable credential that survives reloads
// and lets the gateway mint a fresh access JWT without a password.
//
// Rotation: every refresh revokes the presented record and stores a new
// one, so a stolen record cannot be replayed after its first use. The new
// record inherits Memberships from the one it replaces.
type RefreshRecord struct {
	Token      string       `json:"token"`      // opaque, server-issued
	Gcid       string       `json:"gcid"`       // session owner
	Email      string       `json:"email"`
	TenantID   string       `json:"tenant_id"`
	Roles      []string     `json:"roles"`
	Memberships []Membership `json:"memberships,omitempty"`
	IssuedAt   time.Time    `json:"issued_at"`
	ExpiresAt  time.Time    `json:"expires_at"`
	Revoked    bool         `json:"revoked"`
}

// IsExpired reports whether the record has reached its expiry at now
// (inclusive — a token is expired at exactly ExpiresAt).
func (r *RefreshRecord) IsExpired(now time.Time) bool {
	return !now.Before(r.ExpiresAt)
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

	// ConsumeRefresh atomically claims the record for a single use: it returns
	// the record only if it is present, unexpired, and not already consumed, and
	// marks it consumed in the same locked step. This is what makes rotation
	// safe — two requests racing the same token cannot both mint a live
	// session; the loser gets ErrRefreshAlreadyConsumed.
	ConsumeRefresh(ctx context.Context, token string) (*RefreshRecord, error)

	// ReplaceRefresh atomically consumes the presented token and persists its
	// replacement in one locked step: the old record is marked consumed ONLY if
	// the new record is stored. A failed rotation therefore leaves the old
	// token active and retryable (no stranded session), and a concurrent
	// exchange cannot mint two live descendants.
	ReplaceRefresh(ctx context.Context, oldToken string, newRec *RefreshRecord) error
}
