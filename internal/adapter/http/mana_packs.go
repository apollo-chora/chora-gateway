// mana_packs.go — server-side mana-pack price catalogue (WS-2.4).
//
// Mana is the umbrella prepaid-credit currency for every LLM call (ADR-142 §4),
// so the per-user top-up PRICE must be resolved SERVER-SIDE from a trusted
// catalogue — never from the client request body. Trusting a client-supplied
// amount_cents would let a caller mint arbitrary mana for an arbitrary price
// (free LLM). The browser sends only a `sku`; this catalogue maps it to the
// authoritative {mana_units, amount_cents, currency}.
//
// Per feedback_no_inline_config the catalogue is sourced from the
// CHORA_MANA_PACKS env var (a JSON array; injected from Secret Manager /
// Terraform), NOT hard-coded. When unset, the /api/v1/checkout/user-mana route
// fails loud (503) rather than falling back to a client-priced mint.
package httpadapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ManaPack is one purchasable mana bundle.
type ManaPack struct {
	SKU         string `json:"sku"`
	ManaUnits   int64  `json:"mana_units"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
}

// ManaPackCatalogue maps SKU → ManaPack. nil/empty ⇒ the user-mana checkout
// route is disabled (503).
type ManaPackCatalogue map[string]ManaPack

// ParseManaPackCatalogue parses the CHORA_MANA_PACKS env value — a JSON array
// of ManaPack objects, e.g.:
//
//	[{"sku":"mana_pack_1000","mana_units":1000,"amount_cents":199,"currency":"USD"},
//	 {"sku":"mana_pack_5000","mana_units":5000,"amount_cents":799,"currency":"USD"}]
//
// Returns (nil, nil) for empty input (route stays disabled). Returns an error
// for malformed JSON or an entry that fails validation so a misconfigured
// catalogue fails loud at boot.
func ParseManaPackCatalogue(raw string) (ManaPackCatalogue, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var packs []ManaPack
	if err := json.Unmarshal([]byte(raw), &packs); err != nil {
		return nil, fmt.Errorf("mana packs: invalid CHORA_MANA_PACKS JSON: %w", err)
	}
	cat := make(ManaPackCatalogue, len(packs))
	for i, p := range packs {
		p.SKU = strings.TrimSpace(p.SKU)
		p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
		if p.SKU == "" {
			return nil, fmt.Errorf("mana packs: entry %d missing sku", i)
		}
		if p.ManaUnits <= 0 {
			return nil, fmt.Errorf("mana packs: sku %q mana_units must be > 0", p.SKU)
		}
		if p.AmountCents <= 0 {
			return nil, fmt.Errorf("mana packs: sku %q amount_cents must be > 0", p.SKU)
		}
		if len(p.Currency) != 3 {
			return nil, fmt.Errorf("mana packs: sku %q currency must be a 3-letter ISO code", p.SKU)
		}
		if _, dup := cat[p.SKU]; dup {
			return nil, fmt.Errorf("mana packs: duplicate sku %q", p.SKU)
		}
		cat[p.SKU] = p
	}
	return cat, nil
}

// Lookup returns the pack for a SKU. ok=false for an unknown SKU.
func (c ManaPackCatalogue) Lookup(sku string) (ManaPack, bool) {
	if c == nil {
		return ManaPack{}, false
	}
	p, ok := c[strings.TrimSpace(sku)]
	return p, ok
}

// errManaPacksNotConfigured signals the catalogue is empty (route disabled).
var errManaPacksNotConfigured = errors.New("mana packs not configured")
