// Package medashboard composes the A+ multi-role dashboard DTO that no
// single downstream service serves. A6 follow-up (CHO-1545; Phyllis
// steps 1b/6) per docs/m13/handoff-fe-to-be-service-2026-05-14.md §A6
// GAP #2.
//
// The FE contract is chora-web/src/app/features/surfaces/aplus/dashboard/
// dashboard.model.ts — the `DashboardSummary` interface. This aggregator
// fans out to the REAL downstream endpoints that hold each piece
// (verified by reading each downstream's HTTP adapter) and joins them
// into the FE-expected shape:
//
//	DashboardSummary field        downstream source
//	───────────────────────────   ────────────────────────────────────────
//	userDisplayName               chora-identity     GET /me            .display_name
//	gcidPillLabel                 synthesised gateway-side (a UI label — NOT
//	                              domain data; mirrors the FE wave-3 fixture
//	                              "ONE IDENTITY · GCID active")
//	currentStreakDays             chora-consumption  GET /v1/me/streak  .count
//	streak.current_streak_days    chora-consumption  GET /v1/me/streak  .count
//	streak.last_completion_at     chora-consumption  GET /v1/me/streak  .last_activity_at
//	learnerCourses[]              chora-consumption  GET /v1/me/learning-paths
//	                              .items[]  (mePathSummary — course_id, title,
//	                              progress_percent, current_index, total_atoms)
//	learnerCourses[].retention_state  ⚠ NO DOWNSTREAM SOURCE — see GAP note.
//	                              Field is OMITTED via omitempty (graceful
//	                              MISSING). FE renders its own `unknown` dot
//	                              when absent. WS-5c-BE-A1 follow-up.
//	companions[]                   chora-consumption  GET /v1/me/companions
//	                              .items[]  (instanceResp + inline
//	                              growth_state). Projection:
//	                              companion_id ← instance.companion_id
//	                              name ← instance.name
//	                              species ← growth_state.current_breed
//	                                        (revealed breed), fallback to
//	                                        instance.specialization when unrevealed
//	                              subject ← instance.specialization
//	                              evolution_level ← growth_state.stage
//	                              stage_label ← growth_state.stage_name
//	instructorCourses[]           chora-delivery     GET /api/v1/instructors/
//	                              {instructor_gcid}/courses  .items[]
//	                              (publicCourseDTO: id, title, enrolled_count)
//
// ── instructorCourses[]: role-gated, three honest states ────────────
//
// The GAP note that used to sit here said chora-delivery exposes no
// "courses I instruct" endpoint. That stopped being true when
// instructor_courses_handler.go shipped GET
// /api/v1/instructors/{instructor_gcid}/courses, and leaving the claim in
// place dark-filled an entire identity's lane on the A+ home. The
// aggregator now calls it, addressed by the CALLER'S OWN gcid, and keeps
// three states distinguishable:
//
//	fetched  the session holds instructor or admin → the list, with a
//	         part_errors entry when the call fails.
//	skipped  no such role → [] because they instruct nothing. A fact, so
//	         neither a gap nor an error. The gate exists to spare the
//	         learner majority a downstream call, not to authorise: delivery
//	         authorises a self-gcid read on its own.
//	unwired  SVC_DELIVERY_URL unset → [] plus a not_configured known_gap,
//	         so an unconfigured env never reads as "teaches nothing".
//
// Only courseId / title / studentsEnrolled have a source. The remaining
// four InstructorCourseSummary fields ride at their zero value (NEVER
// fabricated) and are declared per-field in known_gaps.
//
// ── GAP — partial LearnerCourseSummary field coverage ────────────────
//
// chora-consumption's /v1/me/learning-paths mePathSummary supplies
// course_id, title, progress_percent, current_index, total_atoms. The FE
// LearnerCourseSummary ALSO declares courseCode / instructorName /
// xpEarned / memoryPct / dayNumber / dayTotal — none are on the
// learning-paths projection. Those fields are emitted at their zero
// value (NOT fabricated); the FE dashboard component only strictly reads
// learnerCourses.length + courseId + title + progressPct today, so the
// zero-valued extras are non-blocking. Flagged in the BE→FE report.
//
// Graceful degradation: a partial downstream failure omits/empties that
// part — it is NEVER a whole-500 (the dashboard is the A+ landing
// surface). Mirrors phyllis.GetAtom. The response DTO carries
// `partial: true` + a `part_errors` map so the caller can distinguish
// empty-because-failed from empty-because-no-data.
//
// SVC_*_URL is read at cmd/server boot per memory feedback_no_inline_config
// — never inline. New() returns nil when ALL URLs are unset so callers
// can route-skip in unconfigured envs.
package medashboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apollo-chora/chora-common/auth/servicemesh"
)

// DefaultPerCallTimeout caps each downstream call. The dashboard parts
// are cheap (single in-memory / DB read on the downstream); 6s is
// generous headroom over the downstream SLOs.
const DefaultPerCallTimeout = 6 * time.Second

// Config wires the downstream service base URLs + per-call timeout. URLs
// are sourced from env via LoadConfigFromEnv per feedback_no_inline_config.
type Config struct {
	// IdentityURL is the chora-identity base (GET /me — display name).
	// Empty disables the userDisplayName part.
	IdentityURL string

	// ConsumptionURL is the chora-consumption base (GET /v1/me/streak +
	// GET /v1/me/learning-paths). Empty disables the streak +
	// learnerCourses parts.
	ConsumptionURL string

	// DeliveryURL is the chora-delivery base
	// (GET /api/v1/instructors/{instructor_gcid}/courses). Empty disables the
	// instructorCourses part, which is then declared in known_gaps as a
	// CONFIG gap rather than a per-request failure.
	DeliveryURL string

	// PerCallTimeout caps each downstream call. Defaults to DefaultPerCallTimeout.
	PerCallTimeout time.Duration
}

// ApplyDefaults sets the default per-call timeout when zero.
func (c *Config) ApplyDefaults() {
	if c.PerCallTimeout == 0 {
		c.PerCallTimeout = DefaultPerCallTimeout
	}
}

// LoadConfigFromEnv reads the SVC_*_URL env vars per feedback_no_inline_config.
// These are the SAME env vars the other aggregators already read — no new
// deployment config is introduced.
func LoadConfigFromEnv() Config {
	c := Config{
		IdentityURL:    os.Getenv("SVC_IDENTITY_URL"),
		ConsumptionURL: os.Getenv("SVC_CONSUMPTION_URL"),
		DeliveryURL:    os.Getenv("SVC_DELIVERY_URL"),
	}
	c.ApplyDefaults()
	return c
}

// AuthCtx carries the per-request mesh-trust + tracing values stamped on
// outbound calls. Populated by the BFF handler from validated JWT claims.
type AuthCtx struct {
	Bearer      string
	Traceparent string
	TenantID    string
	GCID        string
	RoleSummary map[string]any
	// Roles is the typed role list from the VALIDATED session / mesh claims —
	// never a client-supplied header. It becomes the `x-mesh-user-roles` header.
	// Every Chora role gate reads it and FAILS CLOSED (fail-open was deleted in
	// CHO-2072), so an AuthCtx without Roles is denied 100% of the time by any
	// role-gated downstream. Guarded by upstream/mesh_roles_guard_test.go.
	Roles []string

	// TimeZone is the viewer's IANA zone, forwarded from the ?tz= query
	// parameter the client sends (Intl.DateTimeFormat().resolvedOptions()).
	//
	// The home's urgency bands 4 and 5 are "due inside the viewer's local day",
	// which this aggregator cannot know on its own. Empty or unparseable means
	// the bands are computed in UTC and bands_tz ECHOES "UTC", so the client
	// can refine due-today itself rather than trusting a zone that was never
	// applied. It is a display hint and never an authorisation input.
	TimeZone string
}

// Response is the normalised aggregator output forwarded to the FE.
type Response struct {
	Status int
	Body   []byte
}

// Aggregator is the stateless A+ dashboard composer.
type Aggregator struct {
	cfg    Config
	client *http.Client
}

// New constructs an Aggregator. Returns nil when ALL downstream URLs are
// unset so callers can route-skip in unconfigured envs.
func New(cfg Config) *Aggregator {
	cfg.ApplyDefaults()
	if cfg.IdentityURL == "" && cfg.ConsumptionURL == "" && cfg.DeliveryURL == "" {
		return nil
	}
	return &Aggregator{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.PerCallTimeout + 1*time.Second},
	}
}

// -----------------------------------------------------------------------------
// HTTP plumbing
// -----------------------------------------------------------------------------

type callResult struct {
	status int
	body   []byte
	err    error
}

// ok reports whether the call reached the downstream with a 2xx.
func (cr callResult) ok() bool {
	return cr.err == nil && cr.status >= 200 && cr.status < 300
}

// call performs one outbound GET with mesh-trust headers stamped,
// honouring PerCallTimeout.
func (a *Aggregator) call(ctx context.Context, urlStr string, auth AuthCtx) callResult {
	if urlStr == "" {
		return callResult{err: errEmptyURL}
	}
	callCtx, cancel := context.WithTimeout(ctx, a.cfg.PerCallTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, urlStr, nil)
	if err != nil {
		return callResult{err: err}
	}
	req.Header.Set("Accept", "application/json")
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
		req.Header.Set("X-Chora-GCID", auth.GCID)
		// chora-identity's /me bearer-auth reads gcid from the validated
		// session; chora-consumption's requireContext reads the LOWERCASE
		// `gcid` header (returns "gcid required" without it). Stamp it
		// explicitly — mirrors the gatewayproxy + phyllis aggregators.
		req.Header.Set("gcid", auth.GCID)
	}
	mesh := servicemesh.MarshalToHeaders(servicemesh.MeshClaims{
		GCID:        auth.GCID,
		TenantID:    auth.TenantID,
		Roles:       auth.Roles,
		RoleSummary: auth.RoleSummary,
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
	return callResult{status: resp.StatusCode, body: out}
}

// errEmptyURL is returned by call when the configured downstream URL is
// empty — that part is then treated as a (gracefully-degraded) failure.
var errEmptyURL = &emptyURLError{}

type emptyURLError struct{}

func (*emptyURLError) Error() string { return "medashboard: downstream url not configured" }

// -----------------------------------------------------------------------------
// Downstream payload shapes (only the fields this aggregator reads)
// -----------------------------------------------------------------------------

// identityMe mirrors chora-identity's meResponse — only display_name is
// read here.
type identityMe struct {
	DisplayName string `json:"display_name"`
}

// consumptionStreak mirrors chora-consumption's streakResp. WS-5b folds
// both `count` (the flat scalar — preserved on the wire for FE
// backwards-compat) and `last_activity_at` (renamed to
// `last_completion_at` on the wire to match the FE WS-5 contract) into
// a structured `streak` block.
type consumptionStreak struct {
	Count          int    `json:"count"`
	LastActivityAt string `json:"last_activity_at"`
}

// consumptionCompanionItem mirrors chora-consumption's instanceResp (the
// roster envelope element). Only the fields WS-5b projects into the
// CompanionRosterItem wire shape are read here — the rest are ignored.
type consumptionCompanionItem struct {
	CompanionID    string                      `json:"companion_id"`
	Name           string                      `json:"name"`
	Specialization string                      `json:"specialization"`
	GrowthState    *consumptionCompanionGrowth `json:"growth_state,omitempty"`
}

type consumptionCompanionGrowth struct {
	Stage     int    `json:"stage"`
	StageName string `json:"stage_name"`
	// CurrentBreed is the Companion's REVEALED breed (owl/fox/dragon/...),
	// empty until the breed-reveal ceremony fires. species projects from
	// this; specialization is the SUBJECT axis, not the breed.
	CurrentBreed string `json:"current_breed"`
}

type consumptionCompanionRoster struct {
	Items []consumptionCompanionItem `json:"items"`
}

// consumptionSourceTypeCourse is the ADR-233 D2 provenance value marking a
// learning path as derived from a chora-delivery Course. Mirrored from
// chora-consumption's learning_path.SourceTypeCourse — the gateway keeps its
// own copy because it decodes the WIRE, not the domain type. Migration 0093's
// CHECK admits exactly three values; the other two are "collection" (a WS-4
// study list) and "ad_hoc" (hand-rolled), NEITHER of which is a course.
const consumptionSourceTypeCourse = "course"

// consumptionPathItem mirrors chora-consumption's mePathSummary — the
// fields the FE LearnerCourseSummary needs.
type consumptionPathItem struct {
	CourseID string `json:"course_id"`
	// SourceType is the ADR-233 D2 provenance discriminator
	// (course | collection | ad_hoc), projected onto the wire by
	// chora-consumption commit c56292633. It is what makes a study list
	// distinguishable from a course — CourseID cannot do it, being the
	// delivery BINDING rather than provenance.
	SourceType      string  `json:"source_type"`
	Title           string  `json:"title"`
	CurrentIndex    int     `json:"current_index"`
	TotalAtoms      int     `json:"total_atoms"`
	ProgressPercent float64 `json:"progress_percent"`
}

type consumptionLearningPaths struct {
	Items []consumptionPathItem `json:"items"`
}

// deliveryCourseItem mirrors chora-delivery's publicCourseDTO. Only the three
// fields the FE InstructorCourseSummary can actually be filled from are read;
// the rest of that DTO (price, tags, syllabus counts) is catalogue data the
// dashboard card does not show.
type deliveryCourseItem struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	EnrolledCount int    `json:"enrolled_count"`
}

type deliveryInstructorCourses struct {
	Items []deliveryCourseItem `json:"items"`
}

// instructorRoles are the typed roles that make a by-instructor course read
// worth performing. chora-delivery authorises the read on self-gcid alone, so
// this gate is about not spending a downstream call for the learner majority,
// not about authorisation. Matched case-insensitively because the role string
// reaches the mesh header lowercased but the session may carry either case.
var instructorRoles = map[string]bool{"instructor": true, "admin": true}

// instructsSomething reports whether this session could plausibly own courses.
// An AuthCtx with no Roles fails CLOSED (no call, empty array), matching every
// other Chora role gate since CHO-2072.
func instructsSomething(auth AuthCtx) bool {
	for _, r := range auth.Roles {
		if instructorRoles[strings.ToLower(strings.TrimSpace(r))] {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// FE-facing DTO — chora-web .../dashboard/dashboard.model.ts DashboardSummary
// -----------------------------------------------------------------------------

// learnerCourseDTO is the FE LearnerCourseSummary (camelCase per the
// Angular interface). Fields with no downstream source are emitted at
// their zero value — see the package-doc GAP note.
type learnerCourseDTO struct {
	CourseID       string  `json:"courseId"`
	CourseCode     string  `json:"courseCode"` // GAP — no downstream source
	Title          string  `json:"title"`
	InstructorName string  `json:"instructorName"` // GAP — no downstream source
	ProgressPct    float64 `json:"progressPct"`
	RemainingAtoms int     `json:"remainingAtoms"`
	XPEarned       int     `json:"xpEarned"`  // GAP — no downstream source
	MemoryPct      int     `json:"memoryPct"` // GAP — no downstream source
	DayNumber      int     `json:"dayNumber"` // GAP — no downstream source
	DayTotal       int     `json:"dayTotal"`  // GAP — no downstream source
	// RetentionState is the per-course Ebbinghaus retention dot fed by
	// the (currently MISSING — WS-5c-BE-A1 follow-up) chora-consumption
	// retention envelope. OMITTED from the wire when no upstream source
	// is wired (graceful MISSING per feedback_no_stubs_real_wiring —
	// NEVER fabricated to "unknown"). FE defaults to its own `unknown`
	// dot on absence per the WS-5 fallback contract.
	RetentionState string `json:"retention_state,omitempty"`
}

// companionRosterDTO is the FE CompanionRosterItem (snake_case per the
// Angular interface — chora-web/src/app/features/surfaces/aplus/
// dashboard/dashboard.model.ts §CompanionRosterItem).
type companionRosterDTO struct {
	CompanionID string `json:"companion_id"`
	Name        string `json:"name"`
	Species     string `json:"species"`
	// Subject is the real specialty axis (← instance.specialization), forwarded
	// so the FE can open the topic-matched specialist Companion from a Growth Edge
	// (species is the display stopgap; subject is for matching).
	Subject        string `json:"subject,omitempty"`
	EvolutionLevel int    `json:"evolution_level"`
	StageLabel     string `json:"stage_label"`
}

// streakSummaryDTO is the FE StreakSummary block (snake_case per the
// Angular interface — chora-web/src/app/features/surfaces/aplus/
// dashboard/dashboard.model.ts §StreakSummary). BFF renames
// `last_activity_at` → `last_completion_at` to match the FE daily-dose-
// service terminology.
type streakSummaryDTO struct {
	CurrentStreakDays int    `json:"current_streak_days"`
	LastCompletionAt  string `json:"last_completion_at,omitempty"`
}

// dashboardDTO is the FE DashboardSummary plus the graceful-degradation
// envelope fields. The FE consumes the typed fields; the envelope fields
// let the BFF caller distinguish empty-because-failed from
// empty-because-no-data:
//
//   - partial      — true when a WIREABLE part's downstream call failed
//     this request (a transient/recoverable condition).
//   - part_errors  — per-part failure reason for the wireable parts that
//     failed THIS request.
//   - known_gaps   — per-part reason for parts with NO downstream source
//     at all (a permanent structural gap, NOT a transient
//     failure). This is informational and does NOT flip
//     `partial` — a clean fan-out is still partial=false.
type dashboardDTO struct {
	GcidPillLabel     string             `json:"gcidPillLabel"`
	UserDisplayName   string             `json:"userDisplayName"`
	CurrentStreakDays int                `json:"currentStreakDays"`
	LearnerCourses    []learnerCourseDTO `json:"learnerCourses"`
	InstructorCourses []instructorCourse `json:"instructorCourses"`
	// Companions is the WS-5b N-Companion roster strip — always [] (never
	// null) so the FE @for tracker doesn't NPE.
	Companions []companionRosterDTO `json:"companions"`
	// Streak is the WS-5b structured streak block from chora-consumption.
	// Pointer so an absent upstream / failed fan-out OMITS the field
	// rather than emitting a zero-value block (NEVER fabricate).
	Streak *streakSummaryDTO `json:"streak,omitempty"`
	// Cards is the ranked home (UX Track U C1, docs/references/home-rank-key.md).
	// ALWAYS a list, never null, so the client's iterator cannot NPE on a
	// dashboard with nothing to show. Ordered by the aggregator; the client
	// renders in the order given.
	Cards []cardDTO `json:"cards"`
	// BandsTZ echoes the zone the day-derived urgency bands were computed in.
	// Always present, and "UTC" is a DECLARED answer rather than a silent
	// fallback: without the echo a client cannot tell whether due-today was
	// computed in its own day or somebody else's.
	BandsTZ    string            `json:"bands_tz"`
	Partial    bool              `json:"partial"`
	PartErrors map[string]string `json:"part_errors,omitempty"`
	KnownGaps  map[string]string `json:"known_gaps,omitempty"`
	// CardErrors names WHY each UNREAD card could not be read, keyed by card
	// kind (C1 slice B). Its own map, beside part_errors and never inside it:
	// part_errors feeds `partial`, so a card read recorded there would degrade
	// the whole page for every consumer that has read that field since WS-5b on
	// the strength of one card failing. Additive, so a client that ignores it
	// loses nothing. Present only for unread cards: a live card has nothing to
	// explain and an absent one is already named in known_gaps.
	CardErrors map[string]string `json:"card_errors,omitempty"`
	// CardContext carries what a card needs to SAY and the envelope has no
	// field for, keyed by card_id (C4). Its own map beside card_errors, and for
	// the same reason: rank key section 2 says nothing in the card envelope is
	// optional, so adding omitempty fields for one card's sentence would weaken
	// the contract every card is read against. Present only for the cards that
	// have something to add, and a renderer that does not know a key ignores it.
	CardContext map[string]map[string]string `json:"card_context,omitempty"`
}

// instructorCourse is the FE InstructorCourseSummary, filled from
// chora-delivery GET /api/v1/instructors/{instructor_gcid}/courses.
//
// Only courseId / title / studentsEnrolled have a downstream source. The other
// four ride at their zero value (NEVER fabricated) and are declared per-field
// in known_gaps, mirroring how learnerCourseDTO handles the same situation:
// courseCode is not on the course aggregate, atomsAuthored belongs to
// chora-creation, and averageScorePct / pendingReviews both need the
// cross-assessment grading rollup that does not exist yet (every count today is
// a browser-side filter over ONE assessment's submissions). isLive needs a
// today-scoped offering query.
type instructorCourse struct {
	CourseID         string  `json:"courseId"`
	CourseCode       string  `json:"courseCode"`
	Title            string  `json:"title"`
	StudentsEnrolled int     `json:"studentsEnrolled"`
	AtomsAuthored    int     `json:"atomsAuthored"`
	AverageScorePct  float64 `json:"averageScorePct"`
	PendingReviews   int     `json:"pendingReviews"`
	IsLive           bool    `json:"isLive"`
}

// gcidPillLabel is the synthesised UI label for the A+ dashboard GCID
// pill. It is NOT domain data — it mirrors the FE wave-3 fixture string
// so the rendered pill is identical pre/post BFF swap.
const gcidPillLabel = "ONE IDENTITY · GCID active"

// -----------------------------------------------------------------------------
// GetDashboard — GET /api/me/dashboard
// -----------------------------------------------------------------------------

// GetDashboard fans out to chora-identity (/me) + chora-consumption
// (/v1/me/streak + /v1/me/learning-paths + /v1/me/companions) + for an
// instructor session chora-delivery (/api/v1/instructors/{gcid}/courses) IN
// PARALLEL, composes the FE DashboardSummary DTO, and degrades
// gracefully on a partial downstream failure (a failed part → that part
// omitted/empty + recorded in part_errors; partial=true). It NEVER
// returns a 5xx — the dashboard is the A+ landing surface and must
// always render something.
//
// WS-5b additions (2026-05-26): companions[] fan-out + nested streak{}
// block (current_streak_days + last_completion_at). LearnerCourses[].
// retention_state is OMITTED from the wire today because no
// chora-consumption retention endpoint exists yet (WS-5c-BE-A1 follow-
// up). Per feedback_no_stubs_real_wiring the field is NEVER fabricated
// to a sentinel value — the FE renders the explicit `unknown` dot from
// its own fallback when the field is absent.
func (a *Aggregator) GetDashboard(ctx context.Context, auth AuthCtx) (Response, error) {
	// instructorCourses is fetched only for a session that could own courses.
	// A learner-only session skips the call entirely: an empty array is then a
	// FACT (they instruct nothing), not a gap and not a failure.
	wantInstructor := instructsSomething(auth) && a.cfg.DeliveryURL != ""

	var (
		wg       sync.WaitGroup
		meCR     callResult
		streakCR callResult
		pathsCR  callResult
		famCR    callResult
		instrCR  callResult
		// C1 slice B: the three learner read models behind the ranked home.
		// Fanned out beside the rest rather than fetched after them, because a
		// serial chain would add three round-trips to the A+ landing surface.
		pendingCR    callResult
		transcriptCR callResult
		budgetCR     callResult
		// C4: the Atlas read behind the defend_hex card.
		mapsCR callResult
	)
	wg.Add(8)
	go func() {
		defer wg.Done()
		pendingCR = a.call(ctx,
			a.cfg.ConsumptionURL+"/v1/me/growth-edges/uploads?status=awaiting_review&limit=20", auth)
	}()
	go func() {
		defer wg.Done()
		transcriptCR = a.call(ctx, a.cfg.ConsumptionURL+"/v1/me/transcript", auth)
	}()
	go func() {
		defer wg.Done()
		budgetCR = a.call(ctx, a.cfg.ConsumptionURL+"/v1/me/practice-budget", auth)
	}()
	go func() {
		defer wg.Done()
		mapsCR = a.call(ctx, a.cfg.ConsumptionURL+"/v1/me/maps", auth)
	}()
	if wantInstructor {
		wg.Add(1)
		go func() {
			defer wg.Done()
			instrCR = a.call(ctx,
				a.cfg.DeliveryURL+"/api/v1/instructors/"+url.PathEscape(auth.GCID)+"/courses",
				auth)
		}()
	}
	go func() {
		defer wg.Done()
		meCR = a.call(ctx, a.cfg.IdentityURL+"/me", auth)
	}()
	go func() {
		defer wg.Done()
		streakCR = a.call(ctx, a.cfg.ConsumptionURL+"/v1/me/streak", auth)
	}()
	go func() {
		defer wg.Done()
		pathsCR = a.call(ctx, a.cfg.ConsumptionURL+"/v1/me/learning-paths", auth)
	}()
	go func() {
		defer wg.Done()
		famCR = a.call(ctx, a.cfg.ConsumptionURL+"/v1/me/companions", auth)
	}()
	wg.Wait()

	dto := dashboardDTO{
		GcidPillLabel: gcidPillLabel,
		// Non-null array invariant — the FE interface types both course
		// lists as non-null readonly arrays. companions[] follows the
		// same rule per WS-5b.
		LearnerCourses:    []learnerCourseDTO{},
		InstructorCourses: []instructorCourse{},
		Companions:        []companionRosterDTO{},
		PartErrors:        map[string]string{},
	}

	// userDisplayName ← chora-identity /me
	if meCR.ok() {
		var me identityMe
		if err := json.Unmarshal(meCR.body, &me); err == nil {
			dto.UserDisplayName = me.DisplayName
		} else {
			dto.PartErrors["userDisplayName"] = "decode_failed"
		}
	} else {
		dto.PartErrors["userDisplayName"] = downstreamErrCode(meCR)
	}

	// currentStreakDays (flat, legacy) + streak{} (WS-5b structured)
	// ← chora-consumption /v1/me/streak. The flat scalar mirrors the
	// streak.current_streak_days field so pre-WS-5 FE consumers keep
	// rendering correctly while the new FE consumers prefer the nested
	// block. A failed/decoded-failed call omits the nested block (NEVER
	// fabricate) but the flat scalar stays 0 (its zero-value).
	if streakCR.ok() {
		var s consumptionStreak
		if err := json.Unmarshal(streakCR.body, &s); err == nil {
			dto.CurrentStreakDays = s.Count
			dto.Streak = &streakSummaryDTO{
				CurrentStreakDays: s.Count,
				LastCompletionAt:  s.LastActivityAt,
			}
		} else {
			dto.PartErrors["currentStreakDays"] = "decode_failed"
		}
	} else {
		dto.PartErrors["currentStreakDays"] = downstreamErrCode(streakCR)
	}

	// learnerCourses[] ← chora-consumption /v1/me/learning-paths
	//
	// ADR-233 D2 — POSITIVE provenance filter (CHO-2217). Only a
	// course-provenanced path is a course. Until c56292633 the wire carried no
	// provenance and EVERY path was mapped here, so a converted collection (a
	// WS-4 study list) reached A+ as a *course* with an empty courseId and
	// rendered in "Continue learning".
	//
	// 🔴 NEVER rewrite this as `!= "course"`. Migration 0093 declares
	// source_type NOT NULL DEFAULT 'ad_hoc' and its backfill stamps 'course'
	// ONLY where course_id IS NOT NULL, so all three CHECK values are live on
	// this wire — the negative form sweeps every legacy ad_hoc path in as a
	// study list. A study list is == "collection", never != "course". Guarded
	// by TestGetDashboard_LearnerCoursesFilterIsPositive_ADR233_D2.
	if pathsCR.ok() {
		var lp consumptionLearningPaths
		if err := json.Unmarshal(pathsCR.body, &lp); err == nil {
			unclassified := 0
			for _, it := range lp.Items {
				switch it.SourceType {
				case consumptionSourceTypeCourse:
					// A course. Falls through to the append below.
				case "":
					// The producer projected NO source_type — a stale
					// (pre-c56292633) chora-consumption build. UNCLASSIFIABLE:
					// not 'ad_hoc' (the column is NOT NULL, so a current build
					// cannot emit empty) and not 'course'. Calling it a course
					// would fabricate provenance the wire never carried and
					// would silently re-admit the defect above; it.CourseID
					// cannot stand in, being the delivery binding rather than
					// provenance. Excluded, and recorded below so an emptied
					// Continue-learning states its reason instead of reading as
					// "this learner has no courses". Deploying the producer
					// first — the correct order — never reaches this branch.
					unclassified++
					continue
				default:
					// "collection" (a WS-4 study list) or "ad_hoc"
					// (hand-rolled). Neither is a course. Normal operation, NOT
					// a degradation — no part_errors entry.
					continue
				}
				remaining := it.TotalAtoms - it.CurrentIndex
				if remaining < 0 {
					remaining = 0
				}
				dto.LearnerCourses = append(dto.LearnerCourses, learnerCourseDTO{
					CourseID:       it.CourseID,
					Title:          it.Title,
					ProgressPct:    it.ProgressPercent,
					RemainingAtoms: remaining,
					// courseCode / instructorName / xpEarned / memoryPct /
					// dayNumber / dayTotal — no downstream source (GAP); left
					// at zero values, NOT fabricated.
					//
					// retention_state — no downstream source today (WS-5c-BE-A1
					// follow-up). The field is left as the empty string so the
					// `omitempty` JSON tag OMITS it from the wire (per
					// feedback_no_stubs_real_wiring — NEVER fabricate
					// "unknown"; the FE renders its own `unknown` dot when
					// absent).
				})
			}
			// Fail LOUD on unclassifiable paths. This part IS degraded — the
			// upstream answered, but with a payload this build cannot classify,
			// so learnerCourses is incomplete. part_errors (not known_gaps): a
			// stale producer is transient + recoverable, not a permanent
			// structural gap, and it SHOULD flip `partial`.
			if unclassified > 0 {
				dto.PartErrors["learnerCourses"] = "unclassified_source_type: upstream projected no ADR-233 provenance on " +
					strconv.Itoa(unclassified) + " path(s) — stale chora-consumption build (deploy the producer first)"
			}
		} else {
			dto.PartErrors["learnerCourses"] = "decode_failed"
		}
	} else {
		dto.PartErrors["learnerCourses"] = downstreamErrCode(pathsCR)
	}

	// companions[] (WS-5b) ← chora-consumption /v1/me/companions. Projects
	// each instanceResp into the FE CompanionRosterItem shape:
	//   companion_id     ← instance.companion_id
	//   name            ← instance.name
	//   species         ← growth_state.current_breed (the REVEALED breed —
	//                     owl/fox/dragon/... — the FE binds species to pick
	//                     the breed's FontAwesome icon), FALLING BACK to
	//                     instance.specialization when the breed is not yet
	//                     revealed (current_breed empty / growth absent)
	//   subject         ← instance.specialization (the SUBJECT axis, always)
	//   evolution_level ← growth_state.stage (0 when growth absent)
	//   stage_label     ← growth_state.stage_name (empty when growth absent)
	if famCR.ok() {
		var roster consumptionCompanionRoster
		if err := json.Unmarshal(famCR.body, &roster); err == nil {
			for _, it := range roster.Items {
				// species is the revealed breed; fall back to the
				// specialization (subject) only while the breed is unrevealed.
				species := it.Specialization
				if it.GrowthState != nil && it.GrowthState.CurrentBreed != "" {
					species = it.GrowthState.CurrentBreed
				}
				item := companionRosterDTO{
					CompanionID: it.CompanionID,
					Name:        it.Name,
					Species:     species,
					Subject:     it.Specialization,
				}
				if it.GrowthState != nil {
					item.EvolutionLevel = it.GrowthState.Stage
					item.StageLabel = it.GrowthState.StageName
				}
				dto.Companions = append(dto.Companions, item)
			}
		} else {
			dto.PartErrors["companions"] = "decode_failed"
		}
	} else {
		dto.PartErrors["companions"] = downstreamErrCode(famCR)
	}

	// instructorCourses[] ← chora-delivery
	// GET /api/v1/instructors/{instructor_gcid}/courses, addressed by the
	// CALLER'S OWN gcid. Three states, kept distinguishable on the wire:
	//
	//   fetched   → the projection below, part_errors on a failed call.
	//   skipped   → the session holds no instructor/admin role. The empty
	//               array is a FACT (they instruct nothing), so there is no
	//               gap and no error.
	//   unwired   → SVC_DELIVERY_URL unset. A CONFIG gap, declared in
	//               known_gaps so it never reads as "this instructor teaches
	//               nothing", and never flips partial.
	if wantInstructor {
		if instrCR.ok() {
			var courses deliveryInstructorCourses
			if err := json.Unmarshal(instrCR.body, &courses); err == nil {
				for _, c := range courses.Items {
					dto.InstructorCourses = append(dto.InstructorCourses, instructorCourse{
						CourseID:         c.ID,
						Title:            c.Title,
						StudentsEnrolled: c.EnrolledCount,
					})
				}
			} else {
				dto.PartErrors["instructorCourses"] = "decode_failed"
			}
		} else {
			dto.PartErrors["instructorCourses"] = downstreamErrCode(instrCR)
		}
	}

	// C1 slice B card reads are NOT recorded in part_errors, and that is a
	// decision rather than an omission.
	//
	// part_errors and `partial` are the PAGE-level mechanism for parts that have
	// no per-item state of their own. A card carries its own `state`, per kind,
	// in the same response: a failed read is already reported as `unread` where
	// the reader is looking. Recording it again here would double-report it, and
	// because `partial` is derived from part_errors it would flip the page into
	// a degraded state for every consumer that has read that field since WS-5b,
	// on the strength of one card failing.
	//
	// ⚠ What this costs, said out loud rather than glossed: the card says UNREAD
	// but not WHY, so 503 (unwired), 500 (repository) and 400 (this aggregator
	// built the call wrong) are indistinguishable on the wire. The rank key's
	// 7.1 table calls the 400 case a wiring bug, which is exactly the one an
	// operator would want named. Raised with the orchestrator rather than
	// settled here, because the answer changes the card envelope subagent1 is
	// building against.

	// `partial` reflects ONLY transient failures of WIREABLE parts: a clean
	// fan-out of every wireable part is partial=false even when known_gaps is
	// non-empty (a structural gap is not a failure).
	dto.Partial = len(dto.PartErrors) > 0
	if len(dto.PartErrors) == 0 {
		dto.PartErrors = nil
	}

	// known_gaps carries the parts with NO downstream source at all, so the
	// caller can tell a permanent structural gap from a transient per-request
	// failure. It never flips `partial`.
	//
	//   learnerCourses[].retention_state: no chora-consumption retention
	//     envelope endpoint exists yet (WS-5c-BE-A1 follow-up). The aggregator
	//     OMITS the field per omitempty (graceful MISSING).
	//   instructorCourses[].<field>: chora-delivery serves the course itself
	//     but not these four. They ride at their zero value and are declared
	//     here rather than dressed up.
	dto.KnownGaps = map[string]string{
		"learnerCourses.retention_state":    "no_downstream_source: chora-consumption retention envelope endpoint not yet wired (WS-5c-BE-A1)",
		"instructorCourses.courseCode":      "no_downstream_source: the chora-delivery course aggregate carries no course code",
		"instructorCourses.atomsAuthored":   "no_downstream_source: authored-atom counts live in chora-creation, not the delivery course",
		"instructorCourses.averageScorePct": "no_downstream_source: needs the cross-assessment grading rollup (today every count is a browser-side filter over one assessment)",
		"instructorCourses.pendingReviews":  "no_downstream_source: needs the cross-assessment grading rollup (today every count is a browser-side filter over one assessment)",
		"instructorCourses.isLive":          "no_downstream_source: needs a today-scoped offering query in chora-delivery",
	}
	if a.cfg.DeliveryURL == "" {
		dto.KnownGaps["instructorCourses"] = "not_configured: SVC_DELIVERY_URL unset, the by-instructor course list was not attempted"
	}

	// The ranked home (C1). Composed LAST, from the parts above, so a card can
	// only ever claim what the fan-out actually returned. The zone is resolved
	// and echoed here rather than inside composeCards, because the echo is the
	// contract even when no card ends up depending on a day band.
	loc, zoneName := resolveBandsZone(auth.TimeZone)
	dto.BandsTZ = zoneName
	reads := learnerReads{
		consumptionWired: a.cfg.ConsumptionURL != "",
		pending:          pendingCR,
		transcript:       transcriptCR,
		budget:           budgetCR,
		maps:             mapsCR,
		// The hand-off card is gated on the ROLE, not on the call: a session
		// that holds no instructor role gets no card at all, rather than an
		// absent one telling them about work they do not do.
		instructorRoleHeld: instructsSomething(auth),
		deliveryWired:      a.cfg.DeliveryURL != "",
		instructor:         instrCR,
	}
	dto.Cards, dto.CardContext = composeCards(&dto, time.Now(), loc, reads)
	dto.CardErrors = cardErrors(reads)
	for kind, reason := range cardKnownGaps() {
		if dto.KnownGaps == nil {
			dto.KnownGaps = map[string]string{}
		}
		dto.KnownGaps[kind] = reason
	}

	body, err := json.Marshal(dto)
	if err != nil {
		// Defensive — a marshal failure of our own struct should be
		// impossible, but never 5xx the dashboard: emit a minimal valid DTO.
		return Response{
			Status: http.StatusOK,
			Body:   []byte(`{"gcidPillLabel":"` + gcidPillLabel + `","userDisplayName":"","currentStreakDays":0,"learnerCourses":[],"instructorCourses":[],"companions":[],"cards":[],"bands_tz":"UTC","partial":true}`),
		}, nil
	}
	return Response{Status: http.StatusOK, Body: body}, nil
}

// downstreamErrCode maps a failed callResult to a compact part_errors
// reason string. Transport errors / timeouts → "upstream_unavailable";
// a downstream 4xx/5xx → the status-classed code.
func downstreamErrCode(cr callResult) string {
	if cr.err != nil {
		return "upstream_unavailable"
	}
	if cr.status >= 500 {
		return "upstream_5xx"
	}
	if cr.status >= 400 {
		return "upstream_4xx"
	}
	return "upstream_unexpected"
}
