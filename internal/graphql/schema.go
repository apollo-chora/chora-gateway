// Package graphql contains the chora-gateway GraphQL federation gateway.
//
// Per CLAUDE.md §4 protocol strategy, GraphQL serves learner-facing reads:
// knowledge-graph traversal, atom queries, persona, discovery. Admin CRUD
// stays REST; inter-service async stays Pub/Sub.
//
// This package wires four federated subschemas (engagement, gamification,
// companion, circle) behind a single /graphql endpoint that the BFF gateway
// exposes alongside the existing REST routes (handler.go). It does NOT
// remove or replace any REST route — the two protocols coexist.
//
// In the M11 phase 31 stub, the gateway parses + dispatches a small
// hand-coded subset of the federated schema directly. The full
// gqlgen-generated executor is wired in M12 once chora-contracts buf-gen
// pipelines are in place; gqlgen.yml in this package documents that
// future config.
package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/graphql/resolvers"
)

// UUID is the shared GraphQL UUID scalar.
type UUID = string

// DateTime is the shared GraphQL DateTime scalar (RFC 3339).
type DateTime = string

// JSON is the shared GraphQL JSON scalar.
type JSON = map[string]any

// Schema is the supergraph composed from the four subschemas.
type Schema struct {
	Engagement   *resolvers.EngagementResolver
	Gamification *resolvers.GamificationResolver
	Companion    *resolvers.CompanionResolver
	Circle       *resolvers.CircleResolver
	now          func() time.Time
}

// NewSchema constructs the federated schema with all four resolvers wired.
func NewSchema(eng *resolvers.EngagementResolver, gam *resolvers.GamificationResolver, fam *resolvers.CompanionResolver, circ *resolvers.CircleResolver) *Schema {
	return &Schema{
		Engagement:   eng,
		Gamification: gam,
		Companion:    fam,
		Circle:       circ,
		now:          func() time.Time { return time.Now().UTC() },
	}
}

// Request is the wire format for a GraphQL POST.
type Request struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName,omitempty"`
	Variables     map[string]any `json:"variables,omitempty"`
}

// Response is the wire format for a GraphQL response.
type Response struct {
	Data   map[string]any `json:"data,omitempty"`
	Errors []ErrorItem    `json:"errors,omitempty"`
}

// ErrorItem represents one entry in the GraphQL `errors` array.
type ErrorItem struct {
	Message string         `json:"message"`
	Path    []string       `json:"path,omitempty"`
	Ext     map[string]any `json:"extensions,omitempty"`
}

// Handler returns the http.HandlerFunc that serves /graphql.
func (s *Schema) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")

		switch r.Method {
		case http.MethodGet:
			// Schema introspection / playground stub.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"service":     "chora-gateway",
				"endpoint":    "/graphql",
				"federation":  "v2",
				"subschemas":  []string{"engagement", "gamification", "companion", "circle"},
				"reads_only":  true,
				"description": "Federated learner-facing GraphQL gateway (Phase 31).",
			})
			return
		case http.MethodPost:
			// fallthrough to body parsing.
		default:
			s.writeError(w, http.StatusMethodNotAllowed, "GRAPHQL_METHOD_NOT_ALLOWED",
				"only GET (introspect) + POST (query) supported")
			return
		}

		var req Request
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&req); err != nil {
			s.writeError(w, http.StatusBadRequest, "GRAPHQL_BAD_REQUEST", err.Error())
			return
		}
		if strings.TrimSpace(req.Query) == "" {
			s.writeError(w, http.StatusBadRequest, "GRAPHQL_EMPTY_QUERY", "query field is required")
			return
		}

		resp := s.Execute(r.Context(), req)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// Execute dispatches a GraphQL request to the appropriate resolver. The
// dispatcher inspects the top-level field of the query and routes to the
// owning subschema. Unknown roots return a GraphQL `errors` entry.
func (s *Schema) Execute(ctx context.Context, req Request) Response {
	root := topLevelField(req.Query)
	if root == "" {
		return Response{Errors: []ErrorItem{{Message: "could not determine top-level field"}}}
	}

	data := map[string]any{}
	switch root {
	// engagement
	case "myPaths":
		paths, err := s.Engagement.MyPaths(ctx)
		if err != nil {
			return Response{Errors: []ErrorItem{{Message: err.Error(), Path: []string{root}}}}
		}
		data[root] = paths
	case "path":
		id, _ := req.Variables["id"].(string)
		p, err := s.Engagement.Path(ctx, id)
		if err != nil {
			return Response{Errors: []ErrorItem{{Message: err.Error(), Path: []string{root}}}}
		}
		data[root] = p
	case "dailyDose":
		dose, err := s.Engagement.DailyDose(ctx)
		if err != nil {
			return Response{Errors: []ErrorItem{{Message: err.Error(), Path: []string{root}}}}
		}
		data[root] = dose
	case "discoveryFeed", "discovery":
		seed, _ := req.Variables["seedAtomId"].(string)
		depth := 2
		if d, ok := req.Variables["depth"].(float64); ok {
			depth = int(d)
		}
		edges, err := s.Engagement.DiscoveryFeed(ctx, seed, depth)
		if err != nil {
			return Response{Errors: []ErrorItem{{Message: err.Error(), Path: []string{root}}}}
		}
		data[root] = edges

	// gamification
	case "myWallet":
		w, err := s.Gamification.MyWallet(ctx)
		if err != nil {
			return Response{Errors: []ErrorItem{{Message: err.Error(), Path: []string{root}}}}
		}
		data[root] = w
	case "myBadges":
		b, err := s.Gamification.MyBadges(ctx)
		if err != nil {
			return Response{Errors: []ErrorItem{{Message: err.Error(), Path: []string{root}}}}
		}
		data[root] = b
	case "leaderboard":
		scope, _ := req.Variables["scope"].(string)
		scopeID, _ := req.Variables["scopeId"].(string)
		seasonID, _ := req.Variables["seasonId"].(string)
		lb, err := s.Gamification.Leaderboard(ctx, scope, scopeID, seasonID)
		if err != nil {
			return Response{Errors: []ErrorItem{{Message: err.Error(), Path: []string{root}}}}
		}
		data[root] = lb

	// companion. `myFamiliar` / `familiar` are the pre-rename query roots the
	// SPA still posts (chora-web core/graphql/queries.ts); they resolve through
	// the same resolver, keyed under the root the caller asked for.
	// ADR-254 D9 alias, drop after the SPA cut (WP-X go)
	case "myCompanion", "companion", "myFamiliar", "familiar":
		f, err := s.Companion.MyCompanion(ctx)
		if err != nil {
			return Response{Errors: []ErrorItem{{Message: err.Error(), Path: []string{root}}}}
		}
		data[root] = f

	// circle
	case "feed":
		scope, _ := req.Variables["scope"].(string)
		groupID, _ := req.Variables["groupId"].(string)
		feed, err := s.Circle.Feed(ctx, scope, groupID)
		if err != nil {
			return Response{Errors: []ErrorItem{{Message: err.Error(), Path: []string{root}}}}
		}
		data[root] = feed
	case "myCircleProfile":
		p, err := s.Circle.MyProfile(ctx)
		if err != nil {
			return Response{Errors: []ErrorItem{{Message: err.Error(), Path: []string{root}}}}
		}
		data[root] = p
	case "following":
		f, err := s.Circle.Following(ctx)
		if err != nil {
			return Response{Errors: []ErrorItem{{Message: err.Error(), Path: []string{root}}}}
		}
		data[root] = f
	case "followers":
		f, err := s.Circle.Followers(ctx)
		if err != nil {
			return Response{Errors: []ErrorItem{{Message: err.Error(), Path: []string{root}}}}
		}
		data[root] = f

	// stitched federation entry — `learner`
	case "learner":
		learner, err := s.composeLearner(ctx)
		if err != nil {
			return Response{Errors: []ErrorItem{{Message: err.Error(), Path: []string{root}}}}
		}
		data[root] = learner

	default:
		return Response{Errors: []ErrorItem{{
			Message: "unknown query root: " + root,
			Path:    []string{root},
			Ext:     map[string]any{"code": "GRAPHQL_UNKNOWN_FIELD"},
		}}}
	}

	return Response{Data: data}
}

// composeLearner stitches across all four subschemas to build the federated
// `Learner` aggregate. Failures from any single subschema return null for
// that field but do not fail the whole query (graceful degradation in line
// with the BFF aggregator pattern).
func (s *Schema) composeLearner(ctx context.Context) (map[string]any, error) {
	out := map[string]any{}

	if profile, err := s.Circle.MyProfile(ctx); err == nil && profile != nil {
		out["gcid"] = profile["gcid"]
		out["displayName"] = profile["displayName"]
		out["circleProfile"] = profile
	}
	if paths, err := s.Engagement.MyPaths(ctx); err == nil {
		out["paths"] = paths
	}
	if fam, err := s.Companion.MyCompanion(ctx); err == nil {
		out["companion"] = fam
	}
	if w, err := s.Gamification.MyWallet(ctx); err == nil {
		out["wallet"] = w
	}
	if _, ok := out["gcid"]; !ok {
		return nil, errors.New("learner gcid unresolvable from JWT context")
	}
	return out, nil
}

func (s *Schema) writeError(w http.ResponseWriter, status int, code, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Response{
		Errors: []ErrorItem{{Message: msg, Ext: map[string]any{"code": code}}},
	})
}

// topLevelField scans a GraphQL document and returns the first field name
// inside the outermost selection set. Falls back to "" if the document is
// malformed or empty. Designed for the M11 hand-coded dispatcher; the
// full gqlgen executor will replace this in M12.
func topLevelField(query string) string {
	q := strings.TrimSpace(query)
	// Strip the operation header, e.g. `query Foo { ... }` or `{ ... }`.
	if i := strings.Index(q, "{"); i >= 0 {
		q = q[i+1:]
	}
	q = strings.TrimSpace(q)

	// Read the first identifier.
	first, rest := readIdent(q)
	if first == "" {
		return ""
	}

	// Detect alias form `aliasName: realFieldName ...` — there may be
	// whitespace between aliasName, ':', and realFieldName.
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, ":") {
		realField, _ := readIdent(strings.TrimSpace(strings.TrimPrefix(rest, ":")))
		if realField != "" {
			return realField
		}
	}
	return first
}

// readIdent returns the leading GraphQL identifier in s and the remainder.
func readIdent(s string) (ident, rest string) {
	end := len(s)
	for i, ch := range s {
		if !isIdentChar(ch) {
			end = i
			break
		}
	}
	return s[:end], s[end:]
}

func isIdentChar(ch rune) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
		(ch >= '0' && ch <= '9') || ch == '_'
}
