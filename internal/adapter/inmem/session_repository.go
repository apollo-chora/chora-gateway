// Package inmem holds in-memory adapters for the Session and Route ports
// used in the M10 skeleton. Production will replace these with Memorystore
// (Valkey) and a config-driven route table respectively.
package inmem

import (
	"context"
	"sync"

	"github.com/apollo-chora/chora-gateway/internal/domain/session"
)

// SessionRepository is a goroutine-safe map-backed Session repository.
type SessionRepository struct {
	mu       sync.RWMutex
	sessions map[string]*session.Session
}

// NewSessionRepository constructs an initialised repository.
func NewSessionRepository() *SessionRepository {
	return &SessionRepository{sessions: make(map[string]*session.Session)}
}

// Save persists the session.
func (r *SessionRepository) Save(_ context.Context, s *session.Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	clone := *s
	clone.Roles = append([]string(nil), s.Roles...)
	r.sessions[s.Token] = &clone
	return nil
}

// Get returns the session for the given token.
func (r *SessionRepository) Get(_ context.Context, token string) (*session.Session, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sessions[token]
	if !ok {
		return nil, session.ErrSessionNotFound
	}
	clone := *s
	clone.Roles = append([]string(nil), s.Roles...)
	return &clone, nil
}

// Delete removes the session.
func (r *SessionRepository) Delete(_ context.Context, token string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, token)
	return nil
}
