// kiosk_seed_routes_test.go: the golden run's seed routes (T6.2 part 2) through the real
// kiosk edge chain. They exist only with CHORA_KIOSK_SEED_ROUTES=on (the Mac's compose for
// the golden run); unset they answer 503 like any route outside the allowlist, and any other
// value refuses the boot.
package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

var seedRoutes = []d8Route{
	{"create an atom", "POST", "/api/atoms", `{}`, d8Hit{"creation", "POST", "/api/atoms", ""}},
	{"add a question", "POST", "/api/atoms/a1/questions", `{}`, d8Hit{"creation", "POST", "/api/atoms/a1/questions", ""}},
	{"publish an atom", "POST", "/api/atoms/a1/publish", `{}`, d8Hit{"creation", "POST", "/api/atoms/a1/publish", ""}},
}

// Negative control: unset, the seed routes are outside the kiosk edition.
func TestKioskSeedRoutes_UnsetTheyAreRefused(t *testing.T) {
	rig := newD8Rig(t)
	for _, r := range seedRoutes {
		rec, hits := rig.do(r.method, r.target, r.body, rig.token)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "NOT_PART_OF_KIOSK_EDITION") || len(hits) != 0 {
			t.Errorf("%s unset: %d %s with %d upstream call(s)", r.name, rec.Code, rec.Body.String(), len(hits))
		}
	}
	k, err := loadKioskEdition()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(k.bootLine(), "seed routes off") {
		t.Errorf("boot line: %s", k.bootLine())
	}
}

func TestKioskSeedRoutes_OnTheyReachCreationBehindTheSession(t *testing.T) {
	rig := newD8RigWith(t, map[string]string{EnvKioskSeedRoutes: "on"})
	for _, r := range seedRoutes {
		rec, hits := rig.do(r.method, r.target, r.body, "")
		if rec.Code != http.StatusUnauthorized || len(hits) != 0 {
			t.Errorf("%s without a session: %d with %d upstream call(s)", r.name, rec.Code, len(hits))
		}
		rec, hits = rig.do(r.method, r.target, r.body, rig.token)
		if !reached(hits, r.want) {
			t.Errorf("%s: reached %v (status %d %s), want %v", r.name, hits, rec.Code, rec.Body.String(), r.want)
		}
	}
	// Only these three: another write on the same family stays refused.
	rec, hits := rig.do("DELETE", "/api/atoms/a1", "", rig.token)
	if !strings.Contains(rec.Body.String(), "NOT_PART_OF_KIOSK_EDITION") || len(hits) != 0 {
		t.Errorf("DELETE /api/atoms/a1 with seed routes on: %d %s", rec.Code, rec.Body.String())
	}
	k, err := loadKioskEdition()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(k.bootLine(), "seed routes on (POST /api/atoms, POST /api/atoms/{atomID}/questions, POST /api/atoms/{atomID}/publish)") {
		t.Errorf("boot line: %s", k.bootLine())
	}
}

func TestKioskSeedRoutes_AnyOtherValueRefusesTheBoot(t *testing.T) {
	for _, v := range []string{"true", "ON", "yes", "1", "off"} {
		setKioskEnv(t)
		t.Setenv(EnvKioskSeedRoutes, v)
		if _, err := loadKioskEdition(); !errors.Is(err, ErrKioskConfig) || !strings.Contains(err.Error(), EnvKioskSeedRoutes) {
			t.Errorf("%s=%q: err = %v, want ErrKioskConfig naming it", EnvKioskSeedRoutes, v, err)
		}
	}
}
