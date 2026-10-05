package aggregate_test

import (
	"errors"
	"testing"

	"github.com/apollo-chora/chora-gateway/internal/domain/aggregate"
)

func TestNew_Valid(t *testing.T) {
	v, err := aggregate.New("aplus.home", "tenant-1", "gcid-1")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if v.View != "aplus.home" {
		t.Errorf("view = %q", v.View)
	}
	if v.IsPartial() {
		t.Error("freshly built view should not be partial")
	}
	if got := v.Status(); got != "ok" {
		t.Errorf("status = %q; want ok", got)
	}
}

func TestNew_RequiresView(t *testing.T) {
	if _, err := aggregate.New("  ", "t", "g"); err == nil {
		t.Fatal("expected error for empty view")
	}
}

func TestAddPart_Success(t *testing.T) {
	v, _ := aggregate.New("aplus.home", "t", "g")
	v.AddPart("learning_path", map[string]any{"id": "p1"})
	v.AddPart("recent_atoms", []map[string]any{{"id": "a1"}})
	if got := len(v.Parts); got != 2 {
		t.Errorf("parts len = %d; want 2", got)
	}
	if v.IsPartial() {
		t.Error("expected not partial")
	}
}

func TestAddPart_IgnoresEmptyKey(t *testing.T) {
	v, _ := aggregate.New("aplus.home", "t", "g")
	v.AddPart("  ", map[string]any{"x": 1})
	if got := len(v.Parts); got != 0 {
		t.Errorf("parts len = %d; want 0", got)
	}
}

func TestAddPartError_GracefulDegradation(t *testing.T) {
	v, _ := aggregate.New("aplus.home", "t", "g")
	v.AddPart("learning_path", map[string]any{"id": "p1"})
	v.AddPartError("recent_atoms", errors.New("upstream 500"))
	if !v.IsPartial() {
		t.Error("expected partial=true")
	}
	if got := v.Status(); got != "partial" {
		t.Errorf("status = %q; want partial", got)
	}
	if got := v.PartErrors["recent_atoms"]; got != "upstream 500" {
		t.Errorf("error msg = %q", got)
	}
	// Successful part remains visible.
	if _, ok := v.Parts["learning_path"]; !ok {
		t.Error("expected successful part to remain after error in another")
	}
}

func TestAddPartError_IgnoresNilOrEmpty(t *testing.T) {
	v, _ := aggregate.New("aplus.home", "t", "g")
	v.AddPartError("", errors.New("x"))
	v.AddPartError("k", nil)
	if v.IsPartial() {
		t.Error("expected not partial")
	}
}
