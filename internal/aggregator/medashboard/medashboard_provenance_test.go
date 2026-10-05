// medashboard_provenance_test.go — ADR-233 D2 provenance specs for
// learnerCourses[] (CHO-2217 WP2, gateway half).
//
// THE DEFECT UNDER FIX
// --------------------
// GetDashboard mapped EVERY learning path chora-consumption returned into
// learnerCourses[] unfiltered, so a converted collection (a WS-4 study list)
// reached A+ as a *course* with an empty courseId and rendered in "Continue
// learning". The wire could not express the distinction until c56292633
// projected source_type/source_id onto mePathSummary; this is the consumer.
//
// 🔴 THE FILTER IS POSITIVE — `source_type == "course"`, NEVER `!= "course"`.
// migration 0093 declares `source_type TEXT NOT NULL DEFAULT 'ad_hoc'` with
// CHECK (source_type IN ('course','collection','ad_hoc')), and its backfill
// stamps 'course' ONLY where course_id IS NOT NULL. All three values are live
// on this wire, so the negative form sweeps every legacy ad_hoc path into the
// study-list bucket. A study list is `== "collection"`, never `!= "course"`.
// TestGetDashboard_LearnerCoursesFilterIsPositive_ADR233_D2 is the guard that
// fails on an inversion.
//
// Strict TDD: these specs were written and observed FAILING before the
// consumptionPathItem.SourceType field or the filter existed.
package medashboard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/medashboard"
)

// mixedProvenancePathsBody — one path of EACH live source_type, exactly the
// three values migration 0093's CHECK admits:
//
//	course     — delivery-bootstrapped (learning_path/bootstrap.go)  → a course
//	collection — a WS-4 study list     (learning_path/study_list.go) → NOT a course
//	ad_hoc     — hand-rolled           (learning_path/path.go)       → NOT a course
//
// The study list and the ad_hoc path both carry NO course_id — which is what
// makes the naive `course_id != ""` heuristic tempting, and wrong: it conflates
// ad_hoc with collection. ADR-233 D2 replaced that heuristic with real
// provenance precisely so the two are distinguishable.
const mixedProvenancePathsBody = `{"items":[` +
	`{"path_id":"path-course","course_id":"course-csm-prep","enrollment_id":"enr-1","source_type":"course","source_id":"course-csm-prep","title":"CSM Prep","atom_ids":["a1","a2","a3"],"current_index":1,"total_atoms":3,"progress_percent":33.3,"completed":false},` +
	`{"path_id":"path-study","source_type":"collection","source_id":"01970000-0000-7000-8000-0000000000cc","title":"My Study List","atom_ids":["b1","b2"],"current_index":0,"total_atoms":2,"progress_percent":0,"completed":false},` +
	`{"path_id":"path-adhoc","source_type":"ad_hoc","title":"Hand-rolled Path","atom_ids":["c1"],"current_index":0,"total_atoms":1,"progress_percent":0,"completed":false}` +
	`]}`

// legacyNoProvenancePathsBody — the PRE-c56292633 wire. An old chora-consumption
// build projects no source_type at all (mePathSummary tags it `omitempty`), so
// every path arrives unclassifiable. Note the item DOES carry a course_id: the
// tempting "it has a course_id, call it a course" inference is exactly the
// fabrication this spec forbids.
const legacyNoProvenancePathsBody = `{"items":[` +
	`{"path_id":"path-1","course_id":"course-csm-prep","enrollment_id":"enr-1","title":"CSM Prep","atom_ids":["a1","a2","a3"],"current_index":1,"total_atoms":3,"progress_percent":33.3,"completed":false}` +
	`]}`

// dashboardWithPaths runs a clean full fan-out with pathsBody standing in for
// chora-consumption GET /v1/me/learning-paths, and returns the decoded DTO.
func dashboardWithPaths(t *testing.T, pathsBody string) map[string]any {
	t.Helper()
	identity := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/me": {http.StatusOK, meBody},
	})
	consumption := routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/v1/me/streak":         {http.StatusOK, streakBody},
		"/v1/me/learning-paths": {http.StatusOK, pathsBody},
		"/v1/me/companions":     {http.StatusOK, `{"items":[]}`},
	})
	a := medashboard.New(medashboard.Config{
		IdentityURL:    identity.URL,
		ConsumptionURL: consumption.URL,
		PerCallTimeout: time.Second,
	})

	resp, err := a.GetDashboard(context.Background(), sampleAuth())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200 (the dashboard NEVER 5xxs)", resp.Status)
	}
	var dto map[string]any
	if err := json.Unmarshal(resp.Body, &dto); err != nil {
		t.Fatalf("response body not JSON: %v — body=%s", err, resp.Body)
	}
	return dto
}

// learnerCourseTitles projects learnerCourses[].title for assertions. Titles
// (not courseIds) because a study list HAS no courseId — the defect being that
// it rendered with an empty one.
func learnerCourseTitles(t *testing.T, dto map[string]any) []string {
	t.Helper()
	arr, ok := dto["learnerCourses"].([]any)
	if !ok {
		t.Fatalf("learnerCourses not an array: %T (must be [] — never null)", dto["learnerCourses"])
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		m, _ := e.(map[string]any)
		title, _ := m["title"].(string)
		out = append(out, title)
	}
	return out
}

// -----------------------------------------------------------------------------
// The positive-form filter
// -----------------------------------------------------------------------------

// 🔴 THE ANTI-INVERSION GUARD. Rewriting the filter as `!= "course"` — or
// bucketing "everything that is not a course" as a study list — fails here on
// the ad_hoc path.
func TestGetDashboard_LearnerCoursesFilterIsPositive_ADR233_D2(t *testing.T) {
	dto := dashboardWithPaths(t, mixedProvenancePathsBody)
	titles := learnerCourseTitles(t, dto)

	got := make(map[string]bool, len(titles))
	for _, ti := range titles {
		got[ti] = true
	}

	// POSITIVE FORM: source_type == "course" is the ONLY thing that is a course.
	if !got["CSM Prep"] {
		t.Errorf("learnerCourses = %v; want it to CONTAIN the source_type==\"course\" path (CSM Prep)", titles)
	}
	// A study list is source_type == "collection". This is the CHO-2217 defect.
	if got["My Study List"] {
		t.Errorf("learnerCourses = %v; a source_type==\"collection\" study list rendered as a COURSE — the CHO-2217 defect (it reaches A+ with an empty courseId)", titles)
	}
	// An ad_hoc path is NEITHER a course NOR a study list. It belongs to NO
	// bucket. `!= "course"` puts it in the study-list bucket; `!= "collection"`
	// puts it here. Only the positive form leaves it out of both.
	if got["Hand-rolled Path"] {
		t.Errorf("learnerCourses = %v; a source_type==\"ad_hoc\" path rendered as a COURSE — the filter is INVERTED. It MUST be the positive form == \"course\" (0093: NOT NULL DEFAULT 'ad_hoc', backfill stamps only 'course')", titles)
	}
	if len(titles) != 1 {
		t.Errorf("learnerCourses len = %d (%v); want exactly 1 — only the course-provenanced path", len(titles), titles)
	}

	// The surviving course keeps its real courseId (the delivery BINDING —
	// retained alongside provenance per 0093, not replaced by source_id).
	arr, _ := dto["learnerCourses"].([]any)
	if len(arr) == 1 {
		lc0, _ := arr[0].(map[string]any)
		if lc0["courseId"] != "course-csm-prep" {
			t.Errorf("learnerCourses[0].courseId = %v; want course-csm-prep", lc0["courseId"])
		}
	}

	// A clean fan-out carrying a study list is NOT a degraded fan-out — the
	// filter is normal operation, not a failure.
	if p, _ := dto["partial"].(bool); p {
		t.Errorf("partial = true; want false — filtering a study list out is normal operation, not degradation")
	}
}

// A learner whose ONLY paths are study lists sees an EMPTY Continue-learning,
// not a list of empty-courseId ghosts. Isolated from the mixed fixture so the
// assertion cannot pass because some other item happened to be filtered.
func TestGetDashboard_StudyListOnly_LearnerCoursesEmpty_ADR233_D2(t *testing.T) {
	body := `{"items":[{"path_id":"path-study","source_type":"collection","source_id":"01970000-0000-7000-8000-0000000000cc","title":"My Study List","atom_ids":["b1","b2"],"current_index":0,"total_atoms":2,"progress_percent":0,"completed":false}]}`
	titles := learnerCourseTitles(t, dashboardWithPaths(t, body))
	if len(titles) != 0 {
		t.Errorf("learnerCourses = %v; want [] — a study list is NEVER a course", titles)
	}
}

// An ad_hoc-only learner also sees an empty Continue-learning. Standalone twin
// of the guard above: with `!= "course"` this still yields [] for
// learnerCourses, so it is the MIXED fixture that catches the inversion — this
// one pins the ad_hoc→not-a-course half on its own.
func TestGetDashboard_AdHocOnly_LearnerCoursesEmpty_ADR233_D2(t *testing.T) {
	body := `{"items":[{"path_id":"path-adhoc","source_type":"ad_hoc","title":"Hand-rolled Path","atom_ids":["c1"],"current_index":0,"total_atoms":1,"progress_percent":0,"completed":false}]}`
	titles := learnerCourseTitles(t, dashboardWithPaths(t, body))
	if len(titles) != 0 {
		t.Errorf("learnerCourses = %v; want [] — an ad_hoc path has no course to continue", titles)
	}
}

// -----------------------------------------------------------------------------
// Absent source_type — the rollout case
// -----------------------------------------------------------------------------

// An ABSENT source_type means the PRODUCER could not classify the path (a
// pre-c56292633 chora-consumption build). It does NOT mean 'ad_hoc', and it
// does NOT mean 'course' — the new build cannot emit empty at all, because the
// column is NOT NULL DEFAULT 'ad_hoc'.
//
// DECISION: exclude it AND record it. Including it would fabricate provenance
// the wire never carried (feedback_no_stubs_real_wiring) and would silently
// re-admit the very defect under fix. Excluding it SILENTLY would empty the A+
// landing surface with no stated reason — so it goes in part_errors, which is
// this aggregator's existing channel for "this part is degraded, a transient
// and recoverable condition". The correct deploy order (producer first —
// consumption c56292633 ships before this gateway) never reaches this branch.
func TestGetDashboard_AbsentSourceType_ExcludedAndRecordedLoudly_ADR233_D2(t *testing.T) {
	dto := dashboardWithPaths(t, legacyNoProvenancePathsBody)

	titles := learnerCourseTitles(t, dto)
	if len(titles) != 0 {
		t.Errorf("learnerCourses = %v; want [] — an ABSENT source_type is UNCLASSIFIABLE. Rendering it as a course fabricates provenance the wire never carried (and a course_id is the delivery binding, NOT provenance)", titles)
	}

	// ...and it must be LOUD. A silently-empty dashboard is indistinguishable
	// from "this learner has no courses".
	pe, ok := dto["part_errors"].(map[string]any)
	if !ok {
		t.Fatalf("part_errors absent (%T); want a learnerCourses entry — an unclassifiable path must be RECORDED, never silently dropped", dto["part_errors"])
	}
	reason, _ := pe["learnerCourses"].(string)
	if reason == "" {
		t.Errorf("part_errors = %v; want a learnerCourses entry naming the unclassified-source_type condition", pe)
	}
	if p, _ := dto["partial"].(bool); !p {
		t.Errorf("partial = false; want true — learnerCourses is degraded when the producer cannot classify its paths")
	}
}
