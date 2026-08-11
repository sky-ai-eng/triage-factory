package db

import (
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// The migration that folds Jira entity keys to their canonical spelling
// (202608100001). It runs on real deployed data — every build up to the
// normalization fix recorded whatever spelling a caller supplied — so the test
// stages rows the old way and finishes the migration over them.
//
// Two of the assertions are load-bearing beyond "UPDATE works". (source,
// source_id) is UNIQUE, so a fold onto an occupied key would fail the migration
// and, because migrations run at boot, take the process down with it; the
// colliding pair proves the guard holds. And GitHub source_ids are
// case-sensitive natural keys ("owner/Repo#1"), so the source='jira' predicate
// is what keeps a repair for one provider from corrupting another's.
func TestMigrate_CanonicalizesJiraEntityKeys(t *testing.T) {
	database := openMigrationsTestDB(t)

	gooseMu.Lock()
	treeFS, dir, err := migrationsFor("sqlite3")
	if err != nil {
		gooseMu.Unlock()
		t.Fatalf("migrationsFor: %v", err)
	}
	goose.SetBaseFS(treeFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		gooseMu.Unlock()
		t.Fatalf("SetDialect: %v", err)
	}
	// Stop one version short of the canonicalization.
	upToErr := goose.UpTo(database, dir, 202608080001)
	gooseMu.Unlock()
	if upToErr != nil {
		t.Fatalf("goose.UpTo previous version: %v", upToErr)
	}

	seed := func(id, source, sourceID string) {
		t.Helper()
		if _, err := database.Exec(
			`INSERT INTO entities (id, source, source_id, kind, title) VALUES (?, ?, ?, 'issue', 'seeded')`,
			id, source, sourceID,
		); err != nil {
			t.Fatalf("seed %s/%s: %v", source, sourceID, err)
		}
	}
	// Minted under a non-canonical spelling with nothing occupying the
	// canonical key — the case the repair exists for.
	seed("lone", "jira", "sky-1")
	// The same issue tracked twice, once under each spelling. Folding the
	// non-canonical row would collide with its twin.
	seed("twin-canonical", "jira", "SKY-2")
	seed("twin-variant", "jira", "sky-2")
	// Two variants that are BOTH non-canonical, with nothing yet occupying
	// the spelling they would both fold onto — an agent that addressed one
	// issue two ways across separate calls before the fix. The NOT EXISTS
	// guard cannot see this collision (at statement start neither row holds
	// SKY-4), so without the one-winner clause the UPDATE trips the UNIQUE
	// constraint and fails the migration outright — which, since migrations
	// run at boot, is a process that will not start.
	seed("dupe-a", "jira", "sky-4")
	seed("dupe-b", "jira", "Sky-4")
	// Already canonical, and a non-Jira key that is legitimately lower-case.
	seed("already", "jira", "SKY-3")
	seed("gh", "github", "owner/repo#1")

	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	keyOf := func(id string) string {
		t.Helper()
		var got string
		if err := database.QueryRow(`SELECT source_id FROM entities WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return got
	}

	if got := keyOf("lone"); got != "SKY-1" {
		t.Errorf("lone non-canonical key = %q, want SKY-1 — unfolded, it can never match its own issue in a refresh", got)
	}
	if got := keyOf("twin-variant"); got != "sky-2" {
		t.Errorf("colliding key = %q, want it left as sky-2 — its canonical spelling is taken, and merging two entities is not a rename", got)
	}
	if got := keyOf("twin-canonical"); got != "SKY-2" {
		t.Errorf("healthy twin = %q, want SKY-2 untouched", got)
	}
	if got := keyOf("already"); got != "SKY-3" {
		t.Errorf("already-canonical key = %q, want SKY-3 unchanged", got)
	}
	if got := keyOf("gh"); got != "owner/repo#1" {
		t.Errorf("github key = %q, want owner/repo#1 — only Jira keys are case-insensitive at the source", got)
	}
	// Exactly one of the two all-variant duplicates folds; the other is left
	// as a duplicate, like any other. Which one wins is decided by the
	// migration (lowest id), not by the engine's scan order — asserting the
	// specific outcome is what keeps that deterministic.
	if got := keyOf("dupe-a"); got != "SKY-4" {
		t.Errorf("elected variant = %q, want SKY-4", got)
	}
	if got := keyOf("dupe-b"); got != "Sky-4" {
		t.Errorf("losing variant = %q, want it left as Sky-4 — folding both would collide", got)
	}
}
