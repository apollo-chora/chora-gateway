package medashboard

// defend.go: the defend_hex card (UX Track U, C4).
//
// A province going cold, and the companion stationed there: "Ember is defending
// Equivalence Test tonight". Band 0 (nothing time-derived), warmth 25, which is
// the cooling-hex signal already named in rank key section 4.
//
// Fed by chora-consumption GET /v1/me/maps, whose Atlas cards carry
// coolingCount, coolingHexLabel and attachedCompanionName per map. That read is
// already on the Istio allowlist; a purpose-built route would have been dark.
//
// # One card, not one per map
//
// The continue_learning precedent: the home offers a way IN, it is not a second
// list. So the count is every cooling hex across every map, and the route deep
// links to ONE of them. Which one is a STATED rule and not an accident of map
// ordering, because the learner reads a sentence about it: the lowest goal id
// among the maps that actually have something cooling. Goal ids are UUIDv7, so
// lowest means the learner's OLDEST such map, which is a defensible answer to
// "where do I go first" rather than an arbitrary one. There is no cooling-since
// stamp anywhere in the campaign state to prefer instead: cooling is a derived
// predicate over the retention curve, never a stamped event.
//
// # Why card_context and not two more fields
//
// The companion name and the hex label have no home in the card envelope, and
// rank key section 2 says nothing in that envelope is optional for a card that
// is emitted. Adding two omitempty fields for one card would weaken the contract
// for every card. card_errors already set the precedent for a sibling map keyed
// off the card, present only where it applies, and a renderer that does not know
// a context key ignores it.

import (
	"encoding/json"
	"sort"
)

// defendCardID is stable per identity, as section 2 requires: a dismissal and a
// re-render must agree on what was dismissed. It therefore does NOT carry the
// chosen goal id, which moves as hexes cool and warm.
const defendCardID = "defend_hex"

// coolingMap is one Atlas card reduced to what the defend card needs.
type coolingMap struct {
	GoalID        string `json:"goalId"`
	CoolingCount  int    `json:"coolingCount"`
	HexLabel      string `json:"coolingHexLabel"`
	CompanionName string `json:"attachedCompanionName"`
}

// coolingRead is the decoded /v1/me/maps body.
//
// Partial is chora-consumption saying it could not compute the counts, inside a
// 200. Read as a zero it would be exactly the lie that flag exists to prevent,
// so it degrades the card to unread the same way a non-2xx does.
type coolingRead struct {
	Items   []coolingMap `json:"items"`
	Partial bool         `json:"coolingPartial"`
}

// decodeCooling reads the Atlas body. A body that will not parse yields no maps
// and no total, and the CARD STATE carries whether that zero is a fact.
func decodeCooling(body []byte) coolingRead {
	var out coolingRead
	if err := json.Unmarshal(body, &out); err != nil {
		return coolingRead{Partial: true}
	}
	return out
}

// total sums the cooling hexes across every map served.
func (c coolingRead) total() int {
	n := 0
	for _, m := range c.Items {
		if m.CoolingCount > 0 {
			n += m.CoolingCount
		}
	}
	return n
}

// pick chooses the map the card routes to: the lowest goal id AMONG THOSE WITH
// SOMETHING COOLING. Picking the lowest goal id overall would route the learner
// into a map with nothing to defend, which is the card's own claim broken by its
// own link.
func (c coolingRead) pick() (coolingMap, bool) {
	candidates := make([]coolingMap, 0, len(c.Items))
	for _, m := range c.Items {
		if m.CoolingCount > 0 && m.GoalID != "" {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 0 {
		return coolingMap{}, false
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].GoalID < candidates[j].GoalID })
	return candidates[0], true
}

// defendState maps the fan-out onto the section 5 states. A 200 carrying
// coolingPartial is UNREAD: the status line says the call succeeded, the flag
// says the answer inside it is not one.
func (l learnerReads) defendState(read coolingRead) string {
	if !l.consumptionWired {
		return cardStateAbsent
	}
	if !l.maps.ok() || read.Partial {
		return cardStateUnread
	}
	return cardStateLive
}

// defendCard builds the card and, when there is a province to name, its context
// entry. A nil context return means there is nothing to say: the card still
// rides, because a zero cooling count is a fact worth showing.
func defendCard(reads learnerReads) (cardDTO, map[string]string) {
	read := decodeCooling(reads.maps.body)
	state := reads.defendState(read)

	card := cardDTO{
		CardID: defendCardID, Kind: "defend_hex", Surface: "a",
		// The bare map surface, replaced below once a goal is chosen. A card
		// that could not be read still needs somewhere to send the learner.
		Route:   "/a/knowledge",
		Urgency: urgencyNone,
		// Rank key section 4: the cooling hex on a live goal signal.
		Warmth: 25,
		State:  state,
	}
	if state != cardStateLive {
		return card, nil
	}
	card.Count = read.total()

	chosen, ok := read.pick()
	if !ok {
		// Nothing cooling: a live zero, and nothing to name. The zero STAYS on
		// the wire. The A+ home drops every live count-0 card by one uniform
		// rule so its empty-state copy can render, so this card does not reach
		// that learner; O+ and any other consumer of the envelope still get the
		// fact. Two layers, one intent: the server states what is true and the
		// home decides what is worth a tile.
		return card, nil
	}
	card.Route = "/a/knowledge/" + chosen.GoalID

	ctx := map[string]string{"goal_id": chosen.GoalID}
	if chosen.HexLabel != "" {
		ctx["hex_label"] = chosen.HexLabel
	}
	// Omitted rather than empty: "nobody is stationed here" and "we did not look"
	// are different, and an empty string would render as a nameless defender.
	if chosen.CompanionName != "" {
		ctx["companion_name"] = chosen.CompanionName
	}
	return card, ctx
}
