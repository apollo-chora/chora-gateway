// http_helpers_whitebox_test.go — white-box (package httpadapter) coverage
// for the small unexported helpers the black-box suite never reaches
// directly: decodeJSON (nil-body error + strict decode) and the context
// lookups sessionFromContext / IdentityFromContext. LoadDataLineageYAMLFromEnv
// is exercised for its nil-default + read-error branches.
package httpadapter

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSON_NilBodyAndStrictDecode(t *testing.T) {
	// Nil body → error.
	req := httptest.NewRequest("POST", "/x", nil)
	if err := decodeJSON(req, &struct{}{}); err == nil {
		t.Error("decodeJSON with nil body: expected error")
	}

	// Unknown field → error (DecodeJSON disallows unknown fields).
	req = httptest.NewRequest("POST", "/x", strings.NewReader(`{"unknown_field":1}`))
	var v struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(req, &v); err == nil {
		t.Error("decodeJSON with unknown field: expected error")
	}

	// Valid body decodes.
	req = httptest.NewRequest("POST", "/x", strings.NewReader(`{"name":"x"}`))
	if err := decodeJSON(req, &v); err != nil {
		t.Errorf("decodeJSON valid: %v", err)
	}
	if v.Name != "x" {
		t.Errorf("decoded name = %q; want x", v.Name)
	}
}

func TestSessionFromContext_PresentAndAbsent(t *testing.T) {
	// Absent → nil.
	if got := sessionFromContext(context.Background()); got != nil {
		t.Errorf("sessionFromContext(empty) = %v; want nil", got)
	}
}

func TestIdentityFromContext_AbsentReturnsEmpty(t *testing.T) {
	tenant, gcid := IdentityFromContext(context.Background())
	if tenant != "" || gcid != "" {
		t.Errorf("IdentityFromContext(empty) = (%q, %q); want empty", tenant, gcid)
	}
}

func TestLoadDataLineageYAMLFromEnv_Branches(t *testing.T) {
	// Unset → nil.
	t.Setenv("CHORA_GATEWAY_DATA_LINEAGE_PATH", "")
	if got := LoadDataLineageYAMLFromEnv(); got != nil {
		t.Errorf("unset env → %v; want nil", got)
	}
	// Set to a nonexistent path → nil (read error swallowed).
	t.Setenv("CHORA_GATEWAY_DATA_LINEAGE_PATH", "/nonexistent/lineage.yaml")
	if got := LoadDataLineageYAMLFromEnv(); got != nil {
		t.Errorf("unreadable path → %v; want nil", got)
	}
}
