package inmem

import (
	"context"
	"sync"
	"time"

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

// ConsumeRefresh atomically claims the record for a single use. The check
// (present, unexpired, not consumed) and the mark happen under one lock, so
// concurrent exchanges of the same token cannot both succeed — the loser sees
// the record already consumed.
func (r *RefreshStore) ConsumeRefresh(_ context.Context, token string) (*session.RefreshRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.records[token]
	if !ok {
		return nil, session.ErrRefreshNotFound
	}
	if rec.Revoked {
		return nil, session.ErrRefreshAlreadyConsumed
	}
	if rec.IsExpired(time.Now().UTC()) {
		return nil, session.ErrRefreshExpired
	}
	rec.Revoked = true
	return cloneRefresh(rec), nil
}
