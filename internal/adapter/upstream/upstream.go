// Package upstream defines the UpstreamClient port the BFF uses to fetch
// data from internal domain services (chora-creation, chora-consumption,
// chora-delivery, chora-sharing, chora-tenancy, chora-governance, ...).
//
// The original M10 skeleton only had `FakeUpstream` (deterministic
// placeholders). Phase 6 added `HTTPUpstream` (real HTTP fan-out via service
// mesh mTLS). Phase C (O+ hydration, 2026-05-26) adds the narrow real-wire
// clients used by the new /bff/oplus/* handlers — `GovernanceClient`,
// `ObservabilityClient`, `A2AClient`. Those clients do NOT implement the
// 8-method `Client` interface — they are narrower, single-domain clients
// the new handlers compose directly.
//
// Per `feedback_no_stubs_real_wiring`: `FakeUpstream` is gated behind the
// `CHORA_GATEWAY_UPSTREAM_FAKE=true` env var — production must wire the
// real clients. The legacy /bff/{aplus,cplus,hplus,rplus}/* + the original
// /bff/oplus/governance handlers still depend on the 8-method Client
// interface for their existing flow; nothing about THIS file needs to
// change to support Phase C, and the FakeUpstream selection is enforced
// in cmd/server/main.go via the env-flag check + `IsFakeUpstreamAllowed()`.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// IsFakeUpstreamAllowed reports whether the FakeUpstream may be selected as
// the BFF's `upstream.Client`. Per `feedback_no_stubs_real_wiring` real
// wiring is the production posture — `FakeUpstream` is opt-in only via
// `CHORA_GATEWAY_UPSTREAM_FAKE=true` (smoke / unit-test fallback).
// Callers that wire FakeUpstream without the flag MUST log a warning so the
// drift is visible.
func IsFakeUpstreamAllowed() bool {
	return os.Getenv("CHORA_GATEWAY_UPSTREAM_FAKE") == "true"
}

// ErrUpstream is the sentinel for an upstream call that failed.
var ErrUpstream = errors.New("upstream call failed")

// Client is the BFF's read port for internal domain services. Each method
// composes the BFF's known surface views; methods are intentionally narrow —
// the BFF does NOT proxy arbitrary backend calls through this interface
// (generic proxy is a separate concern).
type Client interface {
	// GetLearningPath fetches the learner's active LearningPath from
	// chora-consumption (Content Consumption domain).
	GetLearningPath(ctx context.Context, tenantID, gcid string) (any, error)

	// GetRecentAtoms fetches the most recent published atoms from
	// chora-creation (Content Creation domain).
	GetRecentAtoms(ctx context.Context, tenantID, gcid string) (any, error)

	// GetCompanion fetches the learner's RPG companion state from
	// chora-consumption (Content Consumption domain). Companion is a domain
	// entity, NOT an AI agent — distinction per memory feedback_companion_vs_agent.
	GetCompanion(ctx context.Context, tenantID, gcid string) (any, error)

	// GetFeed fetches the learner's social feed from chora-sharing.
	GetFeed(ctx context.Context, tenantID, gcid string) (any, error)

	// GetTenant fetches tenant + entitlements from chora-tenancy.
	GetTenant(ctx context.Context, tenantID, gcid string) (any, error)

	// GetGovernance fetches the IMDA governance dashboard from chora-governance.
	GetGovernance(ctx context.Context, tenantID, gcid string) (any, error)

	// GetAuditEvents fetches recent audit events from chora-observability.
	GetAuditEvents(ctx context.Context, tenantID, gcid string) (any, error)

	// GetCourses fetches the course list from chora-delivery.
	GetCourses(ctx context.Context, tenantID, gcid string) (any, error)
}

// FakeUpstream is the deterministic skeleton implementation used during M10.
//
// Failure injection: if a method's name appears in FailMethods, the call
// returns ErrUpstream. This is the mechanism tests use to verify graceful
// degradation in the BFF aggregation paths.
type FakeUpstream struct {
	FailMethods map[string]bool // e.g. {"GetRecentAtoms": true}
}

// NewFakeUpstream constructs a non-failing fake.
//
// Per `feedback_no_stubs_real_wiring`: callers in production main.go MUST
// gate this on `IsFakeUpstreamAllowed()` so the in-memory stub never
// silently ships. Tests pass through unconditionally (they don't read the
// env var anyway, and the env-flag gate is enforced ONLY at the
// production-wiring layer).
func NewFakeUpstream() *FakeUpstream {
	return &FakeUpstream{FailMethods: map[string]bool{}}
}

// shouldFail returns true if the named method should return ErrUpstream.
func (f *FakeUpstream) shouldFail(method string) bool {
	if f == nil || f.FailMethods == nil {
		return false
	}
	return f.FailMethods[method]
}

// GetLearningPath returns a deterministic placeholder LearningPath payload.
func (f *FakeUpstream) GetLearningPath(_ context.Context, tenantID, gcid string) (any, error) {
	if f.shouldFail("GetLearningPath") {
		return nil, fmt.Errorf("%w: chora-consumption stub failure", ErrUpstream)
	}
	return map[string]any{
		"path_id":    "path-skeleton-001",
		"title":      "Skeleton Path: Calculus 101",
		"tenant_id":  tenantID,
		"gcid":       gcid,
		"step_count": 12,
		"completed":  3,
		"next_step": map[string]any{
			"step_id": "step-004",
			"title":   "Limits at infinity",
		},
		"_stub": true,
	}, nil
}

// GetRecentAtoms returns deterministic placeholder atoms.
func (f *FakeUpstream) GetRecentAtoms(_ context.Context, tenantID, gcid string) (any, error) {
	if f.shouldFail("GetRecentAtoms") {
		return nil, fmt.Errorf("%w: chora-creation stub failure", ErrUpstream)
	}
	return []map[string]any{
		{"atom_id": "atom-skel-001", "title": "Photosynthesis Light Reactions", "tenant_id": tenantID, "gcid": gcid},
		{"atom_id": "atom-skel-002", "title": "Newton's Second Law", "tenant_id": tenantID, "gcid": gcid},
	}, nil
}

// GetCompanion returns a deterministic placeholder Companion state.
func (f *FakeUpstream) GetCompanion(_ context.Context, tenantID, gcid string) (any, error) {
	if f.shouldFail("GetCompanion") {
		return nil, fmt.Errorf("%w: chora-consumption stub failure (companion)", ErrUpstream)
	}
	return map[string]any{
		"companion_id": "fam-skel-001",
		"name":         "Pip",
		"tenant_id":    tenantID,
		"gcid":         gcid,
		"level":        4,
		"mood":         "curious",
		"_note":        "Companion = RPG companion (domain entity), distinct from the AI agent powering it.",
		"_stub":        true,
	}, nil
}

// GetFeed returns a placeholder C+ feed.
func (f *FakeUpstream) GetFeed(_ context.Context, tenantID, gcid string) (any, error) {
	if f.shouldFail("GetFeed") {
		return nil, fmt.Errorf("%w: chora-sharing stub failure", ErrUpstream)
	}
	return map[string]any{
		"posts": []map[string]any{
			{"post_id": "post-skel-001", "author_gcid": gcid, "tenant_id": tenantID, "text": "Just hit a 5-day streak!", "reactions": 12},
			{"post_id": "post-skel-002", "author_gcid": "gcid-other", "tenant_id": tenantID, "text": "Calc 101 done.", "reactions": 4},
		},
		"reactions_received": 7,
		"_stub":              true,
	}, nil
}

// GetTenant returns placeholder tenant + entitlements.
func (f *FakeUpstream) GetTenant(_ context.Context, tenantID, _ string) (any, error) {
	if f.shouldFail("GetTenant") {
		return nil, fmt.Errorf("%w: chora-tenancy stub failure", ErrUpstream)
	}
	return map[string]any{
		"tenant_id": tenantID,
		"name":      "Skeleton Academy",
		"plan":      "core",
		"add_ons":   []string{"reward-vault", "skillsfutures"},
		"_stub":     true,
	}, nil
}

// GetGovernance returns a placeholder IMDA dashboard slice.
func (f *FakeUpstream) GetGovernance(_ context.Context, tenantID, _ string) (any, error) {
	if f.shouldFail("GetGovernance") {
		return nil, fmt.Errorf("%w: chora-governance stub failure", ErrUpstream)
	}
	return map[string]any{
		"tenant_id": tenantID,
		"imda_dimensions": map[string]string{
			"d1_risk":         "green",
			"d2_oversight":    "green",
			"d3_cost_safety":  "amber",
			"d4_transparency": "green",
		},
		"open_findings": 1,
		"_stub":         true,
	}, nil
}

// GetAuditEvents returns placeholder audit events.
func (f *FakeUpstream) GetAuditEvents(_ context.Context, tenantID, _ string) (any, error) {
	if f.shouldFail("GetAuditEvents") {
		return nil, fmt.Errorf("%w: chora-observability stub failure", ErrUpstream)
	}
	return []map[string]any{
		{"event_id": "evt-skel-001", "tenant_id": tenantID, "kind": "access.tenant.read", "actor": "admin@skel"},
		{"event_id": "evt-skel-002", "tenant_id": tenantID, "kind": "atom.published", "actor": "gcid-author"},
	}, nil
}

// GetCourses returns a placeholder R+ course list.
func (f *FakeUpstream) GetCourses(_ context.Context, tenantID, _ string) (any, error) {
	if f.shouldFail("GetCourses") {
		return nil, fmt.Errorf("%w: chora-delivery stub failure", ErrUpstream)
	}
	return []map[string]any{
		{"course_id": "course-skel-001", "tenant_id": tenantID, "title": "Spring 2026 Calculus", "instructor": "Dr. Skel", "enrolled": 24},
		{"course_id": "course-skel-002", "tenant_id": tenantID, "title": "Bio Lab", "instructor": "Dr. Skel", "enrolled": 12},
	}, nil
}
