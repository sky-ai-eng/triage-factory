package postgres_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/pgnotify"
)

// credDoorbells starts a tf_ctl listener and returns a channel of the
// cred_request messages it hears, plus a func that blocks until the listener is
// provably registered (NOTIFY only reaches sessions already listening).
func credDoorbells(t *testing.T, h *pgtest.Harness) (<-chan ctlbus.Message, func()) {
	t.Helper()
	dsn, err := h.Container.ConnectionString(context.Background(), "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	msgs := make(chan ctlbus.Message, 16)
	listener := pgnotify.NewListener(dsn, ctlbus.Channel, func(payload string) {
		var m ctlbus.Message
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			return
		}
		msgs <- m
	})
	go listener.Run(ctx)

	ready := func() {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			if err := ctlbus.Publish(context.Background(), h.AdminDB, ctlbus.Message{Kind: "ping"}); err != nil {
				t.Fatalf("publish readiness ping: %v", err)
			}
			select {
			case <-msgs:
				// Drain any further pings still in flight so a later
				// assertion never mistakes one for a doorbell.
				for {
					select {
					case <-msgs:
					case <-time.After(200 * time.Millisecond):
						return
					}
				}
			case <-time.After(200 * time.Millisecond):
				if time.Now().After(deadline) {
					t.Fatal("tf_ctl listener never became ready")
				}
			}
		}
	}
	return msgs, ready
}

// awaitCredRequest waits for the next cred_request doorbell, ignoring the
// readiness pings.
func awaitCredRequest(t *testing.T, msgs <-chan ctlbus.Message) ctlbus.Message {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case m := <-msgs:
			if m.Kind != "cred_request" {
				continue
			}
			return m
		case <-deadline:
			t.Fatal("timed out waiting for the cred_request doorbell")
			return ctlbus.Message{}
		}
	}
}

// expectNoCredRequest asserts no doorbell arrives within a short settling
// window.
func expectNoCredRequest(t *testing.T, msgs <-chan ctlbus.Message) {
	t.Helper()
	settle := time.After(2 * time.Second)
	for {
		select {
		case m := <-msgs:
			if m.Kind == "cred_request" {
				t.Fatalf("unexpected cred_request doorbell: %+v", m)
			}
		case <-settle:
			return
		}
	}
}

// TestRunWorktreeStore_Postgres_InsertRingsCredDoorbell pins the widening
// signal: a genuinely new conversation_worktrees row means the run's authorized
// repo set grew past what its sealed credential bundle covers, so the insert
// fires the same cred_request the claim's own credential request fires. A
// conflicting insert (the row already existed) widens nothing and must stay
// silent, or every idempotent `workspace add` would re-mint the run's tokens.
func TestRunWorktreeStore_Postgres_InsertRingsCredDoorbell(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	conversationID := seedPgArtifactConversation(t, h, orgID, teamID, userID)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	// The worktree store resolves the slug to a registry row and never creates
	// one (the executor holds no INSERT on repositories), so the fixture brings
	// each repository into existence the way tracking would.
	pgtest.SeedRepository(t, h, orgID, "sky-ai-eng", "other-repo")
	pgtest.SeedRepository(t, h, orgID, "sky-ai-eng", "third-repo")

	msgs, ready := credDoorbells(t, h)
	ready()

	row := domain.ConversationWorktree{ConversationID: conversationID, RepoID: "sky-ai-eng/other-repo", Path: "/runs/" + conversationID + "/sky-ai-eng/other-repo/@default", Ref: "@default"}
	inserted, _, err := stores.ConversationWorktrees.InsertSystem(ctx, orgID, row)
	if err != nil || !inserted {
		t.Fatalf("InsertSystem: inserted=%v err=%v", inserted, err)
	}
	got := awaitCredRequest(t, msgs)
	if got.OrgID != orgID || got.ConversationID != conversationID {
		t.Errorf("doorbell = %+v, want org %s run %s", got, orgID, conversationID)
	}

	// Re-adding the same (run, repo, ref) conflicts: nothing widened.
	inserted, _, err = stores.ConversationWorktrees.InsertSystem(ctx, orgID, row)
	if err != nil || inserted {
		t.Fatalf("InsertSystem (conflict): inserted=%v err=%v, want inserted=false", inserted, err)
	}
	expectNoCredRequest(t, msgs)

	// A NEW ref in a repo the run already holds is a genuinely new row, but
	// credentials are minted per repo — the authorized set is unchanged, so
	// re-sealing would spend GitHub App mint quota on a byte-identical grant.
	secondRef := domain.ConversationWorktree{ConversationID: conversationID, RepoID: "sky-ai-eng/other-repo", Path: "/runs/" + conversationID + "/sky-ai-eng/other-repo/pr-42", Ref: "pr-42"}
	inserted, _, err = stores.ConversationWorktrees.InsertSystem(ctx, orgID, secondRef)
	if err != nil || !inserted {
		t.Fatalf("InsertSystem (second ref): inserted=%v err=%v, want inserted=true", inserted, err)
	}
	expectNoCredRequest(t, msgs)

	// A different repo does widen the set.
	otherRepo := domain.ConversationWorktree{ConversationID: conversationID, RepoID: "sky-ai-eng/third-repo", Path: "/runs/" + conversationID + "/sky-ai-eng/third-repo/@default", Ref: "@default"}
	inserted, _, err = stores.ConversationWorktrees.InsertSystem(ctx, orgID, otherRepo)
	if err != nil || !inserted {
		t.Fatalf("InsertSystem (new repo): inserted=%v err=%v, want inserted=true", inserted, err)
	}
	if got := awaitCredRequest(t, msgs); got.ConversationID != conversationID {
		t.Errorf("doorbell = %+v, want run %s", got, conversationID)
	}
}

// TestRunWorktreeStore_Postgres_RolledBackInsertRingsNoDoorbell pins the
// load-bearing half of the doorbell's placement: it is published on the SAME
// connection that ran the INSERT, so pg_notify rides the transaction and is
// delivered at COMMIT. Published on a separate pool handle it would fire before
// the commit, letting the brain read a pre-insert state and seal a bundle
// without the new repo — the very bug the doorbell exists to fix, reintroduced
// through its own fix. A rolled-back insert must therefore be silent.
func TestRunWorktreeStore_Postgres_RolledBackInsertRingsNoDoorbell(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
	conversationID := seedPgArtifactConversation(t, h, orgID, teamID, userID)

	pgtest.SeedRepository(t, h, orgID, "sky-ai-eng", "other-repo")

	msgs, ready := credDoorbells(t, h)
	ready()

	tx, err := h.AdminDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	txStores := pgstore.NewForTx(tx, pgtest.SecretKey)
	inserted, _, err := txStores.ConversationWorktrees.Insert(ctx, orgID, domain.ConversationWorktree{
		ConversationID: conversationID, RepoID: "sky-ai-eng/other-repo", Path: "/runs/x", Ref: "@default",
	})
	if err != nil || !inserted {
		t.Fatalf("Insert in tx: inserted=%v err=%v", inserted, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	expectNoCredRequest(t, msgs)
}
