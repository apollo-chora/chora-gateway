package inmem

import (
	"context"
	"sync"

	"github.com/apollo-chora/chora-gateway/internal/domain/session"
)

// RefreshStore is a goroutine-safe map-backed session.RefreshStore.
type RefreshStore struct {
	mu      sync.RWMutex
	records map[string]*session.RefreshRecord
}

// NewRefreshStore constructs an initialised store.
func NewRefreshStore() *RefreshStore {
	return &RefreshStore{records: make(map[string]*session.RefreshRecord)}
}

func cloneRefresh(r *session.RefreshRecord) *session.RefreshRecord {
	clone := *r
	clone.Roles = append([]string(nil), r.Roles...)
	return &clone
}

// SaveRefresh persists the record.
func (r *RefreshStore) SaveRefresh(_ context.Context, rec *session.RefreshRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records[rec.Token] = cloneRefresh(rec)
	return nil
}

// GetRefresh returns the record for the given token.
func (r *RefreshStore) GetRefresh(_ context.Context, token string) (*session.RefreshRecord, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.records[token]
	if !ok {
		return nil, session.ErrRefreshNotFound
	}
	return cloneRefresh(rec), nil
}

// RevokeRefresh marks the record revoked. Idempotent.
func (r *RefreshStore) RevokeRefresh(_ context.Context, token string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec, ok := r.records[token]; ok {
		rec.Revoked = true
	}
	return nil
}
