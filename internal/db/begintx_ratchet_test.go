package db

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A transaction opened by hand is a client-gone bug waiting to happen.
// database/sql binds a Tx to its ctx and rolls it back from a background
// goroutine the moment that ctx is canceled, so a statement or Commit landing
// after that rollback reports sql.ErrTxDone rather than the cancellation —
// and httpx.IsClientGone matches only context.Canceled, deliberately, since
// under a live ctx ErrTxDone is a real double-commit bug that must stay loud.
// A site that forgets db.TxCause therefore answers a disconnect with a 500 and
// an error-level log, and does it on a race, so it reproduces about half the
// time.
//
// Rather than trust each new transaction to remember, every site routes
// through db.InTx and this guards the set that does not. It is a SHRINKING
// list: a new entry needs a reason its transaction cannot be db.InTx's, and
// every return after its BeginTx has to go through db.TxCause by hand.
var beginTxRatchet = []string{
	// The three claims-bound helpers: each sets request.jwt.claims (and, on
	// Postgres, elevates the role) on the tx before handing TxStores to its
	// closure, and owns a span across the whole boundary.
	"ee/slack/store/pg/tx.go",
	// The Postgres test harness, which hands tests a tx to drive directly.
	"internal/db/pgtest/harness.go",
	"internal/db/postgres/tx.go",
	"internal/db/sqlite/tx.go",
	// db.InTx itself — the one place a transaction is opened without claims.
	"internal/db/withtx.go",
	// The websocket backplane's outbox publish. It logs per stage rather than
	// returning an error — an operator reads the difference between a failed
	// insert and a failed commit — so it keeps its own tx and wraps both
	// post-begin failures where they are logged.
	"internal/wsbackplane/publish.go",
}

func TestRatchet_HandRolledTransactionsStayAccountedFor(t *testing.T) {
	const repoRoot = "../.."

	if !sort.StringsAreSorted(beginTxRatchet) {
		t.Fatal("beginTxRatchet must stay sorted")
	}
	allowed := make(map[string]bool, len(beginTxRatchet))
	for _, path := range beginTxRatchet {
		if allowed[path] {
			t.Fatalf("duplicate ratchet entry: %s", path)
		}
		allowed[path] = true
	}

	found, err := filesCalling(repoRoot, "BeginTx(")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Fatal("scanned no files containing BeginTx( — the scan is broken, not the tree")
	}

	var violations []string
	for _, path := range found {
		if !allowed[path] {
			violations = append(violations, fmt.Sprintf(
				"%s opens a transaction by hand — route it through db.InTx, or add it to beginTxRatchet with the reason its transaction cannot be db.InTx's and every post-BeginTx return wrapped in db.TxCause",
				path))
		}
	}
	for _, path := range beginTxRatchet {
		if !foundSet(found)[path] {
			violations = append(violations, fmt.Sprintf(
				"stale ratchet entry %s: it no longer calls BeginTx — delete the entry so the ratchet shrinks", path))
		}
	}

	sort.Strings(violations)
	for _, violation := range violations {
		t.Error(violation)
	}
}

func foundSet(paths []string) map[string]bool {
	set := make(map[string]bool, len(paths))
	for _, path := range paths {
		set[path] = true
	}
	return set
}

// filesCalling returns the repo-relative paths of the non-test .go files whose
// source contains needle, sorted. Test files are excluded: a test driving a
// transaction it owns end to end is not a production client-gone path.
func filesCalling(root, needle string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// node_modules and .git hold no Go we own, and walking them is
			// most of the runtime of this test.
			if name := entry.Name(); name == "node_modules" || name == ".git" || name == "frontend" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if !strings.Contains(string(source), needle) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("relativize %s: %w", path, err)
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", root, err)
	}
	sort.Strings(paths)
	return paths, nil
}
