package httpadapter

import "testing"

func TestParseManaPackCatalogue_HappyPath(t *testing.T) {
	cat, err := ParseManaPackCatalogue(
		`[{"sku":"mana_pack_1000","mana_units":1000,"amount_cents":199,"currency":"usd"},
		  {"sku":"mana_pack_5000","mana_units":5000,"amount_cents":799,"currency":"USD"}]`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cat) != 2 {
		t.Fatalf("len = %d, want 2", len(cat))
	}
	p, ok := cat.Lookup("mana_pack_1000")
	if !ok || p.ManaUnits != 1000 || p.AmountCents != 199 || p.Currency != "USD" {
		t.Fatalf("lookup mana_pack_1000 = %+v ok=%v (currency must be upper-cased)", p, ok)
	}
}

func TestParseManaPackCatalogue_EmptyDisablesRoute(t *testing.T) {
	cat, err := ParseManaPackCatalogue("   ")
	if err != nil {
		t.Fatalf("empty must not error: %v", err)
	}
	if cat != nil {
		t.Fatalf("empty input must yield nil catalogue, got %v", cat)
	}
	if _, ok := cat.Lookup("anything"); ok {
		t.Fatal("nil catalogue Lookup must be ok=false")
	}
}

func TestParseManaPackCatalogue_Rejects(t *testing.T) {
	cases := map[string]string{
		"bad json":      `{not json`,
		"missing sku":   `[{"mana_units":100,"amount_cents":10,"currency":"USD"}]`,
		"zero units":    `[{"sku":"x","mana_units":0,"amount_cents":10,"currency":"USD"}]`,
		"zero amount":   `[{"sku":"x","mana_units":10,"amount_cents":0,"currency":"USD"}]`,
		"bad currency":  `[{"sku":"x","mana_units":10,"amount_cents":10,"currency":"DOLLARS"}]`,
		"duplicate sku": `[{"sku":"x","mana_units":10,"amount_cents":10,"currency":"USD"},{"sku":"x","mana_units":20,"amount_cents":20,"currency":"USD"}]`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseManaPackCatalogue(raw); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}
