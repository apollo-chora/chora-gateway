package graphql_test

import (
	"context"
	"testing"

	bffgraphql "github.com/apollo-chora/chora-gateway/internal/graphql"
)

// TestSchema_Execute_MyFamiliarAlias_ADR254: the SPA still posts the
// pre-rename `myFamiliar` query root (chora-web core/graphql/queries.ts) until
// the SPA cut; the hand-coded dispatcher serves it as an alias of
// `myCompanion` (same resolver, keyed under the root the caller asked for).
// ADR-254 D9 alias, drop after the SPA cut (WP-X go).
func TestSchema_Execute_MyFamiliarAlias_ADR254(t *testing.T) {
	s := newTestSchema(t)
	for _, tc := range []struct{ root, query string }{
		{"myCompanion", `{ myCompanion { id species level } }`},
		{"myFamiliar", `{ myFamiliar { id species level } }`},
	} {
		t.Run(tc.root, func(t *testing.T) {
			resp := s.Execute(context.Background(), bffgraphql.Request{Query: tc.query})
			if len(resp.Errors) > 0 {
				t.Fatalf("unexpected errors: %v", resp.Errors)
			}
			v, ok := resp.Data[tc.root]
			if !ok || v == nil {
				t.Fatalf("expected data.%s, got %+v", tc.root, resp.Data)
			}
		})
	}
}

// TestSchema_Execute_MyFamiliarPayloadCarriesTheSPAFields pins the shape the
// live SPA call reads. chora-web's familiar.service.ts mapFamiliarProfile
// copies gql.gcid / gql.name / gql.createdAt / gql.updatedAt; the pre-fix
// alias payload omitted all four, so the Familiar profile rendered with
// undefined identity + timestamps.
func TestSchema_Execute_MyFamiliarPayloadCarriesTheSPAFields(t *testing.T) {
	s := newTestSchema(t)
	resp := s.Execute(context.Background(), bffgraphql.Request{
		Query: `{ myFamiliar { id gcid name createdAt updatedAt } }`,
	})
	if len(resp.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", resp.Errors)
	}
	m, ok := resp.Data["myFamiliar"].(map[string]any)
	if !ok {
		t.Fatalf("expected map myFamiliar, got %T", resp.Data["myFamiliar"])
	}
	for _, k := range []string{"gcid", "name", "createdAt", "updatedAt"} {
		if v, ok := m[k]; !ok || v == nil || v == "" {
			t.Errorf("myFamiliar.%s missing or empty: %v", k, m[k])
		}
	}
}
