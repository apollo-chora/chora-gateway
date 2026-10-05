package httpadapter

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// The O+ governance data catalogue is a hand-maintained Go literal naming
// (database, table) pairs on a COMPLIANCE surface. On 2026-08-23 a sweep found
// 17 of its 36 rows named tables that did not exist, three domains being
// entirely fictional, and the suite asserted one of the fictional names, so the
// tests defended the drift instead of catching it. This gate is the check that
// would have caught all 17 the day they appeared.
//
// ⚠⚠⚠ WHY THIS COMPARES AGAINST A COMMITTED SNAPSHOT AND NOT A LIVE DATABASE,
// AND THE RISK THAT CARRIES.
// This estate is routinely cost-paused with Cloud SQL STOPPED. A gate needing 13
// live connections would red-build during a scheduled operational state and
// would then be disabled, and a disabled gate is worse than no gate because it
// also carries the false assurance that the check exists.
//
// The cost is that the snapshot is a SECOND hand-maintained artefact and will
// drift exactly as the catalogue did. If it drifts, this gate compares one
// fiction against another and PASSES. The snapshot is therefore NOT SAFE without
// `chora-infra/scripts/schema-snapshot-refresh.py --check` running where live
// database access already exists. If only one of the two can be operated,
// operate that one.
const schemaSnapshotPath = "testdata/schema_snapshot.json"

func loadSchemaSnapshot(t *testing.T) map[string]map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(schemaSnapshotPath)
	if err != nil {
		t.Fatalf("read %s: %v", schemaSnapshotPath, err)
	}
	var doc struct {
		Databases map[string][]string `json:"databases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode %s: %v", schemaSnapshotPath, err)
	}
	if len(doc.Databases) == 0 {
		t.Fatalf("%s carries no databases; an empty snapshot would make this gate vacuous",
			schemaSnapshotPath)
	}
	out := make(map[string]map[string]bool, len(doc.Databases))
	for db, tables := range doc.Databases {
		set := make(map[string]bool, len(tables))
		for _, tb := range tables {
			set[tb] = true
		}
		out[db] = set
	}
	return out
}

// pendingGovernanceRuling holds the rows KNOWN to name a non-existent table and
// deliberately left uncorrected, because choosing their target is a governance
// decision rather than a correctness fix. Each carries its reason so the next
// reader meets it before the code.
//
// ⚠ THIS LIST IS BIDIRECTIONAL AND MUST SHRINK. The gate fails if a fiction
// appears that is NOT listed here, AND it fails if a listed row starts existing
// and is left behind. A one-directional allowlist becomes permanent, which is
// how a known exception turns back into an unknown one.
var pendingGovernanceRuling = map[string]string{
	// Relocated to chora_consumption by consumption migration 0002, so
	// correcting it also moves the Domain attribution off Content Creation,
	// which is a governance semantic and not a name fix.
	"chora_creation.atom_semantic_edges": "relocated to chora_consumption; owner to confirm",
	// A JSONB COLUMN on learning_atoms (0014_atom_phase1.up.sql), not a table.
	// Whether a data catalogue carries a row for a column is a governance call.
	"chora_creation.media_assets": "is a JSONB column on learning_atoms, not a table",
	// Never created by any migration and referenced by no Go code. Two
	// plausible real candidates (subscriber_preferences, subscription_preferences).
	"chora_notifications.channel_preferences": "ambiguous between two real candidates",
}

// TestDataCatalogueNamesOnlyRealTables is the gate: every (db, table) the
// catalogue asserts must exist in that database, except rows explicitly held
// for a governance ruling.
func TestDataCatalogueNamesOnlyRealTables(t *testing.T) {
	snapshot := loadSchemaSnapshot(t)

	rows, ok := defaultDataLineage().([]lineageRow)
	if !ok || len(rows) == 0 {
		t.Fatalf("defaultDataLineage did not yield rows: %T", defaultDataLineage())
	}

	var fictional []string
	for _, r := range rows {
		tables, known := snapshot[r.DB]
		if !known {
			t.Errorf("row %s/%s names database %q, absent from the snapshot: either the "+
				"database is wrong or the snapshot is stale", r.Domain, r.Table, r.DB)
			continue
		}
		// The PAIR is the assertion, not the name. A table that exists in some
		// OTHER database does not make this row true; that is exactly how a
		// relocated table (atom_semantic_edges, moved from chora_creation to
		// chora_consumption) reads as correct to a name-only check.
		if !tables[r.Table] {
			fictional = append(fictional, r.DB+"."+r.Table)
		}
	}
	sort.Strings(fictional)
	var unexpected []string
	stillFictional := map[string]bool{}
	for _, f := range fictional {
		stillFictional[f] = true
		if _, held := pendingGovernanceRuling[f]; !held {
			unexpected = append(unexpected, f)
		}
	}
	for _, f := range unexpected {
		t.Errorf("data catalogue names %s, which does not exist in that database. This is served "+
			"on a compliance surface. Correct the row against the real schema, or if the snapshot "+
			"is stale run chora-infra/scripts/schema-snapshot-refresh.py --check", f)
	}
	// The other direction: a held row that now EXISTS must leave the list, or
	// the exception outlives the reason for it.
	for pair, why := range pendingGovernanceRuling {
		if !stillFictional[pair] {
			t.Errorf("%s is in pendingGovernanceRuling (%q) but now exists. Remove it from the "+
				"list so the exception does not outlive its reason", pair, why)
		}
	}
}

// The gate is worthless if the snapshot cannot answer for the databases the
// catalogue references, so assert the coverage rather than assuming it.
func TestSchemaSnapshotCoversEveryDatabaseTheCatalogueNames(t *testing.T) {
	snapshot := loadSchemaSnapshot(t)
	rows, _ := defaultDataLineage().([]lineageRow)
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.DB] {
			continue
		}
		seen[r.DB] = true
		if _, ok := snapshot[r.DB]; !ok {
			t.Errorf("catalogue names database %q with no snapshot entry", r.DB)
		}
	}
	// A snapshot whose per-database lists were empty would pass the gate above
	// vacuously for any row, so refuse that shape explicitly.
	for db, tables := range snapshot {
		if len(tables) == 0 {
			t.Errorf("snapshot database %q lists zero tables; the gate would be vacuous for it", db)
		}
	}
}
