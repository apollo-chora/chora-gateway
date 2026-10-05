package medashboard

// medashboard_cards_test.go: the ranked home card envelope (UX Track U, C1
// backend half, slice A).
//
// The contract is docs/references/home-rank-key.md. The BACKEND composes cards
// and orders them; the frontend renders them in the order given. Every card
// carries card_id, kind, surface, route, count, deadline_at, urgency, warmth
// and state, and a card whose data could not be read is emitted with state set
// rather than silently dropped, because an empty home and an unread one are
// different facts.
//
// This slice lands the envelope, the total ordering, the bands timezone and the
// two structurally absent cards. It composes cards ONLY from parts the
// aggregator already fetches, so the contract subagent1 builds against lands
// without a new fan-out riding along inside it.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type cardWire struct {
	CardID     string  `json:"card_id"`
	Kind       string  `json:"kind"`
	Surface    string  `json:"surface"`
	Route      string  `json:"route"`
	Count      int     `json:"count"`
	DeadlineAt *string `json:"deadline_at"`
	Urgency    int     `json:"urgency"`
	Warmth     int     `json:"warmth"`
	State      string  `json:"state"`
}

type dashWire struct {
	Cards     []cardWire        `json:"cards"`
	BandsTZ   string            `json:"bands_tz"`
	KnownGaps map[string]string `json:"known_gaps"`
}

// cardsFor drives the aggregator against a stub consumption and returns the
// decoded envelope.
func cardsFor(t *testing.T, tz string, streakBody, pathsBody string) dashWire {
	t.Helper()
	consumption := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/me/streak":
			_, _ = w.Write([]byte(streakBody))
		case "/v1/me/learning-paths":
			_, _ = w.Write([]byte(pathsBody))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(consumption.Close)
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"display_name":"Dale"}`))
	}))
	t.Cleanup(identity.Close)

	agg := New(Config{ConsumptionURL: consumption.URL, IdentityURL: identity.URL})
	resp, _ := agg.GetDashboard(context.Background(),
		AuthCtx{TenantID: "t-1", GCID: "gcid-1", Roles: []string{"learner"}, TimeZone: tz})

	var got dashWire
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatalf("decode dashboard: %v (%s)", err, resp.Body)
	}
	return got
}

// ⚠ The UPSTREAM streak shape is `count`, not `current_streak_days`. The
// aggregator renames it on the way out (count -> current_streak_days,
// last_activity_at -> last_completion_at), so a fixture written against the
// RESPONSE shape parses as a zero streak and silently emits no card. I wrote it
// that way first and read the miss as "the card is not wired".
const noStreak = `{"count":0}`
const noPaths = `{"items":[]}`

// The two read models with no downstream source at all are declared, not
// dropped. A card the operator cannot see is indistinguishable from a card
// nobody needs.
func TestDashboardCards_StructurallyAbsentCardsAreDeclared(t *testing.T) {
	got := cardsFor(t, "UTC", noStreak, noPaths)

	byKind := map[string]cardWire{}
	for _, c := range got.Cards {
		byKind[c.Kind] = c
	}
	for _, kind := range []string{"grading_queue", "tenants_needing_setup"} {
		c, ok := byKind[kind]
		if !ok {
			t.Errorf("card %q is missing entirely; it must be emitted with state absent", kind)
			continue
		}
		if c.State != "absent" {
			t.Errorf("card %q state = %q; want absent", kind, c.State)
		}
		if got.KnownGaps[kind] == "" {
			t.Errorf("card %q is absent with no reason in known_gaps", kind)
		}
	}
}

// bands_tz is ECHOED so the client knows which day the bands were computed in
// and can refine due-today itself when the zone is not the viewer's.
func TestDashboardCards_EchoesTheZoneItComputedIn(t *testing.T) {
	if got := cardsFor(t, "Asia/Singapore", noStreak, noPaths); got.BandsTZ != "Asia/Singapore" {
		t.Errorf("bands_tz = %q; want the requested zone", got.BandsTZ)
	}
}

// No zone is a DECLARED answer, not a silent fallback.
func TestDashboardCards_NoZoneComputesInUTCAndSaysSo(t *testing.T) {
	if got := cardsFor(t, "", noStreak, noPaths); got.BandsTZ != "UTC" {
		t.Errorf("bands_tz = %q; want UTC declared", got.BandsTZ)
	}
}

// An unparseable zone must not silently become UTC while claiming the caller's
// zone, and must not fail the dashboard either.
func TestDashboardCards_AnUnknownZoneFallsBackToUTCAndSaysSo(t *testing.T) {
	if got := cardsFor(t, "Mars/Olympus_Mons", noStreak, noPaths); got.BandsTZ != "UTC" {
		t.Errorf("bands_tz = %q; want UTC after refusing an unknown zone", got.BandsTZ)
	}
}

// A streak whose day bucket is before today is at risk: band 2.
func TestDashboardCards_StreakAtRiskIsUrgencyTwo(t *testing.T) {
	twoDaysAgo := time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02T00:00:00Z")
	got := cardsFor(t, "UTC", `{"count":5,"last_activity_at":"`+twoDaysAgo+`"}`, noPaths)

	for _, c := range got.Cards {
		if c.Kind == "streak_at_risk" {
			if c.Urgency != 2 {
				t.Errorf("urgency = %d; want 2", c.Urgency)
			}
			if c.State != "live" {
				t.Errorf("state = %q; want live", c.State)
			}
			return
		}
	}
	t.Error("no streak_at_risk card for a streak whose bucket is two days old")
}

// A streak with activity TODAY is not at risk, and the card must not be
// emitted as live with a false claim.
func TestDashboardCards_StreakTouchedTodayIsNotAtRisk(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02T00:00:00Z")
	got := cardsFor(t, "UTC", `{"count":5,"last_activity_at":"`+today+`"}`, noPaths)

	for _, c := range got.Cards {
		if c.Kind == "streak_at_risk" && c.State == "live" {
			t.Errorf("a streak touched today was ranked at risk: %+v", c)
		}
	}
}

// ⚠ The streak stamp is a DAY BUCKET normalised to UTC midnight, not a moment.
// East of UTC the local day starts before the bucket does, so a bucket dated
// today is not yet proof of activity in the viewer's today, and treating it as
// stale would tell a learner who studied minutes ago that their streak is at
// risk. The band must reason from the LAST possible moment inside the bucket.
func TestDashboardCards_TodaysBucketIsNotStaleEastOfUTC(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02T00:00:00Z")
	got := cardsFor(t, "Asia/Singapore", `{"count":5,"last_activity_at":"`+today+`"}`, noPaths)

	for _, c := range got.Cards {
		if c.Kind == "streak_at_risk" && c.State == "live" {
			t.Errorf("a UTC day bucket dated today was read as stale in a UTC+8 zone: %+v", c)
		}
	}
}

// Ordering is total, so two renders of the same data never swap two cards.
func TestDashboardCards_OrderingIsTotalAndStable(t *testing.T) {
	twoDaysAgo := time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02T00:00:00Z")
	streak := `{"count":5,"last_activity_at":"` + twoDaysAgo + `"}`
	// progress_percent is what drives the continuation band; a fixture without it
	// is a course at zero, which is NOT in progress. Caught by a probe: the
	// ordering test was passing over three cards with the band-1 card absent.
	paths := `{"items":[{"source_type":"course","title":"Algebra","current_index":2,"total_atoms":10,"progress_percent":20}]}`

	first := cardsFor(t, "UTC", streak, paths)
	second := cardsFor(t, "UTC", streak, paths)

	if len(first.Cards) != len(second.Cards) {
		t.Fatalf("card count moved between renders: %d then %d", len(first.Cards), len(second.Cards))
	}
	// A guard on the test itself. An ordering assertion over one or two cards
	// proves nothing, and this test DID pass vacuously over three cards with the
	// band-1 card missing until a probe showed the composition. Four is the
	// number this fixture composes: bands 3, 2, 1 and 0.
	if len(first.Cards) < 4 {
		t.Fatalf("only %d cards; the fixture no longer exercises multiple bands "+
			"and this ordering assertion would pass without proving anything", len(first.Cards))
	}
	for i := range first.Cards {
		if first.Cards[i].CardID != second.Cards[i].CardID {
			t.Fatalf("cards swapped between renders at %d: %q then %q",
				i, first.Cards[i].CardID, second.Cards[i].CardID)
		}
	}
	// Urgency descending, then warmth descending: the documented key.
	for i := 1; i < len(first.Cards); i++ {
		prev, cur := first.Cards[i-1], first.Cards[i]
		if prev.Urgency < cur.Urgency {
			t.Errorf("urgency ascends at %d: %d then %d", i, prev.Urgency, cur.Urgency)
		}
		if prev.Urgency == cur.Urgency && prev.Warmth < cur.Warmth {
			t.Errorf("warmth ascends within a band at %d: %d then %d", i, prev.Warmth, cur.Warmth)
		}
	}
}

// cards is ALWAYS a list, never null, so the client's iterator cannot NPE on a
// dashboard with nothing to show.
func TestDashboardCards_IsNeverNull(t *testing.T) {
	got := cardsFor(t, "UTC", noStreak, noPaths)
	if got.Cards == nil {
		t.Error("cards is null; it must be an empty list")
	}
}

// ---- band 4 and 5 in the viewer's zone (ruling 3) ---------------------------
//
// Bands 4 and 5 are the only deadline-derived ones, so the client can refine
// WITHIN {4, 5} from deadline_at but can never demote out of them: bands 3 to 1
// are not deadline-derived, and a client that demoted a card would have no way
// to know which band it should land in. The server therefore must not emit band
// 4 or 5 for a deadline that is neither overdue nor due today IN THE ECHOED
// ZONE, or the card is stuck at the top of the page with nothing able to
// correct it.

func TestDeadlineUrgency_OverdueIsBandFive(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	if got := deadlineUrgency(&past, now, time.UTC); got != urgencyOverdue {
		t.Errorf("urgency = %d; want %d for a deadline in the past", got, urgencyOverdue)
	}
}

func TestDeadlineUrgency_DueInsideTheViewersDayIsBandFour(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	later := now.Add(3 * time.Hour) // still 2026-09-02 in UTC
	if got := deadlineUrgency(&later, now, time.UTC); got != urgencyDueToday {
		t.Errorf("urgency = %d; want %d", got, urgencyDueToday)
	}
}

// ⚠ The edge subagent1 cannot close on the client. 2026-09-02T20:00Z is TODAY
// in UTC and 2026-09-03T04:00 in Asia/Singapore, which is TOMORROW there. Read
// in UTC it is band 4 and pinned to the top of the page; read in the viewer's
// zone it is not due today at all and must not be deadline-ranked, because the
// client can only refine between 4 and 5 and could never demote it.
func TestDeadlineUrgency_TodayInUTCButTomorrowForTheViewerIsNotBandFour(t *testing.T) {
	sg, err := time.LoadLocation("Asia/Singapore")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	deadline := time.Date(2026, 9, 2, 20, 0, 0, 0, time.UTC)

	if got := deadlineUrgency(&deadline, now, time.UTC); got != urgencyDueToday {
		t.Fatalf("the fixture is wrong: in UTC this must be band %d, got %d", urgencyDueToday, got)
	}
	if got := deadlineUrgency(&deadline, now, sg); got == urgencyDueToday || got == urgencyOverdue {
		t.Errorf("urgency = %d in Asia/Singapore, where the deadline falls TOMORROW; "+
			"a band 4 here is pinned to the top of the page and the client cannot demote it", got)
	}
}

// A deadline further out is not deadline-ranked at all, and the caller falls
// back to the card's own band.
func TestDeadlineUrgency_ALaterDeadlineIsNotDeadlineRanked(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	nextWeek := now.AddDate(0, 0, 7)
	if got := deadlineUrgency(&nextWeek, now, time.UTC); got == urgencyDueToday || got == urgencyOverdue {
		t.Errorf("urgency = %d for a deadline a week out; want neither 4 nor 5", got)
	}
}

// No deadline is not deadline-derived, and must not be read as "not overdue".
func TestDeadlineUrgency_NoDeadlineIsNotDeadlineRanked(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	if got := deadlineUrgency(nil, now, time.UTC); got == urgencyDueToday || got == urgencyOverdue {
		t.Errorf("urgency = %d for a card with no deadline", got)
	}
}

// ---- every emitted route is a route that exists -----------------------------
//
// Five of the seven routes in the first cut landed on the A+ not-found. A card's
// route IS its action: a count the learner cannot act on is worse than no card,
// because it looks like the platform lost their work. subagent1 checked each
// against the surface route modules and is adding a home-tree spec so the class
// cannot recur silently; these pin the backend half.
//
// Verified against origin/main route modules, not assumed:
//
//	/a/me/transcript          aplus.routes.ts  (NOT /a/transcript)
//	/a/daily-dose             aplus.routes.ts  (NOT /a/today)
//	/a/knowledge              aplus.routes.ts
//	/a/courses/:id/learn      aplus.routes.ts  (the player; /a/paths does not exist)
//	/r/assessments            rplus.routes.ts  (NOT /r/grading)
//	/h/ready                  hplus.routes.ts  (NOT /h/tenants)
func TestDashboardCards_RoutesAreTheOnesThatExist(t *testing.T) {
	twoDaysAgo := time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02T00:00:00Z")
	got := cardsFor(t, "UTC", `{"count":5,"last_activity_at":"`+twoDaysAgo+`"}`,
		`{"items":[{"course_id":"c-7","source_type":"course","title":"Algebra",`+
			`"current_index":2,"total_atoms":10,"progress_percent":20}]}`)

	byKind := map[string]cardWire{}
	for _, c := range got.Cards {
		byKind[c.Kind] = c
	}
	for kind, want := range map[string]string{
		"streak_at_risk":        "/a/daily-dose",
		"grading_queue":         "/r/assessments",
		"tenants_needing_setup": "/h/ready",
		// The continuation card DEEP LINKS into the player for a specific
		// course. A generic list route would hand the learner back the same
		// decision the card was supposed to make for them.
		"continue_learning": "/a/courses/c-7/learn",
	} {
		c, ok := byKind[kind]
		if !ok {
			t.Errorf("card %q missing", kind)
			continue
		}
		if c.Route != want {
			t.Errorf("card %q route = %q; want %q", kind, c.Route, want)
		}
	}
}

// No in-progress course means NO continuation card, rather than a card routed
// at a list the learner has no reason to open.
func TestDashboardCards_NoContinuationCardWithNothingToContinue(t *testing.T) {
	got := cardsFor(t, "UTC", noStreak, noPaths)
	for _, c := range got.Cards {
		if c.Kind == "continue_learning" {
			t.Errorf("a continuation card with nothing to continue: %+v", c)
		}
	}
}

// A course whose id the upstream did not carry cannot be deep-linked, so the
// card is not emitted at all. A card routed at /a/courses//learn would be a
// broken link dressed as an action.
func TestDashboardCards_NoContinuationCardWithoutACourseID(t *testing.T) {
	got := cardsFor(t, "UTC", noStreak,
		`{"items":[{"source_type":"collection","title":"Loose atoms",`+
			`"current_index":2,"total_atoms":10,"progress_percent":20}]}`)
	for _, c := range got.Cards {
		if c.Kind == "continue_learning" {
			t.Errorf("a continuation card with no course id to link to: %+v", c)
		}
	}
}
