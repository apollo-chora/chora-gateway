package medashboard

// medashboard_cards_learner_test.go: the three learner cards with a live read
// model behind them (UX Track U, C1 slice B).
//
// Slice A landed the envelope from parts the aggregator already fetched. These
// three need their own fan-out, which is why they were kept out of the contract
// slice: a new downstream call that can fail must not be able to make the
// contract itself look wrong.
//
// Every card is asserted in all THREE states, because that distinction is the
// entire point of the envelope and the one thing a reviewer cannot check by
// reading the happy path:
//
//	live:   the read model answered
//	unread: it is wired and did not answer this request (503, 500, 400)
//	absent: there is no downstream source in this deployment at all
//
// A dark upstream that renders as "nothing pending" silently deletes the
// highest-value card on the page, and the learner cannot tell.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// learnerStub serves the three slice-B read models with per-path status control.
type learnerStub struct {
	status map[string]int
	body   map[string]string
}

func (s learnerStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if code, ok := s.status[r.URL.Path]; ok && code != http.StatusOK {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"code":"BOOM"}`))
			return
		}
		if b, ok := s.body[r.URL.Path]; ok {
			_, _ = w.Write([]byte(b))
			return
		}
		switch r.URL.Path {
		case "/v1/me/streak":
			_, _ = w.Write([]byte(`{"count":0}`))
		case "/v1/me/learning-paths":
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}
}

const (
	pathPending  = "/v1/me/growth-edges/uploads"
	pathTranscpt = "/v1/me/transcript"
	pathBudget   = "/v1/me/practice-budget"
)

func learnerCards(t *testing.T, stub learnerStub, wireConsumption bool) map[string]cardWire {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"display_name":"Dale"}`))
	}))
	t.Cleanup(identity.Close)

	cfg := Config{IdentityURL: identity.URL}
	if wireConsumption {
		cfg.ConsumptionURL = srv.URL
	}
	agg := New(cfg)
	resp, _ := agg.GetDashboard(context.Background(),
		AuthCtx{TenantID: "t-1", GCID: "gcid-1", Roles: []string{"learner"}, TimeZone: "UTC"})

	var got dashWire
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, resp.Body)
	}
	byKind := map[string]cardWire{}
	for _, c := range got.Cards {
		byKind[c.Kind] = c
	}
	return byKind
}

func liveStub() learnerStub {
	return learnerStub{
		status: map[string]int{},
		body: map[string]string{
			pathPending:  `{"items":[{"upload_id":"u-1"},{"upload_id":"u-2"}]}`,
			pathTranscpt: `{"items":[{"entry_id":"e-1","unseen":true}],"unseen_count":7}`,
			pathBudget: `{"resets_at":"2026-09-03T00:00:00Z","budgets":[` +
				`{"origin":"march","cap":1,"used":1,"remaining":0,"exhausted":true},` +
				`{"origin":"tap","cap":3,"used":1,"remaining":2,"exhausted":false}]}`,
		},
	}
}

func TestLearnerCards_LiveReadModelsCarryTheirCounts(t *testing.T) {
	cards := learnerCards(t, liveStub(), true)

	for kind, wantCount := range map[string]int{
		"pending_diagnoses": 2,
		"unseen_results":    7, // the ROLL-UP, not the length of the page
		"practice_budget":   2, // taps remaining, not the number of budgets
	} {
		c, ok := cards[kind]
		if !ok {
			t.Errorf("card %q missing", kind)
			continue
		}
		if c.State != "live" {
			t.Errorf("card %q state = %q; want live", kind, c.State)
		}
		if c.Count != wantCount {
			t.Errorf("card %q count = %d; want %d", kind, c.Count, wantCount)
		}
	}
}

// The positive control that matters most. A read model that is WIRED and did
// not answer must read `unread`, never `live` with a zero: a dark upstream
// rendering as "nothing pending" silently deletes the card and the learner
// cannot tell.
func TestLearnerCards_AFailedReadIsUnreadNotAnEmptyLive(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		code int
		kind string
	}{
		{"pending 503", pathPending, http.StatusServiceUnavailable, "pending_diagnoses"},
		{"pending 500", pathPending, http.StatusInternalServerError, "pending_diagnoses"},
		{"pending 400", pathPending, http.StatusBadRequest, "pending_diagnoses"},
		{"transcript 503", pathTranscpt, http.StatusServiceUnavailable, "unseen_results"},
		{"transcript 500", pathTranscpt, http.StatusInternalServerError, "unseen_results"},
		{"budget 503", pathBudget, http.StatusServiceUnavailable, "practice_budget"},
		{"budget 500", pathBudget, http.StatusInternalServerError, "practice_budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := liveStub()
			stub.status[tc.path] = tc.code
			c, ok := learnerCards(t, stub, true)[tc.kind]
			if !ok {
				t.Fatalf("card %q was DROPPED on a %d; it must be emitted unread", tc.kind, tc.code)
			}
			if c.State != "unread" {
				t.Errorf("card %q state = %q on a %d; want unread", tc.kind, c.State, tc.code)
			}
			if c.State == "live" && c.Count == 0 {
				t.Errorf("card %q renders as an empty LIVE card: a dark upstream reading as "+
					"'nothing pending' deletes the card and the learner cannot tell", tc.kind)
			}
		})
	}
}

// No consumption downstream at all in this deployment: absent, which is a
// different fact from unread and must not be confused with it.
func TestLearnerCards_NoDownstreamAtAllIsAbsent(t *testing.T) {
	cards := learnerCards(t, liveStub(), false)

	for _, kind := range []string{"pending_diagnoses", "unseen_results", "practice_budget"} {
		c, ok := cards[kind]
		if !ok {
			t.Errorf("card %q missing entirely; it must be emitted absent", kind)
			continue
		}
		if c.State != "absent" {
			t.Errorf("card %q state = %q with no downstream configured; want absent", kind, c.State)
		}
	}
}

// A live read model with genuinely nothing behind it is `live` with count 0,
// which is a FACT the client may drop from the list. It must not be confused
// with unread in either direction.
func TestLearnerCards_GenuinelyEmptyIsLiveWithZeroNotUnread(t *testing.T) {
	stub := liveStub()
	stub.body[pathPending] = `{"items":[]}`
	stub.body[pathTranscpt] = `{"items":[],"unseen_count":0}`

	cards := learnerCards(t, stub, true)
	for _, kind := range []string{"pending_diagnoses", "unseen_results"} {
		c := cards[kind]
		if c.State != "live" {
			t.Errorf("card %q state = %q on a clean empty answer; want live", kind, c.State)
		}
		if c.Count != 0 {
			t.Errorf("card %q count = %d; want 0", kind, c.Count)
		}
	}
}

// ⚠ The practice budget's `used` is NOT derivable from cap minus remaining: the
// caps are tunable DOWN, so a learner can hold more spent requests than the new
// cap. The card reports REMAINING taps and must read the field rather than
// compute it.
func TestLearnerCards_PracticeBudgetReadsRemainingRatherThanDerivingIt(t *testing.T) {
	stub := liveStub()
	// cap tuned down below used: remaining is clamped at 0, used is the truth.
	stub.body[pathBudget] = `{"resets_at":"2026-09-03T00:00:00Z","budgets":[` +
		`{"origin":"tap","cap":1,"used":5,"remaining":0,"exhausted":true}]}`

	c := learnerCards(t, stub, true)["practice_budget"]
	if c.Count != 0 {
		t.Errorf("count = %d; want 0 remaining. cap minus remaining would give 1, "+
			"and cap minus used would give a negative", c.Count)
	}
}

// Every card carries a route, or the learner is shown a count they cannot act
// on.
func TestLearnerCards_EveryCardCarriesARoute(t *testing.T) {
	for kind, c := range learnerCards(t, liveStub(), true) {
		if c.Route == "" {
			t.Errorf("card %q has no route: a count with nowhere to go", kind)
		}
		if c.Surface == "" {
			t.Errorf("card %q has no surface", kind)
		}
	}
}

// A failed card read does NOT flip the page-level `partial`.
//
// The card carries its own state, per kind, in the same response, so a failed
// read is already reported where the reader is looking. Flipping `partial`
// would degrade the whole page for every consumer that has read that field
// since WS-5b on the strength of one card, and would double-report what the
// card already says.
//
// ⚠ The cost is real and is raised with the orchestrator, not hidden: an
// unread card does not say WHY, so 503, 500 and 400 are indistinguishable on
// the wire, and the rank key calls the 400 case a wiring bug.
func TestLearnerCards_AFailedCardReadDoesNotDegradeTheWholePage(t *testing.T) {
	stub := liveStub()
	stub.status[pathPending] = http.StatusServiceUnavailable
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	// Identity MUST be wired here. Without it /me fails, part_errors records it
	// and partial is true for a reason that has nothing to do with card reads,
	// which would make the assertion below pass or fail on the wrong cause.
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"display_name":"Dale"}`))
	}))
	t.Cleanup(identity.Close)

	agg := New(Config{ConsumptionURL: srv.URL, IdentityURL: identity.URL})
	resp, _ := agg.GetDashboard(context.Background(),
		AuthCtx{TenantID: "t-1", GCID: "gcid-1", Roles: []string{"learner"}, TimeZone: "UTC"})

	var got struct {
		Partial    bool              `json:"partial"`
		PartErrors map[string]string `json:"part_errors"`
		CardErrors map[string]string `json:"card_errors"`
		Cards      []cardWire        `json:"cards"`
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.PartErrors["pending_diagnoses"] != "" {
		t.Errorf("card read recorded in part_errors (%q): it is double-reported and flips partial",
			got.PartErrors["pending_diagnoses"])
	}
	if got.Partial {
		t.Error("partial flipped on a card read; the page is not degraded by one card")
	}
	// The signal that MUST survive: the card itself says it could not be read.
	var found bool
	for _, c := range got.Cards {
		if c.Kind == "pending_diagnoses" {
			found = true
			if c.State != "unread" {
				t.Errorf("card state = %q; want unread. Dropping the part_error is only "+
					"defensible while the CARD carries the signal", c.State)
			}
		}
	}
	if !found {
		t.Error("the card is gone entirely, so nothing reports the failed read")
	}
}

// card_errors names WHY an unread card could not be read, keyed by card kind.
//
// Its own map, beside part_errors and never inside it, so an operator gains the
// status class the card state cannot carry without the page-level `partial`
// flipping for every consumer on the strength of one card.
func TestLearnerCards_CardErrorsNamesTheReasonWithoutDegradingThePage(t *testing.T) {
	stub := liveStub()
	stub.status[pathPending] = http.StatusServiceUnavailable
	stub.status[pathBudget] = http.StatusInternalServerError
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	// Identity MUST be wired here. Without it /me fails, part_errors records it
	// and partial is true for a reason that has nothing to do with card reads,
	// which would make the assertion below pass or fail on the wrong cause.
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"display_name":"Dale"}`))
	}))
	t.Cleanup(identity.Close)

	agg := New(Config{ConsumptionURL: srv.URL, IdentityURL: identity.URL})
	resp, _ := agg.GetDashboard(context.Background(),
		AuthCtx{TenantID: "t-1", GCID: "gcid-1", Roles: []string{"learner"}, TimeZone: "UTC"})

	var got struct {
		Partial    bool              `json:"partial"`
		CardErrors map[string]string `json:"card_errors"`
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CardErrors["pending_diagnoses"] == "" {
		t.Error("an unread pending_diagnoses card names no reason in card_errors")
	}
	if got.CardErrors["practice_budget"] == "" {
		t.Error("an unread practice_budget card names no reason in card_errors")
	}
	if got.CardErrors["unseen_results"] != "" {
		t.Errorf("a LIVE card was given a reason (%q); only unread cards have one",
			got.CardErrors["unseen_results"])
	}
	if got.Partial {
		t.Error("card_errors flipped partial; that is the coupling this map exists to avoid")
	}
}

// With no downstream at all the cards are ABSENT, which is a structural gap
// already named in known_gaps, not a failure with a reason.
func TestLearnerCards_AbsentCardsGetNoCardError(t *testing.T) {
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"display_name":"Dale"}`))
	}))
	t.Cleanup(identity.Close)

	agg := New(Config{IdentityURL: identity.URL})
	resp, _ := agg.GetDashboard(context.Background(),
		AuthCtx{TenantID: "t-1", GCID: "gcid-1", Roles: []string{"learner"}, TimeZone: "UTC"})

	var got struct {
		CardErrors map[string]string `json:"card_errors"`
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.CardErrors) != 0 {
		t.Errorf("absent cards recorded as failures: %v", got.CardErrors)
	}
}

// ---- the instructor hand-off card (ruling 4) --------------------------------
//
// Plan 4.3: role cards appear inline only for roles HELD, and hand off into the
// owning surface with a count and a route, never a re-implementation. This one
// ships from the live instructorCourses states and carries NO queue depth,
// because the cross-assessment grading rollup that would supply it is C1c and
// the `grading_queue` card is still absent.

func instructorCards(t *testing.T, roles []string, deliveryBody string, deliveryWired bool) map[string]cardWire {
	t.Helper()
	consumption := httptest.NewServer(liveStub().handler())
	t.Cleanup(consumption.Close)
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"display_name":"Dale"}`))
	}))
	t.Cleanup(identity.Close)
	delivery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(deliveryBody))
	}))
	t.Cleanup(delivery.Close)

	cfg := Config{ConsumptionURL: consumption.URL, IdentityURL: identity.URL}
	if deliveryWired {
		cfg.DeliveryURL = delivery.URL
	}
	resp, _ := New(cfg).GetDashboard(context.Background(),
		AuthCtx{TenantID: "t-1", GCID: "gcid-1", Roles: roles, TimeZone: "UTC"})

	var got dashWire
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, resp.Body)
	}
	byKind := map[string]cardWire{}
	for _, c := range got.Cards {
		byKind[c.Kind] = c
	}
	return byKind
}

// The delivery shape is {"items":[{"id","title","enrolled_count"}]}, read off
// deliveryInstructorCourses rather than guessed. A bare array decodes to an
// empty Items and the card reads live-with-zero, which looks exactly like an
// instructor who teaches nothing.
const twoCourses = `{"items":[{"id":"c-1","title":"Algebra"},{"id":"c-2","title":"Calculus"}]}`

func TestInstructorCard_LiveForARoleHeld(t *testing.T) {
	c, ok := instructorCards(t, []string{"learner", "instructor"}, twoCourses, true)["instructor_courses"]
	if !ok {
		t.Fatal("no instructor card for a session that holds the role")
	}
	if c.State != "live" {
		t.Errorf("state = %q; want live", c.State)
	}
	if c.Count != 2 {
		t.Errorf("count = %d; want 2", c.Count)
	}
	if c.Surface != "r" {
		t.Errorf("surface = %q; want r: this is a HAND-OFF into the owning surface", c.Surface)
	}
	// /r/catalog, verified mounted in rplus.routes.ts. /r/courses does not
	// exist: I wrote it first and it would have repeated the not-found class
	// this same commit is fixing for five other cards.
	if c.Route != "/r/catalog" {
		t.Errorf("route = %q; want /r/catalog", c.Route)
	}
}

// A learner-only session is not shown an instructor card at all. Plan 4.3:
// inline only for roles HELD. An absent card here would offer a route the
// route guard bounces.
func TestInstructorCard_NotEmittedForALearnerOnlySession(t *testing.T) {
	if c, ok := instructorCards(t, []string{"learner"}, twoCourses, true)["instructor_courses"]; ok {
		t.Errorf("a learner-only session was shown an instructor card: %+v", c)
	}
}

// The role is held but the downstream is not configured: that is a structural
// gap, so the card is ABSENT and says why, never live with a zero that would
// read as "you teach nothing".
func TestInstructorCard_UnconfiguredDeliveryIsAbsentNotAnEmptyLive(t *testing.T) {
	c, ok := instructorCards(t, []string{"instructor"}, twoCourses, false)["instructor_courses"]
	if !ok {
		t.Fatal("card missing entirely; it must be emitted absent")
	}
	if c.State != "absent" {
		t.Errorf("state = %q with no delivery configured; want absent", c.State)
	}
	if c.Count != 0 {
		t.Errorf("count = %d on an absent card; want 0", c.Count)
	}
}

// A role held with genuinely no courses is live with zero, which is a FACT.
func TestInstructorCard_NoCoursesIsLiveWithZero(t *testing.T) {
	c := instructorCards(t, []string{"instructor"}, `{"items":[]}`, true)["instructor_courses"]
	if c.State != "live" || c.Count != 0 {
		t.Errorf("state = %q count = %d; want live and 0 for an instructor who teaches nothing",
			c.State, c.Count)
	}
}

// ⚠ It carries NO queue depth. averageScorePct and pendingReviews both need the
// cross-assessment grading rollup that does not exist (C1c), and the separate
// grading_queue card is still absent. A count of COURSES must never be rendered
// or read as a count of things waiting.
func TestInstructorCard_CountsCoursesNotAQueue(t *testing.T) {
	cards := instructorCards(t, []string{"instructor"}, twoCourses, true)
	if c := cards["instructor_courses"]; c.Urgency == urgencyBlocksOther {
		t.Errorf("the courses card claims band %d, which means someone else is BLOCKED; "+
			"that band belongs to grading_queue, which has no read model yet", c.Urgency)
	}
	if c, ok := cards["grading_queue"]; !ok || c.State != "absent" {
		t.Error("grading_queue must still be absent: nothing supplies queue depth yet")
	}
}
