// companionbridge_helpers_whitebox_test.go — white-box residual coverage for
// the unexported body-reshaping helpers (wrapInData / ensureErrorEnvelope /
// renameKey / catalogFixup / snakeToCamel branch tails) that the black-box
// suite can only reach through 4xx + parsing edge paths.
package companionbridge

import (
	"testing"
)

func TestWrapInData_Branches(t *testing.T) {
	// Empty input → {"data":null}.
	if got := wrapInData(nil); string(got) != `{"data":null}` {
		t.Errorf("wrapInData(nil) = %s", got)
	}
	// Non-JSON → nil.
	if got := wrapInData([]byte(`not-json`)); got != nil {
		t.Errorf("wrapInData(not-json) = %s; want nil", got)
	}
	// Valid JSON value wraps.
	if got := wrapInData([]byte(`{"a":1}`)); string(got) != `{"data":{"a":1}}` {
		t.Errorf("wrapInData(json) = %s", got)
	}
}

func TestEnsureErrorEnvelope_Branches(t *testing.T) {
	// Empty → unchanged.
	if got := ensureErrorEnvelope(nil); got != nil {
		t.Errorf("ensureErrorEnvelope(nil) = %s", got)
	}
	// Non-JSON → unchanged.
	in := []byte(`nope`)
	if got := ensureErrorEnvelope(in); string(got) != "nope" {
		t.Errorf("ensureErrorEnvelope(non-json) = %s", got)
	}
	// Already wrapped {error:...} → unchanged.
	wrapped := []byte(`{"error":{"code":"X"}}`)
	if got := ensureErrorEnvelope(wrapped); string(got) != `{"error":{"code":"X"}}` {
		t.Errorf("ensureErrorEnvelope(wrapped) = %s", got)
	}
	// No code+message keys → unchanged.
	if got := ensureErrorEnvelope([]byte(`{"foo":1}`)); string(got) != `{"foo":1}` {
		t.Errorf("ensureErrorEnvelope(no code) = %s", got)
	}
	// Raw {code,message} → wrapped.
	got := ensureErrorEnvelope([]byte(`{"code":"C","message":"M"}`))
	if string(got) != `{"error":{"code":"C","message":"M"}}` {
		t.Errorf("ensureErrorEnvelope(raw) = %s", got)
	}
}

func TestRenameKey_Branches(t *testing.T) {
	fix := renameKey("items", "skus")
	// Absent source → unchanged.
	if got := fix([]byte(`{"total":1}`)); string(got) != `{"total":1}` {
		t.Errorf("renameKey(absent) = %s", got)
	}
	// Present, no collision → renamed.
	got := fix([]byte(`{"items":[1,2]}`))
	if string(got) != `{"skus":[1,2]}` {
		t.Errorf("renameKey(present) = %s", got)
	}
	// Collision → source retained.
	if got := fix([]byte(`{"items":[1],"skus":[9]}`)); string(got) != `{"items":[1],"skus":[9]}` {
		t.Errorf("renameKey(collision) = %s", got)
	}
	// Non-JSON → unchanged.
	if got := fix([]byte(`zzz`)); string(got) != "zzz" {
		t.Errorf("renameKey(non-json) = %s", got)
	}
}

func TestCatalogFixup_Branches(t *testing.T) {
	// No items key → unchanged.
	if got := catalogFixup([]byte(`{"total":2}`)); string(got) != `{"total":2}` {
		t.Errorf("catalogFixup(no items) = %s", got)
	}
	// items not an array → unchanged.
	if got := catalogFixup([]byte(`{"items":5}`)); string(got) != `{"items":5}` {
		t.Errorf("catalogFixup(items non-array) = %s", got)
	}
	// Row not an object → skipped.
	if got := catalogFixup([]byte(`{"items":[7]}`)); string(got) != `{"skus":[7]}` {
		t.Errorf("catalogFixup(row non-object) = %s", got)
	}
	// Non-JSON → unchanged.
	if got := catalogFixup([]byte(`nope`)); string(got) != "nope" {
		t.Errorf("catalogFixup(non-json) = %s", got)
	}
}

func TestSnakeToCamel_BranchTails(t *testing.T) {
	// Empty string.
	if got := snakeToCamel(""); got != "" {
		t.Errorf("snakeToCamel('') = %q", got)
	}
	// No underscore → unchanged.
	if got := snakeToCamel("camelCase"); got != "camelCase" {
		t.Errorf("snakeToCamel(no underscore) = %q", got)
	}
	// Leading underscore retained.
	if got := snakeToCamel("_leading"); got != "_leading" {
		t.Errorf("snakeToCamel(leading _) = %q", got)
	}
	// Normal conversion.
	if got := snakeToCamel("companion_id"); got != "companionId" {
		t.Errorf("snakeToCamel(companion_id) = %q", got)
	}
}
