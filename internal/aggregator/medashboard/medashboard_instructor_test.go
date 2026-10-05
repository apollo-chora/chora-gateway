// medashboard_instructor_test.go: RED first for closing the instructorCourses
// stub (UX Track U package B5; master plan section 4.2 and the home-signals
// register).
//
// The aggregator hardcoded `InstructorCourses: []instructorCourse{}` and
// published a known_gaps string claiming chora-delivery exposes no
// by-instructor course list. That claim stopped being true when
// chora-delivery shipped GET /api/v1/instructors/{instructor_gcid}/courses:
// the gap is a stale declaration, and it dark-fills an entire identity's lane
// on the A+ home.
package medashboard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/medashboard"
)

// instructorCoursesBody mirrors chora-delivery's publicCourseDTO envelope
// (handlers.go publicCourseDTO + the instructor-roster handler's
// {items,total,page,per} wrapper).
const instructorCoursesBody = `{"items":[` +
	`{"id":"course-csm-prep","tenant_id":"01970000-0000-7000-8000-0000000000bb","title":"CSM Prep","instructor_gcid":"01970000-0000-7000-8000-0000000000aa","instructor_name":"Phyllis Tan","enrolled_count":24,"public":true,"is_free":false,"tags":[]},` +
	`{"id":"course-bio-lab","tenant_id":"01970000-0000-7000-8000-0000000000bb","title":"Bio Lab","instructor_gcid":"01970000-0000-7000-8000-0000000000aa","instructor_name":"Phyllis Tan","enrolled_count":12,"public":false,"is_free":true,"tags":[]}` +
	`],"total":2,"page":1,"per":20}`

// instructorAuth is a session that actually holds the instructor role. Roles
// come from the VALIDATED session, never a client header.
func instructorAuth() medashboard.AuthCtx {
	a := sampleAuth()
	a.Roles = []string{"learner", "instructor"}
	return a
}

// healthyConsumption is the two-part consumption stub every case here reuses,
// so a delivery assertion never fails for an unrelated reason.
func healthyConsumption(t *testing.T) *httptest.Server {
	t.Helper()
	return routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/v1/me/streak":         {http.StatusOK, streakBody},
		"/v1/me/learning-paths": {http.StatusOK, learningPathsBody},
		"/v1/me/companions":     {http.StatusOK, `{"items":[]}`},
	})
}

func healthyIdentity(t *testing.T) *httptest.Server {
	t.Helper()
	return routingStub(t, map[string]struct {
		status int
		body   string
	}{
		"/me": {http.StatusOK, meBody},
	})
}

func decodeDTO(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var dto map[string]any
	if err := json.Unmarshal(body, &dto); err != nil {
		t.Fatalf("body not JSON: %v; body=%s", err, body)
	}
	return dto
}

// The lane lights: an instructor's courses arrive from the live delivery
// endpoint, addressed by the caller's OWN gcid.
func TestGetDashboard_InstructorCoursesComeFromDelivery(t *testing.T) {
	var gotPath string
	delivery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(instructorCoursesBody))
	}))
	t.Cleanup(delivery.Close)

	a := medashboard.New(medashboard.Config{
		IdentityURL:    healthyIdentity(t).URL,
		ConsumptionURL: healthyConsumption(t).URL,
		DeliveryURL:    delivery.URL,
		PerCallTimeout: time.Second,
	})
	resp, err := a.GetDashboard(context.Background(), instructorAuth())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	dto := decodeDTO(t, resp.Body)

	want := "/api/v1/instructors/" + instructorAuth().GCID + "/courses"
	if gotPath != want {
		t.Errorf("delivery path = %q; want %q (the caller's OWN gcid, never a client-supplied one)", gotPath, want)
	}

	ic, ok := dto["instructorCourses"].([]any)
	if !ok {
		t.Fatalf("instructorCourses not an array: %T", dto["instructorCourses"])
	}
	if len(ic) != 2 {
		t.Fatalf("instructorCourses len = %d; want 2", len(ic))
	}
	c0, _ := ic[0].(map[string]any)
	if c0["courseId"] != "course-csm-prep" {
		t.Errorf("instructorCourses[0].courseId = %v; want course-csm-prep", c0["courseId"])
	}
	if c0["title"] != "CSM Prep" {
		t.Errorf("instructorCourses[0].title = %v; want CSM Prep", c0["title"])
	}
	if got, _ := c0["studentsEnrolled"].(float64); got != 24 {
		t.Errorf("instructorCourses[0].studentsEnrolled = %v; want 24", c0["studentsEnrolled"])
	}
	if p, _ := dto["partial"].(bool); p {
		t.Errorf("partial = %v; want false on a clean fan-out", p)
	}
}

// The stale gap string must be gone. Keeping it would tell every consumer that
// a live endpoint does not exist.
func TestGetDashboard_StaleInstructorCoursesGapIsRetired(t *testing.T) {
	delivery := newStub(t, http.StatusOK, instructorCoursesBody)
	a := medashboard.New(medashboard.Config{
		IdentityURL:    healthyIdentity(t).URL,
		ConsumptionURL: healthyConsumption(t).URL,
		DeliveryURL:    delivery.srv.URL,
		PerCallTimeout: time.Second,
	})
	resp, _ := a.GetDashboard(context.Background(), instructorAuth())
	dto := decodeDTO(t, resp.Body)

	gaps, _ := dto["known_gaps"].(map[string]any)
	if got, present := gaps["instructorCourses"]; present {
		t.Errorf("known_gaps[instructorCourses] = %v; want the entry GONE (the endpoint exists)", got)
	}
	// The unrelated retention gap is genuine and must survive the deletion.
	if _, present := gaps["learnerCourses.retention_state"]; !present {
		t.Error("known_gaps lost learnerCourses.retention_state; only the STALE claim was to be deleted")
	}
}

// A learner-only session instructs nothing. Skipping the call is the honest
// answer and it is NOT a gap: the array is empty because it is empty.
func TestGetDashboard_NonInstructorSkipsTheDeliveryCall(t *testing.T) {
	delivery := newStub(t, http.StatusOK, instructorCoursesBody)
	a := medashboard.New(medashboard.Config{
		IdentityURL:    healthyIdentity(t).URL,
		ConsumptionURL: healthyConsumption(t).URL,
		DeliveryURL:    delivery.srv.URL,
		PerCallTimeout: time.Second,
	})
	learner := sampleAuth()
	learner.Roles = []string{"learner"}
	resp, _ := a.GetDashboard(context.Background(), learner)
	dto := decodeDTO(t, resp.Body)

	if delivery.calls != 0 {
		t.Errorf("delivery called %d times for a learner-only session; want 0", delivery.calls)
	}
	ic, ok := dto["instructorCourses"].([]any)
	if !ok || len(ic) != 0 {
		t.Errorf("instructorCourses = %v; want an empty array", dto["instructorCourses"])
	}
	if pe, _ := dto["part_errors"].(map[string]any); pe["instructorCourses"] != nil {
		t.Errorf("part_errors[instructorCourses] = %v; want none (not instructing is not a failure)", pe["instructorCourses"])
	}
	if p, _ := dto["partial"].(bool); p {
		t.Errorf("partial = %v; want false", p)
	}
}

// A delivery 5xx empties that card and says so. It never 500s the dashboard.
func TestGetDashboard_DeliveryUpstream5xx_DegradesGracefully(t *testing.T) {
	delivery := newStub(t, http.StatusBadGateway, `{"error":"boom"}`)
	a := medashboard.New(medashboard.Config{
		IdentityURL:    healthyIdentity(t).URL,
		ConsumptionURL: healthyConsumption(t).URL,
		DeliveryURL:    delivery.srv.URL,
		PerCallTimeout: time.Second,
	})
	resp, _ := a.GetDashboard(context.Background(), instructorAuth())
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200 even when delivery is down", resp.Status)
	}
	dto := decodeDTO(t, resp.Body)
	if ic, ok := dto["instructorCourses"].([]any); !ok || len(ic) != 0 {
		t.Errorf("instructorCourses = %v; want an empty array, never null and never fabricated", dto["instructorCourses"])
	}
	pe, _ := dto["part_errors"].(map[string]any)
	if reason, _ := pe["instructorCourses"].(string); !strings.HasPrefix(reason, "upstream_") {
		t.Errorf("part_errors[instructorCourses] = %q; want an upstream_* reason", reason)
	}
	if p, _ := dto["partial"].(bool); !p {
		t.Error("partial = false; a failed WIREABLE part must flip it")
	}
}

// An unset SVC_DELIVERY_URL is a CONFIG gap, not a transient failure: it is
// declared in known_gaps and must not flip partial, or every unconfigured
// environment would report itself as degraded on every request.
func TestGetDashboard_DeliveryURLUnset_IsAKnownGapNotAFailure(t *testing.T) {
	a := medashboard.New(medashboard.Config{
		IdentityURL:    healthyIdentity(t).URL,
		ConsumptionURL: healthyConsumption(t).URL,
		PerCallTimeout: time.Second,
	})
	resp, _ := a.GetDashboard(context.Background(), instructorAuth())
	dto := decodeDTO(t, resp.Body)

	gaps, _ := dto["known_gaps"].(map[string]any)
	reason, _ := gaps["instructorCourses"].(string)
	if !strings.Contains(reason, "not_configured") {
		t.Errorf("known_gaps[instructorCourses] = %q; want a not_configured reason naming the unset URL", reason)
	}
	if pe, _ := dto["part_errors"].(map[string]any); pe["instructorCourses"] != nil {
		t.Errorf("part_errors[instructorCourses] = %v; want none (config, not a per-request failure)", pe["instructorCourses"])
	}
	if p, _ := dto["partial"].(bool); p {
		t.Error("partial = true; an unset URL is a structural gap and must not flip it")
	}
}

// The residual per-field gaps are declared rather than dressed up: delivery
// serves no course code, authored-atom count, average score or pending-review
// rollup, so those fields ride at their zero value and say so.
func TestGetDashboard_InstructorCourseFieldGapsAreDeclared(t *testing.T) {
	delivery := newStub(t, http.StatusOK, instructorCoursesBody)
	a := medashboard.New(medashboard.Config{
		IdentityURL:    healthyIdentity(t).URL,
		ConsumptionURL: healthyConsumption(t).URL,
		DeliveryURL:    delivery.srv.URL,
		PerCallTimeout: time.Second,
	})
	resp, _ := a.GetDashboard(context.Background(), instructorAuth())
	dto := decodeDTO(t, resp.Body)

	gaps, _ := dto["known_gaps"].(map[string]any)
	for _, field := range []string{
		"instructorCourses.pendingReviews",
		"instructorCourses.averageScorePct",
	} {
		if _, present := gaps[field]; !present {
			t.Errorf("known_gaps missing %q; a zero-valued field with no source must be declared", field)
		}
	}
}

// LoadConfigFromEnv must read the delivery base from the SAME env var the rest
// of the gateway already uses; an inline URL is forbidden.
func TestLoadConfigFromEnv_ReadsDeliveryURL(t *testing.T) {
	t.Setenv("SVC_DELIVERY_URL", "http://chora-delivery:8080")
	if got := medashboard.LoadConfigFromEnv().DeliveryURL; got != "http://chora-delivery:8080" {
		t.Errorf("DeliveryURL = %q; want the SVC_DELIVERY_URL value", got)
	}
}
