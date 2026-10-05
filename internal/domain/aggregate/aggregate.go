// Package aggregate models the AggregatedView aggregate.
//
// AggregatedView is the BFF's value-add: a composed payload built by fanning
// out to N upstream domain services and merging their responses into a single
// JSON envelope tuned for a specific surface screen (e.g. A+ home page).
//
// Graceful degradation invariant: if one upstream fails, the AggregatedView
// records the failure in PartErrors[partKey] and continues — it returns
// partial data with markers, NEVER a full 5xx, so the surface can render the
// available pieces and degrade for the missing pieces.
package aggregate

import (
	"errors"
	"strings"
	"time"
)

// AggregatedView is the aggregate root. It is constructed by domain logic in
// the http handler layer (composition logic isn't itself a repository
// concern). The struct is the "result" type; it carries both successful parts
// and error markers per part.
type AggregatedView struct {
	View       string            `json:"view"` // e.g. "aplus.home"
	TenantID   string            `json:"tenant_id"`
	Gcid       string            `json:"gcid"`
	Parts      map[string]any    `json:"parts"`       // partKey -> upstream JSON
	PartErrors map[string]string `json:"part_errors"` // partKey -> short error message
	BuiltAt    time.Time         `json:"built_at"`
	Meta       map[string]string `json:"meta,omitempty"`
}

// New constructs an empty AggregatedView for a given view ID.
func New(view, tenantID, gcid string) (*AggregatedView, error) {
	v := strings.TrimSpace(view)
	if v == "" {
		return nil, errors.New("view is required")
	}
	return &AggregatedView{
		View:       v,
		TenantID:   tenantID,
		Gcid:       gcid,
		Parts:      make(map[string]any),
		PartErrors: make(map[string]string),
		BuiltAt:    time.Now().UTC(),
	}, nil
}

// AddPart records a successful upstream response under partKey.
func (v *AggregatedView) AddPart(partKey string, payload any) {
	pk := strings.TrimSpace(partKey)
	if pk == "" {
		return
	}
	v.Parts[pk] = payload
}

// AddPartError records a failed upstream response. The view continues to be
// built — graceful degradation is a HARD invariant.
func (v *AggregatedView) AddPartError(partKey string, err error) {
	pk := strings.TrimSpace(partKey)
	if pk == "" || err == nil {
		return
	}
	v.PartErrors[pk] = err.Error()
}

// IsPartial returns true iff any part errored.
func (v *AggregatedView) IsPartial() bool {
	return len(v.PartErrors) > 0
}

// Status returns "ok" when all parts succeeded, "partial" when any failed.
func (v *AggregatedView) Status() string {
	if v.IsPartial() {
		return "partial"
	}
	return "ok"
}
