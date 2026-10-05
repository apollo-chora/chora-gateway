package medashboard

// medashboard_defend_test.go: the defend_hex card (UX Track U, C4).
//
// The card names a province going cold and the companion stationed there:
// "Ember is defending Equivalence Test tonight". Band 0, warmth 25, route into
// the map that holds the hex.
//
// Two shapes here that the other cards do not have, and both are asserted:
//
// It is ONE card for the whole set, like continue_learning, not one per goal.
// The count is every cooling hex across every live map; the route deep links to
// ONE of them, picked by a stated deterministic rule, because a home that hands
// back the same choice it exists to make for the learner is a list, not a card.
//
// It carries card_context, the sibling map keyed by card_id that holds the
// companion name and the hex label. Those two have no field in the card
// envelope, and section 2 of the rank key says nothing in that envelope is
// optional, so extending it for one card would weaken the contract for every
// card. card_errors set the precedent for a keyed sibling map.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const pathMaps = "/v1/me/maps"

// defendWire is the envelope plus the new sibling map.
type defendWire struct {
	Cards       []cardWire                   `json:"cards"`
	CardContext map[string]map[string]string `json:"card_context"`
}

// defendCards drives one dashboard read against a maps body and returns the
// cards by kind plus the context map.
func defendCards(t *testing.T, mapsBody string, mapsStatus int, wireConsumption bool) (map[string]cardWire, map[string]map[string]string) {
	t.Helper()
	stub := liveStub()
	if mapsStatus != http.StatusOK {
		stub.status[pathMaps] = mapsStatus
	} else {
		stub.body[pathMaps] = mapsBody
	}
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"display_name":"Dale"}`))
	}))
	t.Cleanup(identity.Close)

	cfg := Config{IdentityURL: identity.URL}
	if wireConsumption {
		cfg.ConsumptionURL = srv.URL
	}
	resp, _ := New(cfg).GetDashboard(context.Background(),
		AuthCtx{TenantID: "t-1", GCID: "gcid-1", Roles: []string{"learner"}, TimeZone: "UTC"})

	var got defendWire
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, resp.Body)
	}
	byKind := map[string]cardWire{}
	for _, c := range got.Cards {
		byKind[c.Kind] = c
	}
	return byKind, got.CardContext
}

// twoCoolingMaps: g-b holds two cooling hexes, g-a one. g-a sorts lower, so it
// is the deterministic pick; the count is all three.
const twoCoolingMaps = `{"items":[
 {"goalId":"g-b","title":"Poetry","coolingCount":2,"coolingHexLabel":"Sonnets","attachedCompanionName":"Iris"},
 {"goalId":"g-a","title":"Algebra","coolingCount":1,"coolingHexLabel":"Equivalence Test","attachedCompanionName":"Ember"}
]}`

// The card exists, sits in band 0 at warmth 25, counts every cooling hex across
// every map, and routes into the deterministically chosen one.
func TestDefendHex_CountsEveryCoolingHexAndRoutesToOne(t *testing.T) {
	cards, _ := defendCards(t, twoCoolingMaps, http.StatusOK, true)

	c, ok := cards["defend_hex"]
	if !ok {
		t.Fatalf("defend_hex card missing")
	}
	if c.State != "live" {
		t.Errorf("state = %q; want live", c.State)
	}
	if c.Count != 3 {
		t.Errorf("count = %d; want 3 (every cooling hex across both maps)", c.Count)
	}
	if c.Urgency != 0 {
		t.Errorf("urgency = %d; want 0 (nothing time-derived)", c.Urgency)
	}
	if c.Warmth != 25 {
		t.Errorf("warmth = %d; want 25 (rank key section 4, the cooling hex signal)", c.Warmth)
	}
	if c.Surface != "a" {
		t.Errorf("surface = %q; want a", c.Surface)
	}
	if c.Route != "/a/knowledge/g-a" {
		t.Errorf("route = %q; want /a/knowledge/g-a (lowest goal id among cooling maps)", c.Route)
	}
}

// The context map carries what the envelope has no field for, keyed by card_id,
// so the home can write the sentence. It is a SIBLING map: the card envelope is
// unchanged.
func TestDefendHex_ContextNamesTheDefenderAndTheHex(t *testing.T) {
	cards, ctx := defendCards(t, twoCoolingMaps, http.StatusOK, true)

	c := cards["defend_hex"]
	got, ok := ctx[c.CardID]
	if !ok {
		t.Fatalf("card_context has no entry for %q (have %v)", c.CardID, ctx)
	}
	for key, want := range map[string]string{
		"companion_name": "Ember",
		"hex_label":      "Equivalence Test",
		"goal_id":        "g-a",
	} {
		if got[key] != want {
			t.Errorf("card_context[%s] = %q; want %q", key, got[key], want)
		}
	}
}

// A map with no companion stationed omits companion_name rather than sending an
// empty string, so the home can tell "nobody is defending this" from "we did not
// look". The hex label and goal id still ride.
func TestDefendHex_NoStationedCompanionOmitsTheName(t *testing.T) {
	const body = `{"items":[{"goalId":"g-a","coolingCount":1,"coolingHexLabel":"Equivalence Test"}]}`
	cards, ctx := defendCards(t, body, http.StatusOK, true)

	got := ctx[cards["defend_hex"].CardID]
	if _, present := got["companion_name"]; present {
		t.Errorf("companion_name present (%q); want omitted when nothing is stationed", got["companion_name"])
	}
	if got["hex_label"] != "Equivalence Test" {
		t.Errorf("hex_label = %q; want Equivalence Test", got["hex_label"])
	}
}

// Nothing cooling is a FACT, not an absence: the card is emitted live with a
// zero. Dropping it would make a quiet day and a dark upstream look identical,
// which is the distinction the whole envelope exists for.
func TestDefendHex_NothingCoolingIsLiveWithZero(t *testing.T) {
	const body = `{"items":[{"goalId":"g-a","coolingCount":0}]}`
	cards, ctx := defendCards(t, body, http.StatusOK, true)

	c, ok := cards["defend_hex"]
	if !ok {
		t.Fatalf("defend_hex card missing; a zero is a fact and must still be emitted")
	}
	if c.State != "live" || c.Count != 0 {
		t.Errorf("state/count = %q/%d; want live/0", c.State, c.Count)
	}
	if _, present := ctx[c.CardID]; present {
		t.Errorf("card_context entry present for a card with nothing to name; want omitted")
	}
}

// A failed maps read is unread, never live with a zero.
func TestDefendHex_AFailedMapsReadIsUnread(t *testing.T) {
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusInternalServerError} {
		cards, _ := defendCards(t, "", code, true)
		c, ok := cards["defend_hex"]
		if !ok {
			t.Fatalf("defend_hex card missing on %d; an unread card is still emitted", code)
		}
		if c.State != "unread" {
			t.Errorf("state on %d = %q; want unread", code, c.State)
		}
	}
}

// coolingPartial is chora-consumption saying it could not compute the counts,
// inside a 200. Read as live-with-zero it would be exactly the lie the flag
// exists to prevent, so the card must read unread.
func TestDefendHex_ConsumptionPartialIsUnreadNotZero(t *testing.T) {
	const body = `{"items":[{"goalId":"g-a","coolingCount":0}],"coolingPartial":true}`
	cards, _ := defendCards(t, body, http.StatusOK, true)

	if s := cards["defend_hex"].State; s != "unread" {
		t.Errorf("state = %q; want unread (coolingPartial inside a 200 is not a zero)", s)
	}
}

// No consumption downstream at all is absent, the same rule the other learner
// cards follow.
func TestDefendHex_NoConsumptionDownstreamIsAbsent(t *testing.T) {
	cards, _ := defendCards(t, twoCoolingMaps, http.StatusOK, false)

	if s := cards["defend_hex"].State; s != "absent" {
		t.Errorf("state = %q; want absent (no consumption downstream in this deployment)", s)
	}
}

// A retired map is filtered out by chora-consumption, but a map with no cooling
// hexes must not be picked as the route either: the deterministic pick is the
// lowest goal id AMONG THOSE COOLING, not the lowest goal id overall.
func TestDefendHex_TheRouteSkipsMapsWithNothingCooling(t *testing.T) {
	const body = `{"items":[
	 {"goalId":"g-a","coolingCount":0,"attachedCompanionName":"Ember"},
	 {"goalId":"g-z","coolingCount":1,"coolingHexLabel":"Sonnets","attachedCompanionName":"Iris"}
	]}`
	cards, ctx := defendCards(t, body, http.StatusOK, true)

	c := cards["defend_hex"]
	if c.Route != "/a/knowledge/g-z" {
		t.Errorf("route = %q; want /a/knowledge/g-z (g-a has nothing cooling)", c.Route)
	}
	if got := ctx[c.CardID]["companion_name"]; got != "Iris" {
		t.Errorf("companion_name = %q; want Iris (the defender of the CHOSEN map)", got)
	}
}

// card_id is stable per identity, as section 2 requires: a dismissal and a
// re-render must agree on what was dismissed, so it cannot carry the goal id
// that the route picks and that can change as hexes cool and warm.
func TestDefendHex_CardIDIsStableAcrossDifferentChosenGoals(t *testing.T) {
	first, _ := defendCards(t, twoCoolingMaps, http.StatusOK, true)
	const other = `{"items":[{"goalId":"g-z","coolingCount":1,"coolingHexLabel":"Sonnets"}]}`
	second, _ := defendCards(t, other, http.StatusOK, true)

	if first["defend_hex"].CardID != second["defend_hex"].CardID {
		t.Errorf("card_id moved with the chosen goal: %q then %q",
			first["defend_hex"].CardID, second["defend_hex"].CardID)
	}
	if first["defend_hex"].Route == second["defend_hex"].Route {
		t.Errorf("route did NOT move with the chosen goal (%q); the test proves nothing",
			first["defend_hex"].Route)
	}
}
