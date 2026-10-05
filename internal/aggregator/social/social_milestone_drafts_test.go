// social_milestone_drafts_test.go — MilestoneDraftsOp / MilestoneSharePrefOp
// fan-out tests (CHO-2258, completes the CHO-2203 lane).
//
// Both are opaque passthroughs (ADR-196 D4) for the owner-facing C+ Companion-
// milestone routes. Verifies method + path + body + identity/mesh headers reach
// chora-sharing at EXACTLY the paths the ns/sharing Istio policy allowlists, and
// that an unconfigured SharingURL fails loud with 502 on reads as well as writes
// — a stubbed empty drafts list would tell a learner no milestones are waiting.
package social_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/phyllis"
	"github.com/apollo-chora/chora-gateway/internal/aggregator/social"
)

func TestMilestoneDraftsOp_ListFansOutToTheAllowlistedPath(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath, gotTenant, gotGCID string
	var gotRoles []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotTenant = r.Header.Get(servicemesh.HeaderTenantID)
		gotGCID = r.Header.Get(servicemesh.HeaderGCID)
		gotRoles = r.Header.Values(servicemesh.HeaderUserRoles)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"drafts":[]}`))
	}))
	defer srv.Close()

	agg := social.New(social.Config{SharingURL: srv.URL})
	resp, err := agg.MilestoneDraftsOp(context.Background(), social.MilestoneDraftsOpParams{
		Auth:   phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a", Roles: []string{"learner"}},
		Method: http.MethodGet,
	})
	if err != nil {
		t.Fatalf("MilestoneDraftsOp: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200 (passthrough)", resp.Status)
	}
	// The mesh allowlists this EXACT path — any drift here is a live 403.
	if gotMethod != http.MethodGet || gotPath != "/v1/me/post-drafts" {
		t.Fatalf("downstream = %s %s; want GET /v1/me/post-drafts", gotMethod, gotPath)
	}
	if gotTenant != "tenant-a" || gotGCID != "gcid-1" {
		t.Fatalf("mesh hdrs: tenant=%q gcid=%q", gotTenant, gotGCID)
	}
	// Omitting roles denies 100% of any role-gated downstream call.
	if len(gotRoles) == 0 || !strings.Contains(strings.Join(gotRoles, ","), "learner") {
		t.Fatalf("roles must reach the downstream; got %v", gotRoles)
	}
}

func TestMilestoneDraftsOp_PublishAndDiscardSubpaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ subpath, want string }{
		{"/01970000-0000-7000-a000-0000000000d1/publish", "/v1/me/post-drafts/01970000-0000-7000-a000-0000000000d1/publish"},
		{"/01970000-0000-7000-a000-0000000000d1/discard", "/v1/me/post-drafts/01970000-0000-7000-a000-0000000000d1/discard"},
	} {
		var gotMethod, gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			w.WriteHeader(http.StatusCreated)
		}))
		agg := social.New(social.Config{SharingURL: srv.URL})
		_, err := agg.MilestoneDraftsOp(context.Background(), social.MilestoneDraftsOpParams{
			Auth:    phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
			Method:  http.MethodPost,
			Subpath: tc.subpath,
		})
		srv.Close()
		if err != nil {
			t.Fatalf("MilestoneDraftsOp(%s): %v", tc.subpath, err)
		}
		if gotMethod != http.MethodPost || gotPath != tc.want {
			t.Errorf("downstream = %s %s; want POST %s", gotMethod, gotPath, tc.want)
		}
	}
}

// A stubbed empty 200 here would tell a learner no milestones are waiting when
// the truth is the gateway is misconfigured (contrast the read-led feed stubs,
// where an empty feed is a normal state rather than a lie about the learner's
// own queued content).
func TestMilestoneDraftsOp_UnconfiguredSharingURL_FailsLoudEvenOnTheRead(t *testing.T) {
	t.Parallel()
	agg := social.New(social.Config{SharingURL: ""})
	resp, err := agg.MilestoneDraftsOp(context.Background(), social.MilestoneDraftsOpParams{
		Auth:   phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		Method: http.MethodGet,
	})
	if err != nil {
		t.Fatalf("MilestoneDraftsOp: %v", err)
	}
	if resp.Status != http.StatusBadGateway {
		t.Fatalf("status = %d; want 502 GATEWAY_NOT_CONFIGURED", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "GATEWAY_NOT_CONFIGURED") {
		t.Errorf("body must name the fault; got %s", resp.Body)
	}
}

func TestMilestoneSharePrefOp_GetAndSetFanOutToTheAllowlistedPath(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"policy":"auto"}`))
	}))
	defer srv.Close()

	agg := social.New(social.Config{SharingURL: srv.URL})
	resp, err := agg.MilestoneSharePrefOp(context.Background(), social.MilestoneSharePrefOpParams{
		Auth:   phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		Method: http.MethodPost,
		Body:   []byte(`{"policy":"auto"}`),
	})
	if err != nil {
		t.Fatalf("MilestoneSharePrefOp: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.Status)
	}
	// The sharing upstream renames at its own window; the downstream path stays pre-rename.
	if gotMethod != http.MethodPost || gotPath != "/v1/me/preferences/familiar-milestone-share" {
		t.Fatalf("downstream = %s %s; want POST /v1/me/preferences/familiar-milestone-share", gotMethod, gotPath)
	}
	if string(gotBody) != `{"policy":"auto"}` {
		t.Fatalf("body = %q; want verbatim passthrough", gotBody)
	}
}

func TestMilestoneSharePrefOp_UnconfiguredSharingURL_502(t *testing.T) {
	t.Parallel()
	agg := social.New(social.Config{SharingURL: ""})
	resp, err := agg.MilestoneSharePrefOp(context.Background(), social.MilestoneSharePrefOpParams{
		Auth:   phyllis.AuthCtx{GCID: "gcid-1", TenantID: "tenant-a"},
		Method: http.MethodPost,
		Body:   []byte(`{"policy":"auto"}`),
	})
	if err != nil {
		t.Fatalf("MilestoneSharePrefOp: %v", err)
	}
	// A preference write that fakes a success leaves the learner believing they
	// opted into auto while every milestone keeps drafting.
	if resp.Status != http.StatusBadGateway {
		t.Fatalf("status = %d; want 502", resp.Status)
	}
}
