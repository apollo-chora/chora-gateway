package main

import (
	"net/http"
	"sort"
	"strings"
	"testing"
)

// T3.9 (K131): the dashboard's today strip reads the streak and the mana balance through
// production's own edge reads, GET only. The streak reaches a browser only through the
// composing GET /api/me/dashboard (no edge route serves chora-consumption /v1/me/streak
// alone); the balance through GET /api/v1/me/mana, a pass-through to chora-identity.

// kioskTestGCID is the demo identity signKioskToken signs for (INTERFACES 6).
const kioskTestGCID = "01a1411f-7f22-76c6-ab1d-362dddc20413"

func TestKioskDashboardReads_TheMana(t *testing.T) {
	rig := newD8Rig(t)
	rec, hits := rig.do("GET", "/api/v1/me/mana", "", "")
	if rec.Code != http.StatusUnauthorized || len(hits) != 0 {
		t.Errorf("without a session: %d with %d upstream call(s), want 401 and none", rec.Code, len(hits))
	}
	rec, hits = rig.do("GET", "/api/v1/me/mana", "", rig.token)
	if !reached(hits, d8Hit{"identity", "GET", "/api/v1/me/mana", ""}) || rec.Code != http.StatusOK {
		t.Errorf("GET /api/v1/me/mana reached %v (status %d %s)", hits, rec.Code, rec.Body.String())
	}
}

// The dashboard read fans out to production's read models only: every upstream call a GET,
// and only identity, consumption and delivery, all of which the kiosk runs. A write, or a
// call to an absent upstream, fails it.
func TestKioskDashboardReads_TheDashboardFansOutToReadsOnly(t *testing.T) {
	rig := newD8Rig(t)
	rec, hits := rig.do("GET", "/api/me/dashboard", "", "")
	if rec.Code != http.StatusUnauthorized || len(hits) != 0 {
		t.Errorf("without a session: %d with %d upstream call(s), want 401 and none", rec.Code, len(hits))
	}
	rec, hits = rig.do("GET", "/api/me/dashboard", "", rig.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/me/dashboard: %d %s", rec.Code, rec.Body.String())
	}
	var got []string
	for _, h := range hits {
		if h.method != http.MethodGet {
			t.Errorf("the dashboard read sent %s %s to %s", h.method, h.path, h.service)
		}
		got = append(got, h.service+" "+h.path)
	}
	sort.Strings(got)
	want := []string{
		"consumption /v1/me/companions",
		"consumption /v1/me/growth-edges/uploads",
		"consumption /v1/me/learning-paths",
		"consumption /v1/me/maps",
		"consumption /v1/me/practice-budget",
		"consumption /v1/me/streak",
		"consumption /v1/me/transcript",
		"delivery /api/v1/instructors/" + kioskTestGCID + "/courses",
		"identity /me",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the dashboard read called:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Negative control: only the two GETs open. The mana top-up and ledger, and any other
// method on either path, stay refused at the edge with no upstream call.
func TestKioskDashboardReads_OnlyTheTwoReadsOpen(t *testing.T) {
	rig := newD8Rig(t)
	for _, c := range [][2]string{
		{"POST", "/api/v1/me/mana"},
		{"POST", "/api/v1/me/mana/topup"},
		{"GET", "/api/v1/me/mana/ledger"},
		{"POST", "/api/me/dashboard"},
		{"GET", "/api/me/dashboard/extra"},
	} {
		rec, hits := rig.do(c[0], c[1], `{}`, rig.token)
		if !strings.Contains(rec.Body.String(), "NOT_PART_OF_KIOSK_EDITION") || len(hits) != 0 {
			t.Errorf("%s %s answered %d %s with %d upstream call(s), want the kiosk refusal and none",
				c[0], c[1], rec.Code, strings.TrimSpace(rec.Body.String()), len(hits))
		}
	}
}
