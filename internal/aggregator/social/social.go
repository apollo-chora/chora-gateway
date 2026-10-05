// Package social aggregates the C+ Phyllis MVP social paths per
// CHO-1457 (`chora-contracts/openapi/bff-gateway.yaml` social section).
//
// Phyllis MVP scope is intentionally narrow — basic social discovery
// only:
//
//	GET    /v1/feed                                — paginated feed
//	GET    /v1/me/social                           — caller SocialProfile
//	POST   /v1/atoms/{atom_id}/share               — share atom to feed
//	POST   /v1/posts/{post_id}/reactions           — react to a post
//	DELETE /v1/posts/{post_id}/reactions/{rid}     — remove a reaction
//	GET    /v1/posts/{post_id}/comments            — threaded comments
//	POST   /v1/posts/{post_id}/comments            — add comment / reply
//	GET    /v1/discovery/courses/public            — public catalogue (Phyllis Step 5)
//
// Out of scope (parked for M14+ per delta plan): Duels, Leaderboards,
// Three-Currency Economy, Reward Vault, Territory Conquest, Companion
// Missions, Refer-a-Friend.
//
// Implementation note: this is a STUB AGGREGATOR. Read paths return 200
// with empty `data` arrays; write paths return 501 Not Implemented with
// a TODO body referencing CHO-1457. Full implementation lands when
// chora-sharing learner-facing API ships (M12+) — at that point the
// aggregator will fan out to chora-sharing the same way `phyllis`
// fans out to identity/tenancy/creation/etc. The TenantID + GCID
// extracted from AuthCtx are echoed back where relevant so the C+
// Angular surface can compile + e2e against real shapes.
//
// Per `feedback_no_inline_config`, the SVC_SHARING_URL + SVC_DELIVERY_URL
// env vars drive future fan-out behaviour. They are NOT consulted in
// stub mode.
package social

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
)

// Config wires the downstream URLs the aggregator uses for fan-out.
// Reads from SVC_SHARING_URL + SVC_DELIVERY_URL at cmd/server boot.
type Config struct {
	// SharingURL is the chora-sharing service base. Required for
	// POST /v1/atoms/{atom_id}/share fan-out (M14.iter5.B).
	SharingURL string

	// DeliveryURL points at chora-delivery for the public-catalog
	// cross-link used by GET /v1/discovery/courses/public. Stub returns
	// empty data when blank.
	DeliveryURL string

	// PerCallTimeout caps each downstream call. Defaults to 5s.
	PerCallTimeout time.Duration
}

// Response mirrors the phyllis.Response shape for cross-aggregator
// consistency.
type Response struct {
	Status  int
	Headers http.Header
	Body    []byte
}

// Aggregator is the stateless C+ social aggregator.
type Aggregator struct {
	cfg    Config
	client *http.Client
}

// New constructs an Aggregator.
func New(cfg Config) *Aggregator {
	if cfg.PerCallTimeout == 0 {
		cfg.PerCallTimeout = 5 * time.Second
	}
	return &Aggregator{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.PerCallTimeout},
	}
}

// SharingBaseURL returns the chora-sharing downstream base URL the
// aggregator fans out to. Used by the WS upgrade proxy in
// social_handler.go to dial chora-sharing directly for /v1/duels/{id}/ws
// (a verbatim HTTP proxy cannot carry the 101 Switching Protocols
// handshake). Mirrors gatewayproxy.DeliveryBaseURL.
func (a *Aggregator) SharingBaseURL() string {
	return a.cfg.SharingURL
}

// StampDownstreamHeaders stamps the canonical mesh-trust headers onto an
// outbound request — the same headers call() sets (Authorization,
// X-Tenant-Id, gcid, traceparent, Idempotency-Key, servicemesh claims).
// Used by the WS upgrade proxy to stamp identity onto the hijacked upgrade
// request before writing it to the downstream conn, so chora-sharing's WS
// handler (which requires X-Tenant-Id + gcid) accepts the handshake.
func (a *Aggregator) StampDownstreamHeaders(req *http.Request, auth phyllis.AuthCtx) {
	if auth.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+auth.Bearer)
	}
	if auth.Traceparent != "" {
		req.Header.Set("traceparent", auth.Traceparent)
	}
	if auth.TenantID != "" {
		req.Header.Set("X-Tenant-Id", auth.TenantID)
	}
	if auth.GCID != "" {
		req.Header.Set("gcid", auth.GCID)
	}
	if auth.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", auth.IdempotencyKey)
	}
	// Roles → x-mesh-user-roles. Mirrors call() below: phyllis.AuthCtx already
	// carries the validated typed roles, and NOT forwarding them denies 100% of
	// any role-gated downstream call, because every Chora role gate fails CLOSED.
	// Silently, too: the denial only appears once such a gate exists downstream.
	// See mesh_roles_guard_test.go, which enumerates every marshal site.
	mesh := servicemesh.MarshalToHeaders(servicemesh.MeshClaims{
		GCID: auth.GCID, TenantID: auth.TenantID, Roles: auth.Roles, RoleSummary: auth.RoleSummary,
	})
	for k, vs := range mesh {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
}

// callResult mirrors the phyllis aggregator shape for cross-aggregator
// classify() consistency.
type callResult struct {
	status int
	body   []byte
	header http.Header
	err    error
}

// call fans out to a downstream service, propagating auth + traceparent +
// mesh metadata.
func (a *Aggregator) call(ctx context.Context, method, urlStr string, body []byte, auth phyllis.AuthCtx) callResult {
	if urlStr == "" {
		return callResult{err: fmt.Errorf("social: empty url for %s", method)}
	}
	callCtx, cancel := context.WithTimeout(ctx, a.cfg.PerCallTimeout)
	defer cancel()
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(callCtx, method, urlStr, bodyReader)
	if err != nil {
		return callResult{err: fmt.Errorf("social: build request: %w", err)}
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+auth.Bearer)
	}
	if auth.Traceparent != "" {
		req.Header.Set("traceparent", auth.Traceparent)
	}
	if auth.TenantID != "" {
		req.Header.Set("X-Tenant-Id", auth.TenantID)
	}
	if auth.GCID != "" {
		req.Header.Set("gcid", auth.GCID)
	}
	// ADR-196 D4: forward the inbound Idempotency-Key on write fan-outs.
	// chora-sharing POST /v1/atoms/{id}/share 400s without it (§7.1 step 5).
	// Empty for read paths — never synthesised.
	if auth.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", auth.IdempotencyKey)
	}
	// Roles → x-mesh-user-roles. phyllis.AuthCtx already carries the validated
	// typed roles; NOT forwarding them denies 100% of any role-gated downstream
	// call (every Chora role gate fails CLOSED). See mesh_roles_guard_test.go.
	mesh := servicemesh.MarshalToHeaders(servicemesh.MeshClaims{
		GCID: auth.GCID, TenantID: auth.TenantID, Roles: auth.Roles, RoleSummary: auth.RoleSummary,
	})
	for k, vs := range mesh {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return callResult{err: err}
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return callResult{status: resp.StatusCode, body: out, header: resp.Header}
}

func classify(cr callResult) Response {
	if cr.err != nil {
		if errors.Is(cr.err, context.DeadlineExceeded) || errors.Is(cr.err, context.Canceled) {
			return Response{Status: http.StatusGatewayTimeout, Body: mustMarshalJSON(map[string]any{
				"error":   "GATEWAY_UPSTREAM_TIMEOUT",
				"message": cr.err.Error(),
			})}
		}
		return Response{Status: http.StatusBadGateway, Body: mustMarshalJSON(map[string]any{
			"error":   "GATEWAY_UPSTREAM_ERROR",
			"message": cr.err.Error(),
		})}
	}
	if cr.status >= 500 {
		return Response{Status: http.StatusBadGateway, Body: mustMarshalJSON(map[string]any{
			"error":   "GATEWAY_UPSTREAM_5XX",
			"message": fmt.Sprintf("upstream returned %d", cr.status),
		})}
	}
	return Response{Status: cr.status, Body: cr.body, Headers: cr.header}
}

// -----------------------------------------------------------------------------
// GET /v1/feed
// -----------------------------------------------------------------------------

// GetFeedParams is the input shape for GetFeed.
type GetFeedParams struct {
	Auth     phyllis.AuthCtx
	TenantID string
	Limit    int
	Cursor   string
}

// GetFeed returns the paginated atomic-posts feed.
//
// Phase 6: fans out to chora-sharing:GET /v1/feed when SharingURL is set.
// Stub fallback (empty page) retained when SharingURL is empty so the C+
// FeedTimelineComponent renders its empty-state in unconfigured environments.
func (a *Aggregator) GetFeed(ctx context.Context, params GetFeedParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		body := mustMarshalJSON(map[string]any{
			"data":        []any{},
			"next_cursor": "",
		})
		return ok200(body), nil
	}
	return classify(a.call(ctx, http.MethodGet,
		a.cfg.SharingURL+"/v1/feed/shared-atoms", nil, params.Auth)), nil
}

// -----------------------------------------------------------------------------
// GET /v1/me/social
// -----------------------------------------------------------------------------

// GetMySocial returns the caller's per-class reusable SocialProfile.
//
// Phase 6: fans out to chora-sharing:GET /v1/connections when SharingURL is
// set (chora-sharing has no /me/social endpoint — the connections list is
// the closest equivalent). Stub fallback (echo GCID + zero counts) retained
// when SharingURL is empty so the C+ surface renders zero-state in
// unconfigured envs.
func (a *Aggregator) GetMySocial(ctx context.Context, auth phyllis.AuthCtx) (Response, error) {
	if a.cfg.SharingURL == "" {
		body := mustMarshalJSON(map[string]any{
			"gcid":            auth.GCID,
			"display_name":    "",
			"avatar_url":      "",
			"atom_count":      0,
			"follower_count":  0,
			"following_count": 0,
			"digital_skins":   []any{},
		})
		return ok200(body), nil
	}
	return classify(a.call(ctx, http.MethodGet,
		a.cfg.SharingURL+"/v1/connections", nil, auth)), nil
}

// -----------------------------------------------------------------------------
// POST /v1/atoms/{atom_id}/share
// -----------------------------------------------------------------------------

// ShareAtomParams is the input for sharing an atom to the feed.
type ShareAtomParams struct {
	Auth   phyllis.AuthCtx
	AtomID string
	// Body is the raw shareAtomRequest JSON {atom_revision_id, caption,
	// license_terms, royalty_rate} (chora-sharing §7.1). The BFF passes it
	// through opaquely — it does not reshape the share DTO — mirroring the
	// ChallengeDuel body-passthrough pattern. atom_id comes from the path;
	// author_gcid + tenant_id come from the identity headers, not the body.
	Body []byte
}

// ShareAtomToFeed shares an atom by fanning out to chora-sharing:
// POST /v1/atoms/{atom_id}/share (ADR-196 D4 reconciled this from the dead
// /api/posts route). The body is forwarded opaquely + the inbound
// Idempotency-Key header is propagated (chora-sharing 400s without it per
// §7.1 step 5). When SharingURL is empty (misconfigured BFF) returns 502.
func (a *Aggregator) ShareAtomToFeed(ctx context.Context, params ShareAtomParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return Response{Status: http.StatusBadGateway, Body: mustMarshalJSON(map[string]any{
			"error":   "GATEWAY_NOT_CONFIGURED",
			"message": "SVC_SHARING_URL must be set for atom share fan-out",
		})}, nil
	}
	return classify(a.call(ctx, http.MethodPost,
		a.cfg.SharingURL+"/v1/atoms/"+params.AtomID+"/share", params.Body, params.Auth)), nil
}

// RevokeShareFromFeed revokes a previously shared atom by fanning out to
// chora-sharing:DELETE /v1/atoms/{atom_id}/share. The downstream appends an
// EventRevoked (R-15-A: the feed entry is immutable, never deleted). Returns
// 204 on success or when the share was already revoked / never shared
// (idempotent). When SharingURL is empty (misconfigured BFF) returns 502.
func (a *Aggregator) RevokeShareFromFeed(ctx context.Context, params ShareAtomParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return Response{Status: http.StatusBadGateway, Body: mustMarshalJSON(map[string]any{
			"error":   "GATEWAY_NOT_CONFIGURED",
			"message": "SVC_SHARING_URL must be set for atom share fan-out",
		})}, nil
	}
	return classify(a.call(ctx, http.MethodDelete,
		a.cfg.SharingURL+"/v1/atoms/"+params.AtomID+"/share", nil, params.Auth)), nil
}

// -----------------------------------------------------------------------------
// POST /v1/posts/{post_id}/reactions
// -----------------------------------------------------------------------------

// CreateReactionParams is the input for adding a reaction.
type CreateReactionParams struct {
	Auth   phyllis.AuthCtx
	PostID string
	Type   string // like | insight | question
}

// CreateReaction adds a reaction to a post.
//
// Fans out to chora-sharing:POST /v1/posts/{id}/reactions when SharingURL is
// set (ADR-196 D4 reconciled the missing /v1 prefix). Stub 501 retained for
// unconfigured envs.
func (a *Aggregator) CreateReaction(ctx context.Context, params CreateReactionParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return notImplemented501("create reaction"), nil
	}
	body := mustMarshalJSON(map[string]any{"kind": params.Type})
	return classify(a.call(ctx, http.MethodPost,
		a.cfg.SharingURL+"/v1/posts/"+params.PostID+"/reactions", body, params.Auth)), nil
}

// -----------------------------------------------------------------------------
// DELETE /v1/posts/{post_id}/reactions/{reaction_id}
// -----------------------------------------------------------------------------

// DeleteReactionParams is the input for removing a reaction.
type DeleteReactionParams struct {
	Auth       phyllis.AuthCtx
	PostID     string
	ReactionID string
}

// DeleteReaction removes a reaction. Idempotent: 204 even when the
// reaction does not exist.
//
// Fans out to chora-sharing:DELETE /v1/posts/{id}/reactions/{rid} when
// SharingURL is set (ADR-196 D4 reconciled the missing /v1 prefix). Stub 204
// retained for unconfigured envs.
func (a *Aggregator) DeleteReaction(ctx context.Context, params DeleteReactionParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return Response{Status: http.StatusNoContent}, nil
	}
	return classify(a.call(ctx, http.MethodDelete,
		a.cfg.SharingURL+"/v1/posts/"+params.PostID+"/reactions/"+params.ReactionID, nil, params.Auth)), nil
}

// -----------------------------------------------------------------------------
// GET /v1/posts/{post_id}/comments
// -----------------------------------------------------------------------------

// GetCommentsParams is the input for fetching comments.
type GetCommentsParams struct {
	Auth   phyllis.AuthCtx
	PostID string
	Cursor string
}

// GetComments returns the threaded comment tree for a post.
//
// Fans out to chora-sharing:GET /v1/posts/{id}/comments when SharingURL is
// set (ADR-196 D4 reconciled the missing /v1 prefix). Stub empty page
// retained for unconfigured envs so the AtomCommentThreadComponent renders
// its empty-state.
func (a *Aggregator) GetComments(ctx context.Context, params GetCommentsParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		body := mustMarshalJSON(map[string]any{
			"data":        []any{},
			"next_cursor": "",
		})
		return ok200(body), nil
	}
	return classify(a.call(ctx, http.MethodGet,
		a.cfg.SharingURL+"/v1/posts/"+params.PostID+"/comments", nil, params.Auth)), nil
}

// -----------------------------------------------------------------------------
// POST /v1/posts/{post_id}/comments
// -----------------------------------------------------------------------------

// AddCommentParams is the input for adding a comment.
type AddCommentParams struct {
	Auth            phyllis.AuthCtx
	PostID          string
	Body            string
	ParentCommentID string
}

// AddComment appends a comment (or reply when ParentCommentID is set)
// to a post.
//
// Fans out to chora-sharing:POST /v1/posts/{id}/comments when SharingURL is
// set (ADR-196 D4 reconciled the missing /v1 prefix). Stub 501 retained for
// unconfigured envs.
func (a *Aggregator) AddComment(ctx context.Context, params AddCommentParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return notImplemented501("add comment"), nil
	}
	body := mustMarshalJSON(map[string]any{
		"body":              params.Body,
		"parent_comment_id": params.ParentCommentID,
	})
	return classify(a.call(ctx, http.MethodPost,
		a.cfg.SharingURL+"/v1/posts/"+params.PostID+"/comments", body, params.Auth)), nil
}

// -----------------------------------------------------------------------------
// GET /v1/discovery/courses/public
// -----------------------------------------------------------------------------

// GetPublicCoursesParams is the input for the public-catalogue read.
type GetPublicCoursesParams struct {
	Auth     phyllis.AuthCtx
	TenantID string
}

// GetPublicCourses lists public-visibility courses for cross-surface
// discovery (Phyllis Step 5 — Mr. Chen visibility).
//
// Phase 6: fans out to chora-delivery:GET /courses?public=true when
// DeliveryURL is set. Stub empty list retained for unconfigured envs so
// the PublicCoursesGridComponent renders its empty-state.
func (a *Aggregator) GetPublicCourses(ctx context.Context, params GetPublicCoursesParams) (Response, error) {
	if a.cfg.DeliveryURL == "" {
		body := mustMarshalJSON(map[string]any{
			"data": []any{},
		})
		return ok200(body), nil
	}
	return classify(a.call(ctx, http.MethodGet,
		a.cfg.DeliveryURL+"/courses?public=true", nil, params.Auth)), nil
}

// -----------------------------------------------------------------------------
// Atom Sharing Redesign (specs/001-atom-sharing-redesign) — T016.
//
// The new RMM L2 sharing routes (per contracts/rest-routes-api-catalogue.md +
// the extended bff-gateway.yaml social section): the shared-atom feed, the
// duel arena, and the leaderboard. Each fans out to chora-sharing when
// SharingURL is set; the per-route REAL fan-out is completed in its user-
// story phase (US1 feed / US3 duels / US4 leaderboard). This skeleton wires
// the route → aggregator → downstream path + propagates tenant/gcid so the
// C+ surface compiles + e2e against real shapes once the downstream handlers
// ship. R-12: chora-web NEVER calls chora-sharing directly — only via this
// BFF aggregator.
// -----------------------------------------------------------------------------

// GetSharedAtomsParams is the input for the shared-atom feed read.
type GetSharedAtomsParams struct {
	Auth               phyllis.AuthCtx
	TenantID           string
	ViewerGCID         string
	Cursor             string
	Limit              int
	TopicFilter        string
	QuestionTypeFilter string
	Scope              string // following | tenant | global
}

// GetSharedAtomsFeed returns the paginated shared-atom feed (RMM L2:
// GET /v1/feed/shared-atoms; pagination default 20/max 100 per design §4.1).
//
// Fans out to chora-sharing:GET /v1/feed/shared-atoms when SharingURL is set.
// Stub empty page retained when SharingURL is empty so the C+ feed renders its
// empty-state in unconfigured environments. The real downstream handler lands
// in US1 Phase 3 (T024).
func (a *Aggregator) GetSharedAtomsFeed(ctx context.Context, params GetSharedAtomsParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		body := mustMarshalJSON(map[string]any{
			"data":        []any{},
			"next_cursor": "",
		})
		return ok200(body), nil
	}
	q := url.Values{}
	if params.Cursor != "" {
		q.Set("cursor", params.Cursor)
	}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.TopicFilter != "" {
		q.Set("topic_filter", params.TopicFilter)
	}
	if params.QuestionTypeFilter != "" {
		q.Set("question_type_filter", params.QuestionTypeFilter)
	}
	if params.Scope != "" {
		q.Set("scope", params.Scope)
	}
	downstream := a.cfg.SharingURL + "/v1/feed/shared-atoms"
	if enc := q.Encode(); enc != "" {
		downstream += "?" + enc
	}
	return classify(a.call(ctx, http.MethodGet, downstream, nil, params.Auth)), nil
}

// GetLeaderboardParams is the input for the leaderboard read.
type GetLeaderboardParams struct {
	Auth          phyllis.AuthCtx
	TenantID      string
	Scope         string // tenant | class | global
	ScopeTargetID string
	Metric        string // xp | duel_wins | duel_elo | reputation | streak_days
	Period        string // weekly | monthly | all-time
	Season        string
	Limit         int
	Cursor        string
}

// GetLeaderboard returns the atom-completion-driven leaderboard (RMM L2:
// GET /v1/leaderboard; ELO from ranked duels + XP from completed atoms).
//
// Fans out to chora-sharing:GET /v1/leaderboard when SharingURL is set. Stub
// empty page retained for unconfigured envs. The real downstream handler
// lands in US4 Phase 6 (T060).
func (a *Aggregator) GetLeaderboard(ctx context.Context, params GetLeaderboardParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		body := mustMarshalJSON(map[string]any{
			"data":        []any{},
			"next_cursor": "",
		})
		return ok200(body), nil
	}
	q := url.Values{}
	if params.Scope != "" {
		q.Set("scope", params.Scope)
	}
	if params.ScopeTargetID != "" {
		q.Set("scope_target_id", params.ScopeTargetID)
	}
	if params.Metric != "" {
		q.Set("metric", params.Metric)
	}
	if params.Period != "" {
		q.Set("period", params.Period)
	}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Cursor != "" {
		q.Set("cursor", params.Cursor)
	}
	downstream := a.cfg.SharingURL + "/v1/leaderboard"
	if enc := q.Encode(); enc != "" {
		downstream += "?" + enc
	}
	return classify(a.call(ctx, http.MethodGet, downstream, nil, params.Auth)), nil
}

// -----------------------------------------------------------------------------
// Duels
// -----------------------------------------------------------------------------

// GetDuelParams is the input for reading a duel (or listing duels).
type GetDuelParams struct {
	Auth     phyllis.AuthCtx
	TenantID string
	DuelID   string
}

func (a *Aggregator) GetDuel(ctx context.Context, params GetDuelParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return notImplemented501("duel read"), nil
	}
	path := a.cfg.SharingURL + "/v1/duels"
	if params.DuelID != "" {
		path += "/" + params.DuelID
	}
	return classify(a.call(ctx, http.MethodGet, path, nil, params.Auth)), nil
}

// GetMyRatingParams is the input for the caller's duel rating.
type GetMyRatingParams struct {
	Auth     phyllis.AuthCtx
	TenantID string
}

// GetMyRating fans out to chora-sharing:GET /v1/duels/my-rating.
func (a *Aggregator) GetMyRating(ctx context.Context, params GetMyRatingParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return notImplemented501("duel rating"), nil
	}
	return classify(a.call(ctx, http.MethodGet,
		a.cfg.SharingURL+"/v1/duels/my-rating", nil, params.Auth)), nil
}

// GetDuelLeaderboardParams is the input for the duel leaderboard read.
type GetDuelLeaderboardParams struct {
	Auth     phyllis.AuthCtx
	TenantID string
}

// GetDuelLeaderboard fans out to chora-sharing:GET /v1/duels/leaderboard.
func (a *Aggregator) GetDuelLeaderboard(ctx context.Context, params GetDuelLeaderboardParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return notImplemented501("duel leaderboard"), nil
	}
	return classify(a.call(ctx, http.MethodGet,
		a.cfg.SharingURL+"/v1/duels/leaderboard", nil, params.Auth)), nil
}

// QueueDuelParams is the input for entering the matchmaking pool.
type QueueDuelParams struct {
	Auth     phyllis.AuthCtx
	TenantID string
	Body     []byte
}

// QueueDuel fans out to chora-sharing:POST /v1/duels/queue.
// The Idempotency-Key is propagated via auth.IdempotencyKey.
func (a *Aggregator) QueueDuel(ctx context.Context, params QueueDuelParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return Response{Status: http.StatusBadGateway, Body: mustMarshalJSON(map[string]any{
			"error":   "GATEWAY_NOT_CONFIGURED",
			"message": "SVC_SHARING_URL must be set for duel matchmaking",
		})}, nil
	}
	return classify(a.call(ctx, http.MethodPost,
		a.cfg.SharingURL+"/v1/duels/queue", params.Body, params.Auth)), nil
}

// CancelQueueDuel fans out to chora-sharing:DELETE /v1/duels/queue.
func (a *Aggregator) CancelQueueDuel(ctx context.Context, params QueueDuelParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return Response{Status: http.StatusBadGateway, Body: mustMarshalJSON(map[string]any{
			"error":   "GATEWAY_NOT_CONFIGURED",
			"message": "SVC_SHARING_URL must be set for duel matchmaking",
		})}, nil
	}
	return classify(a.call(ctx, http.MethodDelete,
		a.cfg.SharingURL+"/v1/duels/queue", nil, params.Auth)), nil
}

// HeartbeatQueue fans out to chora-sharing:POST /v1/duels/queue/heartbeat.
func (a *Aggregator) HeartbeatQueue(ctx context.Context, params QueueDuelParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return Response{Status: http.StatusBadGateway, Body: mustMarshalJSON(map[string]any{
			"error":   "GATEWAY_NOT_CONFIGURED",
			"message": "SVC_SHARING_URL must be set for duel matchmaking",
		})}, nil
	}
	return classify(a.call(ctx, http.MethodPost,
		a.cfg.SharingURL+"/v1/duels/queue/heartbeat", nil, params.Auth)), nil
}

// QueueStatus fans out to chora-sharing:GET /v1/duels/queue/status.
func (a *Aggregator) QueueStatus(ctx context.Context, params QueueDuelParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return Response{Status: http.StatusBadGateway, Body: mustMarshalJSON(map[string]any{
			"error":   "GATEWAY_NOT_CONFIGURED",
			"message": "SVC_SHARING_URL must be set for duel matchmaking",
		})}, nil
	}
	return classify(a.call(ctx, http.MethodGet,
		a.cfg.SharingURL+"/v1/duels/queue/status", nil, params.Auth)), nil
}

type SubmitDuelAnswerParams struct {
	Auth   phyllis.AuthCtx
	DuelID string
	Body   []byte
}

func (a *Aggregator) SubmitDuelAnswer(ctx context.Context, params SubmitDuelAnswerParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return Response{Status: http.StatusBadGateway, Body: mustMarshalJSON(map[string]any{
			"error":   "GATEWAY_NOT_CONFIGURED",
			"message": "SVC_SHARING_URL must be set for duel fan-out",
		})}, nil
	}
	return classify(a.call(ctx, http.MethodPost,
		a.cfg.SharingURL+"/v1/duels/"+params.DuelID+"/answer", params.Body, params.Auth)), nil
}

type DuelWSParams struct {
	Auth   phyllis.AuthCtx
	DuelID string
}

func (a *Aggregator) DuelWS(ctx context.Context, params DuelWSParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return Response{Status: http.StatusBadGateway, Body: mustMarshalJSON(map[string]any{
			"error":   "GATEWAY_NOT_CONFIGURED",
			"message": "SVC_SHARING_URL must be set for duel fan-out",
		})}, nil
	}
	return classify(a.call(ctx, http.MethodGet,
		a.cfg.SharingURL+"/v1/duels/"+params.DuelID+"/ws", nil, params.Auth)), nil
}

// -----------------------------------------------------------------------------
// Profiler
// -----------------------------------------------------------------------------

type GetProfileParams struct {
	Auth phyllis.AuthCtx
}

func (a *Aggregator) GetProfile(ctx context.Context, params GetProfileParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return notImplemented501("profile read"), nil
	}
	return classify(a.call(ctx, http.MethodGet,
		a.cfg.SharingURL+"/v1/me/profile", nil, params.Auth)), nil
}

type GenerateProfileParams struct {
	Auth phyllis.AuthCtx
	Body []byte
}

func (a *Aggregator) GenerateProfile(ctx context.Context, params GenerateProfileParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return Response{Status: http.StatusBadGateway, Body: mustMarshalJSON(map[string]any{
			"error":   "GATEWAY_NOT_CONFIGURED",
			"message": "SVC_SHARING_URL must be set for profile fan-out",
		})}, nil
	}
	return classify(a.call(ctx, http.MethodPost,
		a.cfg.SharingURL+"/v1/me/profile/generate", params.Body, params.Auth)), nil
}

type UpdateProfileTagsParams struct {
	Auth phyllis.AuthCtx
	Body []byte
}

func (a *Aggregator) UpdateProfileTags(ctx context.Context, params UpdateProfileTagsParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return Response{Status: http.StatusBadGateway, Body: mustMarshalJSON(map[string]any{
			"error":   "GATEWAY_NOT_CONFIGURED",
			"message": "SVC_SHARING_URL must be set for profile fan-out",
		})}, nil
	}
	return classify(a.call(ctx, http.MethodPut,
		a.cfg.SharingURL+"/v1/me/profile/tags", params.Body, params.Auth)), nil
}

// -----------------------------------------------------------------------------
// Connections (following / followers / blocked)
// -----------------------------------------------------------------------------

type ListConnectionsParams struct {
	Auth   phyllis.AuthCtx
	Type   string // following | followers | blocked
	Cursor string
	Limit  int
}

func (a *Aggregator) ListConnections(ctx context.Context, params ListConnectionsParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return ok200(mustMarshalJSON(map[string]any{
			"connections": []any{},
			"next_cursor": "",
		})), nil
	}
	q := url.Values{}
	if params.Type != "" {
		q.Set("type", params.Type)
	}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Cursor != "" {
		q.Set("cursor", params.Cursor)
	}
	downstream := a.cfg.SharingURL + "/v1/connections"
	if enc := q.Encode(); enc != "" {
		downstream += "?" + enc
	}
	return classify(a.call(ctx, http.MethodGet, downstream, nil, params.Auth)), nil
}

// FollowUser fans out to chora-sharing: POST /v1/connections/{target_gcid}/follow.
func (a *Aggregator) FollowUser(ctx context.Context, auth phyllis.AuthCtx, targetGCID string) (Response, error) {
	if a.cfg.SharingURL == "" {
		return Response{Status: http.StatusBadGateway, Body: mustMarshalJSON(map[string]any{
			"error": "GATEWAY_NOT_CONFIGURED",
		})}, nil
	}
	return classify(a.call(ctx, http.MethodPost,
		a.cfg.SharingURL+"/v1/connections/"+targetGCID+"/follow", nil, auth)), nil
}

// UnfollowUser fans out to chora-sharing: DELETE /v1/connections/{target_gcid}/follow.
func (a *Aggregator) UnfollowUser(ctx context.Context, auth phyllis.AuthCtx, targetGCID string) (Response, error) {
	if a.cfg.SharingURL == "" {
		return Response{Status: http.StatusBadGateway, Body: mustMarshalJSON(map[string]any{
			"error": "GATEWAY_NOT_CONFIGURED",
		})}, nil
	}
	return classify(a.call(ctx, http.MethodDelete,
		a.cfg.SharingURL+"/v1/connections/"+targetGCID+"/follow", nil, auth)), nil
}

// BlockUser fans out to chora-sharing: POST /v1/connections/{target_gcid}/block.
func (a *Aggregator) BlockUser(ctx context.Context, auth phyllis.AuthCtx, targetGCID string) (Response, error) {
	if a.cfg.SharingURL == "" {
		return Response{Status: http.StatusBadGateway, Body: mustMarshalJSON(map[string]any{
			"error": "GATEWAY_NOT_CONFIGURED",
		})}, nil
	}
	return classify(a.call(ctx, http.MethodPost,
		a.cfg.SharingURL+"/v1/connections/"+targetGCID+"/block", nil, auth)), nil
}

// UnblockUser fans out to chora-sharing: DELETE /v1/connections/{target_gcid}/block.
func (a *Aggregator) UnblockUser(ctx context.Context, auth phyllis.AuthCtx, targetGCID string) (Response, error) {
	if a.cfg.SharingURL == "" {
		return Response{Status: http.StatusBadGateway, Body: mustMarshalJSON(map[string]any{
			"error": "GATEWAY_NOT_CONFIGURED",
		})}, nil
	}
	return classify(a.call(ctx, http.MethodDelete,
		a.cfg.SharingURL+"/v1/connections/"+targetGCID+"/block", nil, auth)), nil
}

// -----------------------------------------------------------------------------
// Relationship ops — /v1/connections/* subtree (ADR-230 B-lite.2)
// -----------------------------------------------------------------------------

// RelationshipOpParams is the input shape for RelationshipOp. Subpath is the
// path below /v1/connections/ (e.g. "follows",
// "friend-requests/{gcid}/accept") — the HTTP handler owns route parsing;
// this method owns only the forward.
type RelationshipOpParams struct {
	Auth    phyllis.AuthCtx
	Method  string
	Subpath string
	Body    []byte
}

// RelationshipOp forwards one relationship write / pending-list read to
// chora-sharing verbatim (ADR-196 D4 opaque passthrough — the BFF never
// reshapes relationship bodies). Unconfigured SharingURL fails loud with
// 502: a relationship write must never fake a success, and the pending
// lists must never fake an empty state (contrast the read-led feed stubs).
func (a *Aggregator) RelationshipOp(ctx context.Context, params RelationshipOpParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return Response{Status: http.StatusBadGateway, Body: mustMarshalJSON(map[string]any{
			"error":   "GATEWAY_NOT_CONFIGURED",
			"message": "SVC_SHARING_URL must be set for relationship fan-out",
		})}, nil
	}
	downstream := a.cfg.SharingURL + "/v1/connections/" + params.Subpath
	return classify(a.call(ctx, params.Method, downstream, params.Body, params.Auth)), nil
}

// -----------------------------------------------------------------------------
// Companion-milestone drafts + share preference (CHO-2258)
//
// Two opaque passthroughs (ADR-196 D4 — the BFF forwards bodies verbatim and
// never reshapes them) for the owner-facing C+ milestone surface that completes
// the CHO-2203 lane.
//
// ⚠ The downstream prefixes below are hardcoded, and they must stay byte-exact:
// the ns/sharing Istio AuthorizationPolicy is a strict per-path allowlist that
// already carries /v1/me/post-drafts, /v1/me/post-drafts/* and
// /v1/me/preferences/familiar-milestone-share (live-verified 2026-07-17; the
// sharing upstream renames at its own ADR-254 window, so the downstream path
// below stays pre-rename while the gateway's public path gains the companion
// name plus an alias). Any
// drift is a mesh 403 that no unit test can see. Hardcoding the prefix (rather
// than forwarding a caller-supplied path) also keeps the fan-out target closed.
//
// Both fail LOUD when SharingURL is unset — including the READ. That is a
// deliberate departure from the read-led feed stubs above: an empty feed is a
// normal state, but an empty drafts list is a claim about the learner's OWN
// queued milestones, and stubbing it would say "nothing is waiting for you"
// when the truth is the gateway is misconfigured.
// -----------------------------------------------------------------------------

// MilestoneDraftsOpParams addresses /v1/me/post-drafts and its subpaths.
// Subpath is "" for the list, or "/{draft_id}/publish" | "/{draft_id}/discard".
type MilestoneDraftsOpParams struct {
	Auth    phyllis.AuthCtx
	Method  string
	Subpath string
	Body    []byte
}

// MilestoneDraftsOp forwards one drafts operation to chora-sharing verbatim.
func (a *Aggregator) MilestoneDraftsOp(ctx context.Context, params MilestoneDraftsOpParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return gatewayNotConfigured("SVC_SHARING_URL must be set for milestone-drafts fan-out"), nil
	}
	downstream := a.cfg.SharingURL + "/v1/me/post-drafts" + params.Subpath
	return classify(a.call(ctx, params.Method, downstream, params.Body, params.Auth)), nil
}

// MilestoneSharePrefOpParams addresses the single-preference resource.
type MilestoneSharePrefOpParams struct {
	Auth   phyllis.AuthCtx
	Method string
	Body   []byte
}

// MilestoneSharePrefOp forwards the Companion-milestone share-preference read or
// write to chora-sharing verbatim.
func (a *Aggregator) MilestoneSharePrefOp(ctx context.Context, params MilestoneSharePrefOpParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		// A preference write that fakes a success leaves the learner believing
		// they opted into auto while every milestone keeps silently drafting.
		return gatewayNotConfigured("SVC_SHARING_URL must be set for milestone share-preference fan-out"), nil
	}
	downstream := a.cfg.SharingURL + "/v1/me/preferences/familiar-milestone-share" // sharing upstream renames at its window
	return classify(a.call(ctx, params.Method, downstream, params.Body, params.Auth)), nil
}

// gatewayNotConfigured is the canonical fail-loud 502 for an unset upstream URL.
func gatewayNotConfigured(msg string) Response {
	return Response{Status: http.StatusBadGateway, Body: mustMarshalJSON(map[string]any{
		"error":   "GATEWAY_NOT_CONFIGURED",
		"message": msg,
	})}
}

// -----------------------------------------------------------------------------
// Bookmarks (save atom to collection)
// -----------------------------------------------------------------------------

type BookmarkParams struct {
	Auth   phyllis.AuthCtx
	AtomID string
}

func (a *Aggregator) BookmarkAtom(ctx context.Context, params BookmarkParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return Response{Status: http.StatusBadGateway, Body: mustMarshalJSON(map[string]any{
			"error":   "GATEWAY_NOT_CONFIGURED",
			"message": "SVC_SHARING_URL must be set for bookmark fan-out",
		})}, nil
	}
	path := a.cfg.SharingURL + "/v1/atoms/" + params.AtomID + "/bookmark"
	return classify(a.call(ctx, http.MethodPost, path, nil, params.Auth)), nil
}

func (a *Aggregator) UnbookmarkAtom(ctx context.Context, params BookmarkParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return Response{Status: http.StatusBadGateway, Body: mustMarshalJSON(map[string]any{
			"error":   "GATEWAY_NOT_CONFIGURED",
			"message": "SVC_SHARING_URL must be set for bookmark fan-out",
		})}, nil
	}
	path := a.cfg.SharingURL + "/v1/atoms/" + params.AtomID + "/bookmark"
	return classify(a.call(ctx, http.MethodDelete, path, nil, params.Auth)), nil
}

type ListBookmarksParams struct {
	Auth   phyllis.AuthCtx
	Cursor string
	Limit  int
}

func (a *Aggregator) ListBookmarks(ctx context.Context, params ListBookmarksParams) (Response, error) {
	if a.cfg.SharingURL == "" {
		return ok200(mustMarshalJSON(map[string]any{
			"bookmarks":   []any{},
			"next_cursor": "",
		})), nil
	}
	q := url.Values{}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Cursor != "" {
		q.Set("cursor", params.Cursor)
	}
	downstream := a.cfg.SharingURL + "/v1/me/bookmarks"
	if enc := q.Encode(); enc != "" {
		downstream += "?" + enc
	}
	return classify(a.call(ctx, http.MethodGet, downstream, nil, params.Auth)), nil
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func ok200(body []byte) Response {
	return Response{
		Status: http.StatusOK,
		Body:   body,
	}
}

func notImplemented501(action string) Response {
	body := mustMarshalJSON(map[string]any{
		"error": map[string]any{
			"code":    "GATEWAY_NOT_IMPLEMENTED",
			"message": "TODO(CHO-1457): " + action + " is stubbed pending chora-sharing learner-facing API wire-up",
		},
	})
	return Response{
		Status: http.StatusNotImplemented,
		Body:   body,
	}
}

func mustMarshalJSON(v any) []byte {
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(v)
	// Trim the trailing newline json.Encoder appends.
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out
}
