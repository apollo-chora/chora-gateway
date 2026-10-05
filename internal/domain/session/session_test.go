package session_test

import (
	"strings"
	"testing"
	"time"

	"github.com/apollo-chora/chora-gateway/internal/domain/session"
)

func TestNew_Valid(t *testing.T) {
	s, err := session.New(session.NewParams{
		Gcid: "gcid-1", TenantID: "tenant-1",
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if s.Token == "" {
		t.Error("expected non-empty token")
	}
	if s.IsExpired() {
		t.Error("freshly minted session should not be expired")
	}
	if got := s.ExpiresAt.Sub(s.IssuedAt); got != 8*time.Hour {
		t.Errorf("default TTL = %v; want 8h", got)
	}
}

func TestNew_CustomTTL(t *testing.T) {
	s, err := session.New(session.NewParams{
		Gcid: "g", TenantID: "t", TTL: 30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got := s.ExpiresAt.Sub(s.IssuedAt); got != 30*time.Minute {
		t.Errorf("TTL = %v; want 30m", got)
	}
}

func TestNew_RequiresGcid(t *testing.T) {
	_, err := session.New(session.NewParams{Gcid: "  ", TenantID: "t"})
	if err == nil {
		t.Fatal("expected error for empty gcid")
	}
}

func TestNew_RequiresTenant(t *testing.T) {
	_, err := session.New(session.NewParams{Gcid: "g", TenantID: ""})
	if err == nil {
		t.Fatal("expected error for empty tenant")
	}
}

func TestIsExpired(t *testing.T) {
	s, _ := session.New(session.NewParams{Gcid: "g", TenantID: "t"})
	s.ExpiresAt = time.Now().UTC().Add(-time.Second)
	if !s.IsExpired() {
		t.Error("expected expired session")
	}
}

func TestHasAdmin(t *testing.T) {
	s, _ := session.New(session.NewParams{Gcid: "g", TenantID: "t", Roles: []string{"learner", "Admin"}})
	if !s.HasAdmin() {
		t.Error("expected HasAdmin=true (case-insensitive)")
	}
	s2, _ := session.New(session.NewParams{Gcid: "g", TenantID: "t"})
	if s2.HasAdmin() {
		t.Error("did not expect HasAdmin=true on empty roles")
	}
}

func TestValidateJWTShape(t *testing.T) {
	cases := map[string]bool{
		"a.b.c":              true,
		"header.payload.sig": true,
		"":                   false,
		"   ":                false,
		"only.two":           false,
		"four.parts.are.bad": false,
		".missing.first":     false,
		"missing..middle":    false,
		"missing.last.":      false,
	}
	for tok, ok := range cases {
		err := session.ValidateJWTShape(tok)
		if ok && err != nil {
			t.Errorf("ValidateJWTShape(%q) err=%v want nil", tok, err)
		}
		if !ok && err == nil {
			t.Errorf("ValidateJWTShape(%q) err=nil want non-nil", tok)
		}
	}
	// Sanity check a more realistic shape.
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signature"
	if err := session.ValidateJWTShape(jwt); err != nil {
		t.Errorf("realistic shape err=%v; want nil", err)
	}
	if !strings.Contains(jwt, ".") {
		t.Error("sanity")
	}
}
