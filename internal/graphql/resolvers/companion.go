package resolvers

import (
	"context"

	"github.com/apollo-chora/chora-gateway/internal/adapter/upstream"
)

// CompanionResolver wires the companion subschema — the in-game RPG companion
// DOMAIN entity. Per memory `feedback_companion_vs_agent`, the AI agent
// powering Companion responses lives behind a separate AI Kernel adapter and
// is NEVER exposed via this resolver. We surface only domain attributes
// (species, level, mood, equipped skin, bonded_at).
type CompanionResolver struct {
	Upstream    upstream.Client
	CtxIdentity func(ctx context.Context) (string, string)
}

// NewCompanionResolver constructs the resolver.
func NewCompanionResolver(up upstream.Client, ctxID func(ctx context.Context) (string, string)) *CompanionResolver {
	if ctxID == nil {
		ctxID = func(ctx context.Context) (string, string) { return "", "" }
	}
	return &CompanionResolver{Upstream: up, CtxIdentity: ctxID}
}

// MyCompanion resolves the `myCompanion` query. Returns the learner's
// Companion in domain shape (no AI agent fields).
func (r *CompanionResolver) MyCompanion(ctx context.Context) (any, error) {
	tenantID, gcid := r.CtxIdentity(ctx)
	authCtx := withResolverAuthCtx(ctx, tenantID, gcid)
	v, err := r.Upstream.GetCompanion(authCtx, tenantID, gcid)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, nil
	}
	// Project upstream's loose JSON into the federation-friendly shape.
	// CRITICAL: never copy fields that begin with `_ai_` or `model_` —
	// those would leak AI agent details, which the SDL forbids.
	//
	// gcid / name / createdAt / updatedAt are carried through because the live
	// SPA query (chora-web core/graphql/queries.ts QUERY_MY_FAMILIAR, mapped in
	// features/choraverse/services/familiar.service.ts mapFamiliarProfile)
	// reads them; the pre-fix shape omitted all four, so the Familiar profile
	// rendered with undefined identity + timestamps. `ownerGcid` is the
	// historical key kept for existing consumers.
	out := map[string]any{
		"id":           m["companion_id"],
		"gcid":         gcid,
		"name":         m["name"],
		"ownerGcid":    gcid,
		"species":      derefSpecies(m),
		"level":        m["level"],
		"mood":         derefMood(m),
		"bondedAt":     "2026-04-01T08:00:00Z",
		"equippedSkin": nil,
		"createdAt":    m["created_at"],
		"updatedAt":    m["updated_at"],
	}
	return out, nil
}

// derefSpecies returns the species enum string. Skeleton-mode upstream
// doesn't carry species, so we infer a deterministic one from the gcid.
func derefSpecies(m map[string]any) string {
	if s, ok := m["species"].(string); ok && s != "" {
		return s
	}
	if name, ok := m["name"].(string); ok && name != "" {
		switch name[0] {
		case 'A', 'B', 'C':
			return "OWL"
		case 'D', 'E', 'F':
			return "FOX"
		case 'G', 'H', 'I':
			return "CAT"
		}
	}
	return "OWL"
}

// derefMood returns the mood enum string from the upstream payload.
func derefMood(m map[string]any) string {
	if mood, ok := m["mood"].(string); ok && mood != "" {
		// Upstream uses lowercase; we normalise to enum form.
		switch mood {
		case "curious":
			return "CURIOUS"
		case "proud":
			return "PROUD"
		case "sleepy":
			return "SLEEPY"
		case "energetic":
			return "ENERGETIC"
		case "contemplative":
			return "CONTEMPLATIVE"
		}
	}
	return "CURIOUS"
}
