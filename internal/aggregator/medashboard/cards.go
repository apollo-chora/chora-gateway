package medashboard

// cards.go: the ranked home card envelope (UX Track U, C1 backend half).
//
// Contract: docs/references/home-rank-key.md. The BACKEND composes cards and
// orders them; the frontend renders them in the order given, so the ordering
// here is the contract and not an implementation detail.
//
// Two rules run through everything below.
//
// A card whose data could not be read is EMITTED with its state set, never
// dropped. An empty home and an unread one are different facts, and dropping
// the card makes a dark upstream look like a quiet day.
//
// A card is never emitted claiming something the data does not support. Where
// a band cannot be computed honestly the card carries the lower band, because
// over-ranking a card steals the top of the page from something that really is
// due.

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// cardDTO is the home card envelope. Every field is present on every card;
// only deadline_at is nullable, and it renders as an explicit null rather than
// being omitted so the client never has to tell "no deadline" from "the key was
// not served".
type cardDTO struct {
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

// Card states (rank key section 5).
const (
	cardStateLive   = "live"
	cardStateUnread = "unread"
	cardStateAbsent = "absent"
)

// Urgency bands (rank key section 3). Named rather than inlined so a band can
// be argued about by name in a review.
const (
	urgencyOverdue      = 5
	urgencyDueToday     = 4
	urgencyBlocksOther  = 3
	urgencyStreakAtRisk = 2
	urgencyContinuation = 1
	urgencyNone         = 0
)

// resolveBandsZone returns the location the day bands are computed in, and the
// name to ECHO to the client.
//
// An unknown or empty zone falls back to UTC and SAYS SO. That echo is the
// whole point: a silent UTC fallback would have the client believe due-today
// was computed in its own zone, which is wrong for up to a day at the edges,
// and the client cannot detect it. With the echo the client can refine bands 4
// and 5 from deadline_at itself whenever the zone it gets back is not its own.
func resolveBandsZone(tz string) (*time.Location, string) {
	name := strings.TrimSpace(tz)
	if name == "" {
		return time.UTC, "UTC"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC, "UTC"
	}
	return loc, name
}

// streakBucketIsStale reports whether a streak day bucket proves the learner
// has NOT been active in the viewer's today.
//
// ⚠ The stamp is a DAY BUCKET normalised to UTC midnight by chora-consumption,
// not a moment. It proves a day and never an hour. So the only safe reading is
// from the LAST possible moment inside the bucket: if any instant in that
// bucket could still fall on the viewer's today, the streak is NOT provably at
// risk. Reading the bucket as a moment would tell a learner who studied minutes
// ago that their streak is about to break, which is a new defect dressed as a
// helpful nudge.
func streakBucketIsStale(bucket time.Time, now time.Time, loc *time.Location) bool {
	if bucket.IsZero() {
		return false // no bucket at all proves nothing about today
	}
	lastMomentInBucket := bucket.UTC().Add(24*time.Hour - time.Nanosecond)
	localNow := now.In(loc)
	startOfLocalToday := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, loc)
	return lastMomentInBucket.Before(startOfLocalToday)
}

// learnerReads carries the C1 slice B fan-out results into card composition.
//
// consumptionWired is separate from the three results on purpose. It is the
// only thing that can tell ABSENT from UNREAD: a deployment with no consumption
// downstream has no source for these cards at all, while a configured one that
// did not answer is a card the learner has and could not be shown. Collapsing
// the two would make a dark upstream indistinguishable from a feature that was
// never deployed, which is the distinction this whole envelope exists for.
type learnerReads struct {
	consumptionWired bool
	pending          callResult
	transcript       callResult
	budget           callResult
	// maps feeds the C4 defend_hex card: the Atlas read carries a cooling
	// count, a hex label and the stationed companion per map. See defend.go.
	maps callResult

	// instructorRoleHeld gates the hand-off card: plan 4.3 says role cards
	// appear inline ONLY for roles held, or the home offers a route the guard
	// bounces. deliveryWired and instructor tell absent from live, the same
	// distinction the learner cards draw.
	instructorRoleHeld bool
	deliveryWired      bool
	instructor         callResult
}

// instructorState maps the delivery fan-out onto the section 5 states.
//
// A role held with NO delivery downstream is ABSENT, never live with a zero: a
// zero would read as "you teach nothing", which is a confident false claim
// about somebody's job.
func (l learnerReads) instructorState() string {
	if !l.deliveryWired {
		return cardStateAbsent
	}
	if !l.instructor.ok() {
		return cardStateUnread
	}
	return cardStateLive
}

// cardState maps one fan-out result onto the section 5 states.
//
// A non-2xx is UNREAD and never live-with-zero. 503 means unwired downstream,
// 500 a repository error, 400 a call this aggregator built wrong: all three are
// "we could not read it", and all three must stay distinguishable from "there
// is nothing". The learner sees a card that says it could not be read.
//
// The card's own state is the ONLY signal for these three: they are
// deliberately not recorded in part_errors, because that would double-report
// what the card already says and flip the page-level `partial` for every
// existing consumer on the strength of one card. GetDashboard carries the
// reasoning and the cost.
func (l learnerReads) state(cr callResult) string {
	if !l.consumptionWired {
		return cardStateAbsent
	}
	if !cr.ok() {
		return cardStateUnread
	}
	return cardStateLive
}

// countFrom decodes a count out of a result, answering 0 for anything it could
// not read. The STATE carries whether that zero is a fact, so the caller never
// has to encode "unknown" in the number itself.
func countFrom(cr callResult, decode func([]byte) int) int {
	if !cr.ok() {
		return 0
	}
	return decode(cr.body)
}

// composeCards builds the ranked card list from the parts already fetched.
//
// It takes no new downstream call on purpose: this is the envelope contract the
// frontend builds against, and a new fan-out riding inside it would couple the
// contract's correctness to a call that can fail.
// The second return is card_context: the sibling map keyed by card_id holding
// what a card needs to say and the envelope has no field for. Nil when no card
// has anything to add, so the key is omitted entirely.
func composeCards(dto *dashboardDTO, now time.Time, loc *time.Location, reads learnerReads) ([]cardDTO, map[string]map[string]string) {
	cards := []cardDTO{}
	cardCtx := map[string]map[string]string{}

	// Pending diagnoses (band 1). Work the learner STARTED and parked awaiting
	// review: in-progress with no deadline, which is what band 1 names. The
	// warmest of the three because it is the curiosity card the rank key calls
	// the highest-value thing on the page.
	cards = append(cards, cardDTO{
		CardID: "pending_diagnoses", Kind: "pending_diagnoses", Surface: "a",
		Route:   "/a/knowledge",
		Count:   countFrom(reads.pending, decodeItemCount),
		Urgency: urgencyContinuation, Warmth: 85,
		State: reads.state(reads.pending),
	})

	// Unseen results (band 0). New information rather than work in progress, so
	// nothing time-derived: it rides on warmth alone.
	cards = append(cards, cardDTO{
		CardID: "unseen_results", Kind: "unseen_results", Surface: "a",
		Route: "/a/me/transcript",
		// The ROLL-UP, not the length of the returned page. The page is capped
		// and a badge built from its length undercounts exactly the learner the
		// badge exists for.
		Count:   countFrom(reads.transcript, decodeUnseenCount),
		Urgency: urgencyNone, Warmth: 75,
		State: reads.state(reads.transcript),
	})

	// Practice budget (band 0). Informational: it explains the second lane
	// rather than asking for work, so it is the coolest of the three and sits
	// at the bottom of its band by design.
	cards = append(cards, cardDTO{
		CardID: "practice_budget", Kind: "practice_budget", Surface: "a",
		Route:   "/a/knowledge",
		Count:   countFrom(reads.budget, decodeTapsRemaining),
		Urgency: urgencyNone, Warmth: 20,
		State: reads.state(reads.budget),
	})

	// Defend a cooling hex (band 0, C4). One card for the whole set, like
	// continue_learning: the count is every cooling hex across every map and the
	// route deep links to one of them by a stated rule. defend.go carries the
	// rule and why the companion name rides in card_context instead of here.
	defend, defendCtx := defendCard(reads)
	cards = append(cards, defend)
	if defendCtx != nil {
		cardCtx[defend.CardID] = defendCtx
	}

	// Streak at risk (band 2). Only when the bucket PROVES staleness; a streak
	// of zero has nothing to lose and is not a card.
	if dto.Streak != nil && dto.Streak.CurrentStreakDays > 0 {
		bucket := parseStamp(dto.Streak.LastCompletionAt)
		if streakBucketIsStale(bucket, now, loc) {
			cards = append(cards, cardDTO{
				CardID:  "streak_at_risk",
				Kind:    "streak_at_risk",
				Surface: "a",
				Route:   "/a/daily-dose",
				Count:   dto.Streak.CurrentStreakDays,
				Urgency: urgencyStreakAtRisk,
				// A streak is the warmest thing on a quiet page: it is the one
				// card that rewards returning rather than asking for work.
				Warmth: 70,
				State:  cardStateLive,
			})
		}
	}

	// Continuation (band 1), one card for the whole set rather than one per
	// path: the home offers a way BACK IN, it is not a second path list.
	//
	// It DEEP LINKS into the player for a specific course. A generic list route
	// would hand the learner back the same decision the card exists to make for
	// them, and /a/paths does not exist at all: it was invented in the first cut
	// and landed on the not-found, along with four others.
	if inProgress, courseID := continuation(dto.LearnerCourses); inProgress > 0 && courseID != "" {
		cards = append(cards, cardDTO{
			CardID:  "continue_learning",
			Kind:    "continue_learning",
			Surface: "a",
			Route:   "/a/courses/" + courseID + "/learn",
			Count:   inProgress,
			Urgency: urgencyContinuation,
			Warmth:  40,
			State:   cardStateLive,
		})
	}

	// The instructor hand-off (plan 4.3). Inline ONLY for a role held, and it
	// hands off into the owning surface with a count and a route, never a
	// re-implementation: the A+ home does not re-host R+.
	//
	// ⚠ It counts COURSES, not a queue. averageScorePct and pendingReviews both
	// need the cross-assessment grading rollup that does not exist (C1c), so
	// this card must never be read as "things waiting for you"; that is the
	// separate grading_queue card, still absent. Hence band 0 and not band 3:
	// teaching two courses does not mean anybody is blocked behind you.
	if reads.instructorRoleHeld {
		cards = append(cards, cardDTO{
			CardID: "instructor_courses", Kind: "instructor_courses",
			// Surface r: the work lives in R+ and the card is a doorway.
			// /r/catalog, verified in rplus.routes.ts. /r/courses does not
			// exist and would have repeated the very defect this commit fixes.
			Surface: "r", Route: "/r/catalog",
			Count:   len(dto.InstructorCourses),
			Urgency: urgencyNone, Warmth: 10,
			State: reads.instructorState(),
		})
	}

	// The two read models with NO downstream source anywhere (rank key rows 2
	// and 6). Emitted absent rather than omitted, so the card exists in the
	// contract from day one and lights up when C1c and C1d land, with no
	// envelope change and no frontend change.
	cards = append(cards,
		cardDTO{
			CardID: "grading_queue", Kind: "grading_queue", Surface: "r",
			Route: "/r/assessments", Urgency: urgencyBlocksOther, Warmth: 0,
			State: cardStateAbsent,
		},
		cardDTO{
			CardID: "tenants_needing_setup", Kind: "tenants_needing_setup", Surface: "h",
			Route: "/h/ready", Urgency: urgencyNone, Warmth: 0,
			State: cardStateAbsent,
		},
	)

	sortCards(cards)
	if len(cardCtx) == 0 {
		return cards, nil
	}
	return cards, cardCtx
}

// sortCards applies the documented total order: urgency desc, warmth desc,
// deadline_at asc, count desc, and card_id asc as the final tiebreak.
//
// The order has to be TOTAL. Two renders of the same data must never swap two
// cards, or the page moves under a reader who changed nothing, and card_id is
// the only field guaranteed unique per card.
func sortCards(cards []cardDTO) {
	sort.SliceStable(cards, func(i, j int) bool {
		a, b := cards[i], cards[j]
		if a.Urgency != b.Urgency {
			return a.Urgency > b.Urgency
		}
		if a.Warmth != b.Warmth {
			return a.Warmth > b.Warmth
		}
		if ad, bd := deadlineKey(a), deadlineKey(b); ad != bd {
			return ad < bd
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.CardID < b.CardID
	})
}

// deadlineKey sorts a card with no deadline AFTER every card that has one:
// "nothing is due" cannot outrank a real due date inside the same band.
func deadlineKey(c cardDTO) string {
	if c.DeadlineAt == nil {
		return "￿"
	}
	return *c.DeadlineAt
}

// cardKnownGaps names why a structurally absent card is absent. The reason is
// the payload: an absent card with no reason is a mystery the operator cannot
// act on, which is the state this whole envelope exists to prevent.
func cardKnownGaps() map[string]string {
	return map[string]string{
		"grading_queue": "no cross-assessment grading rollup in chora-delivery yet; " +
			"every count today is a browser-side filter over ONE assessment's submissions (C1c)",
		"tenants_needing_setup": "nothing enumerates tenants by setup progress in chora-tenancy yet; " +
			"the RBAC gate on the operator tile is live but the list is not (C1d)",
	}
}

// continuation counts the courses mid-traversal and returns the one to link to.
//
// A course at zero has not been started and is not a continuation; one at 100
// percent is finished.
//
// ⚠ THE LINKED COURSE IS THE FIRST IN THE UPSTREAM ORDER, NOT THE MOST RECENT.
// Neither learnerCourseDTO nor the consumptionPathItem it is built from carries
// any timestamp, so recency is not derivable from this read and calling it "the
// most recently touched" would be a claim the data cannot support. It is
// deterministic, which is what the ordering contract needs; if recency is what
// the card should actually offer, the learning-paths read has to grow a stamp
// first. Raised with the orchestrator.
//
// A course with no id is skipped rather than linked: a route of
// /a/courses//learn is a broken link dressed as an action.
func continuation(courses []learnerCourseDTO) (int, string) {
	n, courseID := 0, ""
	for _, c := range courses {
		if c.ProgressPct <= 0 || c.ProgressPct >= 100 {
			continue
		}
		n++
		if courseID == "" {
			courseID = c.CourseID
		}
	}
	return n, courseID
}

// parseStamp reads an RFC3339 stamp, answering the zero time for anything it
// cannot read. Callers treat the zero time as "proves nothing", never as "long
// ago": a stamp we failed to parse must not rank a learner as neglected.
func parseStamp(s string) time.Time {
	if strings.TrimSpace(s) == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// decodeItemCount reads the length of an `items` list.
func decodeItemCount(body []byte) int {
	var w struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return 0
	}
	return len(w.Items)
}

// decodeUnseenCount reads the transcript ROLL-UP, never the page length.
func decodeUnseenCount(body []byte) int {
	var w struct {
		UnseenCount int `json:"unseen_count"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return 0
	}
	return w.UnseenCount
}

// decodeTapsRemaining reads the TAP lane's remaining allowance.
//
// ⚠ It reads `remaining` and never derives it. `used` is the true count and is
// NOT clamped, while `remaining` IS clamped at zero, so an owner who tunes a cap
// down leaves `used` above `cap` legitimately: cap minus used would go negative
// and cap minus remaining would overstate. The two clamps run in opposite
// directions on purpose (B6 item 3).
//
// The `march` lane is deliberately not summed in. The two draw on independent
// allowances, and adding them would offer a learner taps the enforcement path
// will refuse because the tap allowance alone is spent.
func decodeTapsRemaining(body []byte) int {
	var w struct {
		Budgets []struct {
			Origin    string `json:"origin"`
			Remaining int    `json:"remaining"`
		} `json:"budgets"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return 0
	}
	for _, b := range w.Budgets {
		if b.Origin == "tap" {
			return b.Remaining
		}
	}
	return 0
}

// deadlineUrgency returns the deadline-derived band for a card, or
// urgencyNone when the deadline does not put it in one.
//
// Bands 5 and 4 are the ONLY deadline-derived bands, and that asymmetry is why
// this has to be right on the server. A client holding deadline_at and bands_tz
// can refine WITHIN {4, 5} when the echoed zone is not its own, but it can never
// DEMOTE a card out of them: bands 3 to 1 are not deadline-derived, so a client
// that decided a card was not due today would have no way to know which band it
// belongs in instead. A band 4 emitted wrongly is therefore pinned near the top
// of the page with nothing able to correct it.
//
// So a deadline that is neither past nor inside the viewer's local day returns
// urgencyNone and the caller falls back to the card's own band. The failure this
// closes: 2026-09-02T20:00Z is today in UTC and 2026-09-03T04:00 in
// Asia/Singapore, which is tomorrow there.
func deadlineUrgency(deadline *time.Time, now time.Time, loc *time.Location) int {
	if deadline == nil {
		return urgencyNone
	}
	if deadline.Before(now) {
		return urgencyOverdue
	}
	local := deadline.In(loc)
	localNow := now.In(loc)
	if local.Year() == localNow.Year() && local.YearDay() == localNow.YearDay() {
		return urgencyDueToday
	}
	return urgencyNone
}

// cardErrors names WHY each unread card could not be read, keyed by card kind.
//
// Deliberately its own map rather than an entry in part_errors. part_errors
// feeds `partial`, so putting a card read there would degrade the whole page
// for every consumer that has read that field since WS-5b on the strength of
// one card failing, and would double-report what the card's own state already
// says. This map is additive: a client that ignores it loses nothing, and an
// operator gains the status class the card state cannot carry.
//
// Present ONLY for cards that are unread. A live card has nothing to explain,
// and an absent one is a structural gap already named in known_gaps.
func cardErrors(reads learnerReads) map[string]string {
	if !reads.consumptionWired {
		return nil // every card is absent: a gap, not a failure
	}
	out := map[string]string{}
	for kind, cr := range map[string]callResult{
		"pending_diagnoses": reads.pending,
		"unseen_results":    reads.transcript,
		"practice_budget":   reads.budget,
	} {
		if !cr.ok() {
			out[kind] = downstreamErrCode(cr)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
