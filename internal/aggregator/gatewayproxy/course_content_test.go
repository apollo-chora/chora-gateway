package gatewayproxy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/aggregator/gatewayproxy"
)

const meCourseContentPath = "/api/v1/me/courses/019e30db-692f-7d10-8ce0-59669fe9298d/content"

// curriculumWithGsVideo is a consumption projection body: one uploaded video
// (gs:// ref) + one atom (non-media).
const curriculumWithGsVideo = `{"course_id":"019e30db-692f-7d10-8ce0-59669fe9298d","items":[` +
	`{"item_id":"item-vid","kind":"video","ref":"gs://b/v.mp4","title":"Lecture","position":0},` +
	`{"item_id":"item-atom","kind":"atom","ref":"atom-uuid","title":"Intro","position":1}]}`

// parseMergedItems decodes the merged MeCourseContent body into item maps keyed
// by item_id.
func parseMergedItems(t *testing.T, body []byte) map[string]map[string]any {
	t.Helper()
	var doc struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode merged body: %v (%s)", err, string(body))
	}
	out := make(map[string]map[string]any, len(doc.Items))
	for _, it := range doc.Items {
		id, _ := it["item_id"].(string)
		out[id] = it
	}
	return out
}

// CHO-1612 — MeCourseContent proxies /api/v1/me/courses/{id}/content to
// chora-consumption with the /api prefix stripped (→ /v1/me/courses/...).
func TestMeCourseContent_ProxiesToConsumptionPathTranslated(t *testing.T) {
	cb := newCaptureBackend(t, http.StatusOK, `{"course_id":"c1","items":[]}`)
	a := gatewayproxy.New(gatewayproxy.Config{ConsumptionURL: cb.srv.URL, PerCallTimeout: time.Second})

	const inPath = "/api/v1/me/courses/019e30db-692f-7d10-8ce0-59669fe9298d/content"
	resp, err := a.MeCourseContent(context.Background(), sampleAuth(), inPath)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.Status)
	}
	wantPath := "/v1/me/courses/019e30db-692f-7d10-8ce0-59669fe9298d/content"
	if cb.path != wantPath {
		t.Errorf("downstream path = %q; want %q", cb.path, wantPath)
	}
	if cb.method != http.MethodGet {
		t.Errorf("method = %q; want GET", cb.method)
	}
}

// ADR-185 — when delivery is configured, MeCourseContent fans out in parallel to
// the chora-delivery media-urls endpoint and swaps gs://→signed `ref` by item_id.
func TestMeCourseContent_MergesSignedMediaFromDelivery(t *testing.T) {
	cons := newCaptureBackend(t, http.StatusOK, curriculumWithGsVideo)
	deliv := newCaptureBackend(t, http.StatusOK, `{"resolved":{"item-vid":"https://signed-get/v"},"expires_at":"2026-06-20T00:00:00Z"}`)
	a := gatewayproxy.New(gatewayproxy.Config{
		ConsumptionURL: cons.srv.URL,
		DeliveryURL:    deliv.srv.URL,
		PerCallTimeout: time.Second,
	})

	resp, err := a.MeCourseContent(context.Background(), sampleAuth(), meCourseContentPath)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200 (%s)", resp.Status, string(resp.Body))
	}
	// Delivery was called at the media-urls sub-path.
	wantDeliv := "/v1/me/courses/019e30db-692f-7d10-8ce0-59669fe9298d/content/media-urls"
	if deliv.path != wantDeliv {
		t.Errorf("delivery path = %q; want %q", deliv.path, wantDeliv)
	}
	items := parseMergedItems(t, resp.Body)
	vid := items["item-vid"]
	if vid["ref"] != "https://signed-get/v" {
		t.Errorf("video ref = %v; want the signed URL", vid["ref"])
	}
	if vid["object_ref"] != "gs://b/v.mp4" {
		t.Errorf("video object_ref = %v; want the durable gs:// URI", vid["object_ref"])
	}
	// The atom (non-media) is untouched.
	atom := items["item-atom"]
	if atom["ref"] != "atom-uuid" {
		t.Errorf("atom ref = %v; want untouched", atom["ref"])
	}
	if _, has := atom["object_ref"]; has {
		t.Errorf("atom must not gain object_ref")
	}
}

// On a delivery error/403, MeCourseContent leaves media refs RAW and still
// returns the curriculum 200 — fail-visible, never blank the whole thing.
func TestMeCourseContent_DeliveryErrorLeavesRefsRaw(t *testing.T) {
	cons := newCaptureBackend(t, http.StatusOK, curriculumWithGsVideo)
	deliv := newCaptureBackend(t, http.StatusForbidden, `{"error":{"code":"FORBIDDEN"}}`)
	a := gatewayproxy.New(gatewayproxy.Config{
		ConsumptionURL: cons.srv.URL,
		DeliveryURL:    deliv.srv.URL,
		PerCallTimeout: time.Second,
	})

	resp, err := a.MeCourseContent(context.Background(), sampleAuth(), meCourseContentPath)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d; want 200 (curriculum survives delivery failure)", resp.Status)
	}
	vid := parseMergedItems(t, resp.Body)["item-vid"]
	if vid["ref"] != "gs://b/v.mp4" {
		t.Errorf("video ref = %v; want the raw gs:// ref preserved", vid["ref"])
	}
	if _, has := vid["object_ref"]; has {
		t.Errorf("no object_ref when delivery failed")
	}
}

// When the curriculum read itself fails, the error is surfaced verbatim (the
// delivery media-urls result is irrelevant).
func TestMeCourseContent_ConsumptionErrorSurfaced(t *testing.T) {
	cons := newCaptureBackend(t, http.StatusNotFound, `{"error":{"code":"NOT_FOUND"}}`)
	deliv := newCaptureBackend(t, http.StatusOK, `{"resolved":{}}`)
	a := gatewayproxy.New(gatewayproxy.Config{
		ConsumptionURL: cons.srv.URL,
		DeliveryURL:    deliv.srv.URL,
		PerCallTimeout: time.Second,
	})

	resp, err := a.MeCourseContent(context.Background(), sampleAuth(), meCourseContentPath)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp.Status != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 surfaced from consumption", resp.Status)
	}
}
