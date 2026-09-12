package db

import (
	"io/fs"
	"strings"
	"testing"
)

// Two migration files sharing a version is not a merge conflict — git takes
// both happily, because they are different filenames — and it is not a
// compile error. It is a goose panic at tree-parse time, which takes down
// every test that opens a DB and every process at boot. Three sibling tickets
// off one epic each picked the next free version against main as it looked
// when they branched, two of them landed, and main went red on a collision
// neither PR could see on its own.
//
// So the tree is checked directly rather than through a migration run: a plain
// walk over both embedded trees, no DB, no Docker, no dialect setup. It fails
// on the PR that introduces the duplicate instead of on the merge that reveals
// it.
func TestMigrationVersionsAreUniquePerDialect(t *testing.T) {
	for _, dialect := range []string{"sqlite3", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			treeFS, dir, err := migrationsFor(dialect)
			if err != nil {
				t.Fatalf("migrationsFor(%s): %v", dialect, err)
			}
			entries, err := fs.ReadDir(treeFS, dir)
			if err != nil {
				t.Fatalf("read %s: %v", dir, err)
			}

			seen := map[string]string{} // version → the file that claimed it
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() || !strings.HasSuffix(name, ".sql") {
					continue
				}
				version, _, ok := strings.Cut(name, "_")
				if !ok {
					t.Errorf("%s/%s is not <version>_<description>.sql", dir, name)
					continue
				}
				// goose parses the version as an integer, so a non-numeric
				// prefix is its own boot failure.
				if strings.TrimLeft(version, "0123456789") != "" {
					t.Errorf("%s/%s has a non-numeric version prefix %q", dir, name, version)
					continue
				}
				if prior, dup := seen[version]; dup {
					t.Errorf("duplicate migration version %s: %q and %q — goose panics on this at "+
						"tree-parse time, so every DB-touching test and the boot path fail together. "+
						"Renumber the one that landed second.", version, prior, name)
					continue
				}
				seen[version] = name
			}
			if len(seen) == 0 {
				t.Fatalf("no migrations found under %s — the guard scanned nothing and has verified nothing", dir)
			}
		})
	}
}
