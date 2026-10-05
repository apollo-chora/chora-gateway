// gatewayproxy_question_jobs_deadline_test.go: the AI-Assist batch upload
// route must lift its own read/write deadline so a multi-MB multipart upload is
// not severed at the 15s server-wide timeout (the 504 that read as a
// forever-spinner). This asserts the POST /question-jobs route calls the
// deadline extension; the underlying middleware-unwrap wiring is covered in
// libs/chora-go-common/tracing, and the helper itself in
// libs/chora-go-common/http.
package httpadapter_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	httpadapter "github.com/apollo-chora/chora-gateway/internal/adapter/http"
)

// gwDeadlineWriter is a deadline-capable recorder: http.NewResponseController
// finds SetReadDeadline/SetWriteDeadline on it, so a route that extends its
// deadline records the values here.
type gwDeadlineWriter struct {
	*httptest.ResponseRecorder
	readDeadline  time.Time
	writeDeadline time.Time
}

func (d *gwDeadlineWriter) SetReadDeadline(t time.Time) error  { d.readDeadline = t; return nil }
func (d *gwDeadlineWriter) SetWriteDeadline(t time.Time) error { d.writeDeadline = t; return nil }

func TestGwProxy_QuestionJobsPost_ExtendsUploadDeadline(t *testing.T) {
	var gotBody string
	stub := newGwProxyStub(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":"j1","status":"requested"}`))
	})
	h := newGwProxyMux(t, stub)

	body := `{"settings":"{\"job_type\":\"batch_source_material\"}"}`
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/atoms/019ff65a-f5ea-7db9-b1e8-07d7794f32a3/question-jobs", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer testtoken")
	r.Header.Set("X-Tenant-Id", "tenant-001")
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(httpadapter.InjectMeshClaimsForTest(r.Context(), "gcid-001", "tenant-001"))

	before := time.Now()
	w := &gwDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202; body=%s", w.Code, w.Body.String())
	}
	if gotBody != body {
		t.Errorf("downstream body = %q; want the upload body forwarded verbatim", gotBody)
	}
	// The route must have lifted both deadlines well past the 15s default.
	floor := before.Add(60 * time.Second)
	if w.readDeadline.Before(floor) {
		t.Errorf("read deadline %v not extended past 60s of %v (route did not call ExtendRequestDeadlines)", w.readDeadline, before)
	}
	if w.writeDeadline.Before(floor) {
		t.Errorf("write deadline %v not extended past 60s of %v", w.writeDeadline, before)
	}
}
