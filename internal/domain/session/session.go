// Package session is the Session aggregate.
//
// Session is the opaque session token issued by the BFF after exchanging an
// upstream OIDC JWT (issued by chora-identity). The BFF validates the JWT
// shape (skeleton: header.payload.signature with non-empty parts), extracts
// gcid + tenant_id, and mints an opaque session token. The session is stored
// in the skeleton's in-memory adapter; production will store in Memorystore
// (Valkey 8).
package session

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidJWT is returned when the JWT shape is malformed.
var ErrInvalidJWT = errors.New("invalid jwt shape")

// ErrSessionNotFound is returned when no session exists for the given token.
var ErrSessionNotFound = errors.New("session not found")

// Session is the aggregate root.
type Session struct {
	Token     string    `json:"token"`     // opaque, server-issued
	Gcid      string    `json:"gcid"`      // GCID UUIDv7 from JWT sub
	TenantID  string    `json:"tenant_id"` // active tenant from JWT claim
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Roles     []string  `json:"roles"` // optional admin role list
}

// NewParams is the constructor input.
type NewParams struct {
	Gcid     string
	TenantID string
	TTL      time.Duration
	Roles    []string
}

// New constructs a Session and mints a token. TTL defaults to 8h if zero.
func New(p NewParams) (*Session, error) {
	if strings.TrimSpace(p.Gcid) == "" {
		return nil, errors.New("gcid required")
	}
	if strings.TrimSpace(p.TenantID) == "" {
		return nil, errors.New("tenant_id required")
	}
	ttl := p.TTL
	if ttl <= 0 {
		ttl = 8 * time.Hour
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("uuidv7: %w", err)
	}
	now := time.Now().UTC()
	roles := append([]string(nil), p.Roles...)
	return &Session{
		Token:     id.String(),
		Gcid:      p.Gcid,
		TenantID:  p.TenantID,
		IssuedAt:  now,
		ExpiresAt: now.Add(ttl),
		Roles:     roles,
	}, nil
}

// IsExpired returns true iff the session has passed its expiry.
func (s *Session) IsExpired() bool {
	return time.Now().UTC().After(s.ExpiresAt)
}

// HasAdmin returns true iff the session carries an "admin" role string.
// (Skeleton: simple substring match; production will integrate with chora-tenancy
// TenantMembership lookups.)
func (s *Session) HasAdmin() bool {
	for _, r := range s.Roles {
		if strings.EqualFold(r, "admin") {
			return true
		}
	}
	return false
}

// ValidateJWTShape performs a structural check on a Bearer JWT — three
// dot-separated non-empty segments. This is intentionally minimal in the
// skeleton; production will verify signatures via chora-identity.
func ValidateJWTShape(token string) error {
	t := strings.TrimSpace(token)
	if t == "" {
		return ErrInvalidJWT
	}
	parts := strings.Split(t, ".")
	if len(parts) != 3 {
		return ErrInvalidJWT
	}
	for _, p := range parts {
		if strings.TrimSpace(p) == "" {
			return ErrInvalidJWT
		}
	}
	return nil
}
