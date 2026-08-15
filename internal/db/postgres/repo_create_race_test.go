package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Two transactions creating the same repository under different casing cannot
// see each other's uncommitted rows. Against a case-SENSITIVE key that is a
// race and not a theoretical one — both would insert, their keys would differ,
// ON CONFLICT would catch neither, and TF would hold two rows for one GitHub
// repository, the state every reader downstream assumes cannot exist.
//
// What prevents it is the folded identity index: the second writer conflicts
// on it and blocks on the first while it is in flight, then DO NOTHING turns
// the resolved conflict into a no-op. That is a property of the schema, so it
// holds for any writer, including ones that never call this function.
//
// This drives the interleaving deterministically rather than hoping a goroutine
// race reproduces it: tx1 creates "Acme/Api" and is held open while tx2 tries
// "acme/api". Against a key that did not fold, tx2 would return immediately and
// the row count would be 2.
//
// Package-internal so it can reach insertRepoProfileRow with transactions it
// controls — the exported entry point opens (and closes) its own.
func TestInsertRepoProfileRow_CaseDifferingCreatorsResolveToOneRow(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	// repo_profiles FKs org_id, so the rows need an org to hang off.
	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "race")

	tx1, err := h.AdminDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	defer func() { _ = tx1.Rollback() }()

	if err := insertRepoProfileRow(ctx, tx1, orgID, domain.RepoRef{Owner: "Acme", Repo: "Api"}); err != nil {
		t.Fatalf("tx1 create: %v", err)
	}

	// tx2 runs on its own connection and must not be able to conclude the
	// repository is absent while tx1 holds it.
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		tx2, err := h.AdminDB.BeginTx(ctx, nil)
		if err != nil {
			done <- result{err}
			return
		}
		if err := insertRepoProfileRow(ctx, tx2, orgID, domain.RepoRef{Owner: "acme", Repo: "api"}); err != nil {
			_ = tx2.Rollback()
			done <- result{err}
			return
		}
		done <- result{tx2.Commit()}
	}()

	// The blocked-ness is the mechanism, so assert it: if tx2 finishes while
	// tx1 is still open it decided "absent" against an uncommitted row, which
	// is the defect even when the row count happens to come out right.
	select {
	case r := <-done:
		t.Fatalf("tx2 completed (err=%v) while tx1 held the repository open; "+
			"the identity key is not folding case, so both creators will insert", r.err)
	case <-time.After(300 * time.Millisecond):
	}

	if err := tx1.Commit(); err != nil {
		t.Fatalf("commit tx1: %v", err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("tx2 create: %v", r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tx2 never resumed after tx1 committed — the lock is not being released")
	}

	var rows int
	if err := h.AdminDB.QueryRow(
		`SELECT count(*) FROM repo_profiles WHERE org_id = $1`, orgID,
	).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("repo_profiles rows = %d, want 1 — a casing difference is not a second repository", rows)
	}
	var owner, repo string
	if err := h.AdminDB.QueryRow(
		`SELECT owner, repo FROM repo_profiles WHERE org_id = $1`, orgID,
	).Scan(&owner, &repo); err != nil {
		t.Fatalf("read surviving row: %v", err)
	}
	if owner != "Acme" || repo != "Api" {
		t.Errorf("surviving row = %s/%s, want Acme/Api — the first creator's casing is the sticky one", owner, repo)
	}
}
