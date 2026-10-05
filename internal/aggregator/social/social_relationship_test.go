// social_relationship_test.go — RelationshipOp fan-out tests (ADR-230
// B-lite.2, CHO-2121).
//
// RelationshipOp is the single opaque passthrough for every
// /v1/connections/* relationship route (ADR-196 D4 — the BFF forwards the
// body verbatim and never reshapes it). Verifies method + path + body +
// identity/mesh headers reach chora-sharing, and that an unconfigured
// SharingURL fails loud with 502 (a relationship write must never fake a
// success and the pending lists must never fake an empty state).
package social_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/social"
)

func TestRelationshipOp_FansOutMethodPathBodyHeaders(t *testing.T) {
	t.Parallel()
	var (
		gotMethod, gotPath string
		gotBody            []byte
		gotTenant, gotGCID string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		gotTenant = r.Header.Get(servicemesh.HeaderTenantID)
		gotGCID = r.Header.Get(servicemesh.HeaderGCID)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"gcid":"g-2","created":true}`))
	}))
	defer srv.Close()

	agg := social.New(social.Config{SharingURL: srv.URL})
	resp, err := agg.RelationshipOp(context.Background(), social.RelationshipOpParams{
		Auth:    phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		Method:  http.MethodPost,
		Subpath: "follows",
		Body:    []byte(`{"gcid":"g-2"}`),
	})
	if err != nil {
		t.Fatalf("RelationshipOp: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Fatalf("status = %d; want 201 (passthrough)", resp.Status)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/connections/follows" {
		t.Fatalf("downstream = %s %s; want POST /v1/connections/follows", gotMethod, gotPath)
	}
	if string(gotBody) != `{"gcid":"g-2"}` {
		t.Fatalf("body = %q; want verbatim passthrough", gotBody)
	}
	if gotTenant != "tenant-a" || gotGCID != "gcid-1" {
		t.Fatalf("mesh hdrs: tenant=%q gcid=%q", gotTenant, gotGCID)
	}
}

func TestRelationshipOp_DeleteSubpath(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	agg := social.New(social.Config{SharingURL: srv.URL})
	resp, err := agg.RelationshipOp(context.Background(), social.RelationshipOpParams{
		Auth:    phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		Method:  http.MethodDelete,
		Subpath: "friend-requests/incoming/g-9",
	})
	if err != nil {
		t.Fatalf("RelationshipOp: %v", err)
	}
	if resp.Status != http.StatusNoContent {
		t.Fatalf("status = %d; want 204", resp.Status)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v1/connections/friend-requests/incoming/g-9" {
		t.Fatalf("downstream = %s %s; want DELETE /v1/connections/friend-requests/incoming/g-9", gotMethod, gotPath)
	}
}

func TestRelationshipOp_QueryStringForwarded(t *testing.T) {
	t.Parallel()
	var gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.URL.RequestURI()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"suggestions":[]}`))
	}))
	defer srv.Close()

	agg := social.New(social.Config{SharingURL: srv.URL})
	resp, err := agg.RelationshipOp(context.Background(), social.RelationshipOpParams{
		Auth:    phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		Method:  http.MethodGet,
		Subpath: "suggestions?limit=5",
	})
	if err != nil {
		t.Fatalf("RelationshipOp: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.Status)
	}
	if gotURI != "/v1/connections/suggestions?limit=5" {
		t.Fatalf("downstream URI = %q; want query forwarded verbatim", gotURI)
	}
}

func TestRelationshipOp_Unconfigured502(t *testing.T) {
	t.Parallel()
	agg := social.New(social.Config{})
	resp, err := agg.RelationshipOp(context.Background(), social.RelationshipOpParams{
		Auth:    phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		Method:  http.MethodPost,
		Subpath: "follows",
		Body:    []byte(`{"gcid":"g-2"}`),
	})
	if err != nil {
		t.Fatalf("RelationshipOp: %v", err)
	}
	if resp.Status != http.StatusBadGateway {
		t.Fatalf("unconfigured status = %d; want 502 (never a fake success)", resp.Status)
	}
}
