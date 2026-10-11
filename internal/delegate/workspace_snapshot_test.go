package delegate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// TestEnsureWorkspace_WarmTreeIsReused: a parked run root that survived on disk
// is returned as-is. A file written after the snapshot survives, which a
// rebuild from the older blob would have lost.
func TestEnsureWorkspace_WarmTreeIsReused(t *testing.T) {
	f := newSnapshotFixture(t, "task-warm")
	f.addCheckout(t, "acme/app", "default")
	f.snapshot(t, "", domain.ConversationRuntimeSDK)
	marker := filepath.Join(f.root, worktree.ScratchDir, "warm-marker.txt")
	writeFile(t, marker, "warm")

	got, prov, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conv(""), f.restorer(), nil)
	if err != nil {
		t.Fatalf("ensureWorkspace: %v", err)
	}
	if prov != domain.WorkspaceProvenanceWarm || got != f.root {
		t.Errorf("ensureWorkspace = (%q, %q), want the warm root %q", got, prov, f.root)
	}
	assertFileContains(t, marker, "warm")
	if len(f.ledger.recordedRows()) != 0 {
		t.Errorf("a warm tree recorded checkouts %v; nothing was rebuilt", f.ledger.recordedRows())
	}
}

// TestEnsureWorkspace_ColdRoundTrip is the restore acceptance: a run root with
// a PR checkout and checkouts of two other repos — each with an unpushed
// commit, an uncommitted edit and an untracked file — plus scratch and a
// session transcript, rebuilt after the root is gone. Every checkout comes
// back exactly, as a self-contained clone when the run trees are, the PR
// checkout with its push tracking, and each is recorded as the restoring
// conversation's. A snapshot of the rebuilt tree carries all three again.
func TestEnsureWorkspace_ColdRoundTrip(t *testing.T) {
	for _, selfContained := range []bool{false, true} {
		name := "linked"
		if selfContained {
			name = "self-contained"
		}
		t.Run(name, func(t *testing.T) {
			f := newSnapshotFixture(t, "task-cold")
			runmode.SetLocalSandboxForTest(t, selfContained)

			pr := f.addCheckout(t, "acme/app", "pr-7")
			lib := f.addCheckout(t, "acme/lib", "default")
			docs := f.addCheckout(t, "acme/docs", "ref-main")
			gitT(t, lib, "checkout", "-q", "-b", "agent-work")
			heads := map[string]string{}
			for _, co := range []string{pr, lib, docs} {
				dirtyCheckout(t, co)
				heads[co] = strings.TrimSpace(gitOut(t, co, "rev-parse", "HEAD"))
			}
			prBranch := worktree.CurrentBranch(pr)

			writeFile(t, filepath.Join(f.root, worktree.ScratchDir, "notes", "build.log"), "scratch note")
			writeFile(t, filepath.Join(f.root, worktree.ScratchDir, "entity-memory", "ns", "x.md"), "memory")
			writeFile(t, filepath.Join(f.root, worktree.ScratchDir, worktree.CILogsDir, "42", "build.log"), "ci log line")
			const sessionID = "sess-cold"
			sessPath := writeSession(t, f.root, sessionID, `{"type":"summary","sid":"cold"}`)

			f.snapshot(t, sessionID, domain.ConversationRuntimeSDK)
			f.loseRoot(t)
			if err := os.Remove(sessPath); err != nil {
				t.Fatalf("rm session: %v", err)
			}

			conv := f.conv(sessionID)
			got, prov, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, conv, f.restorer(), nil)
			if err != nil {
				t.Fatalf("ensureWorkspace: %v", err)
			}
			if prov != domain.WorkspaceProvenanceRehydrated || got != f.root {
				t.Fatalf("ensureWorkspace = (%q, %q), want %q rehydrated", got, prov, f.root)
			}
			if worktree.IsGitWorktree(got) {
				t.Error("the rebuilt run root is a git checkout; it is a plain folder")
			}
			for _, co := range []string{pr, lib, docs} {
				assertFileContains(t, filepath.Join(co, "committed.txt"), "unpushed commit")
				assertFileContains(t, filepath.Join(co, "README.md"), "uncommitted edit")
				assertFileContains(t, filepath.Join(co, "untracked.txt"), "untracked file")
				if head := strings.TrimSpace(gitOut(t, co, "rev-parse", "HEAD")); head != heads[co] {
					t.Errorf("%s HEAD = %s, want %s", co, head, heads[co])
				}
				if fi, err := os.Lstat(filepath.Join(co, ".git")); err != nil || fi.IsDir() != selfContained {
					t.Errorf("%s/.git is a directory = %v (err %v), want %v", co, fi != nil && fi.IsDir(), err, selfContained)
				}
			}
			if b := worktree.CurrentBranch(pr); b != prBranch {
				t.Errorf("PR checkout branch = %q, want %q", b, prBranch)
			}
			if target := worktree.PushTargetBranch(pr); target != "feature" {
				t.Errorf("PR checkout push target = %q, want the PR head branch", target)
			}
			if b := worktree.CurrentBranch(lib); b != "agent-work" {
				t.Errorf("acme/lib branch = %q, want agent-work", b)
			}
			if err := exec.Command("git", "-C", docs, "symbolic-ref", "-q", "HEAD").Run(); err == nil {
				t.Error("acme/docs came back on a branch; it was detached")
			}

			assertFileContains(t, filepath.Join(got, worktree.ScratchDir, "notes", "build.log"), "scratch note")
			assertMissing(t, filepath.Join(got, worktree.ScratchDir, "entity-memory", "ns", "x.md"))
			assertMissing(t, filepath.Join(got, worktree.ScratchDir, worktree.CILogsDir, "42", "build.log"))
			assertFileContains(t, filepath.Join(got, worktree.ScratchDir, worktree.CILogsDir, ciLogsNoticeFile), "download-logs")
			if !sessionTranscriptExists(got, sessionID) {
				t.Error("the session transcript did not come back; a --resume would fail")
			}

			recorded := map[string]domain.ConversationWorktree{}
			for _, w := range f.ledger.recordedRows() {
				recorded[w.Path] = w
			}
			for co, repo := range map[string]string{pr: "app", lib: "lib", docs: "docs"} {
				if w, ok := recorded[co]; !ok || w.ConversationID != conv.ID {
					t.Errorf("no row recorded for %s as %s's (recorded: %v)", co, conv.ID, f.ledger.recordedRows())
				} else if want := testRepositoryID("acme", repo); w.RepositoryID != want {
					t.Errorf("row recorded for %s names repository %q, want %q", co, w.RepositoryID, want)
				}
			}

			f.snapshot(t, sessionID, domain.ConversationRuntimeSDK)
			members := snapshotMembers(t, f.s.Storage(), snapshotKey(runmode.LocalDefaultOrgID, f.key))
			for _, rel := range []string{"acme/app/pr-7", "acme/lib/default", "acme/docs/ref-main"} {
				if !members[snapCheckoutsPrefix+rel+"/bundle"] || !members[snapCheckoutsPrefix+rel+"/patch"] {
					t.Errorf("the snapshot of the rebuilt tree lost %s's members (members: %v)", rel, members)
				}
			}
		})
	}
}

// TestEnsureWorkspace_FailedCheckoutLeavesNothing: one checkout that cannot be
// rebuilt — here because GitHub could not be reached to read its PR — fails the
// whole restore with that outage as the error, so the hand-back spends the
// upstream budget, and nothing of the attempt is left behind — no root, no
// checkout, no recorded row — for a later claim to take for a warm tree.
func TestEnsureWorkspace_FailedCheckoutLeavesNothing(t *testing.T) {
	f := newSnapshotFixture(t, "task-fail")
	f.addCheckout(t, "acme/lib", "default")
	f.addCheckout(t, "acme/app", "pr-7")
	f.snapshot(t, "", domain.ConversationRuntimeNative)
	f.loseRoot(t)

	restorer := f.restorer()
	restorer.pr = func(context.Context, string, string, int) (*ghclient.PRView, error) {
		return nil, &upstream.TransportError{Err: errors.New("dial tcp: connection refused")}
	}
	_, _, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conv(""), restorer, failingFreshBuilder(t))
	if err == nil {
		t.Fatal("ensureWorkspace succeeded with a checkout that could not be rebuilt")
	}
	if !upstreamSetupFailure(err) {
		t.Errorf("ensureWorkspace error = %v, want the upstream outage that stopped it", err)
	}
	if _, err := os.Stat(f.root); !os.IsNotExist(err) {
		t.Errorf("the run root survived a failed restore (stat err %v)", err)
	}
	if rows := f.ledger.recordedRows(); len(rows) != 0 {
		t.Errorf("a failed restore recorded %v", rows)
	}
}

// TestEnsureWorkspace_UnreadablePRRestoresReadOnly: a PR GitHub refuses to show
// this credential, rather than one it could not be reached for, does not cost
// the workspace. Only push tracking needs the PR, so its checkout comes back
// with all its work and no push remote, the way a deleted head repository
// leaves a fresh one.
func TestEnsureWorkspace_UnreadablePRRestoresReadOnly(t *testing.T) {
	f := newSnapshotFixture(t, "task-unreadable-pr")
	runmode.SetLocalSandboxForTest(t, true)
	pr := f.addCheckout(t, "acme/app", "pr-7")
	dirtyCheckout(t, pr)
	head := strings.TrimSpace(gitOut(t, pr, "rev-parse", "HEAD"))
	f.snapshot(t, "", domain.ConversationRuntimeNative)
	f.loseRoot(t)

	restorer := f.restorer()
	restorer.pr = func(context.Context, string, string, int) (*ghclient.PRView, error) {
		return nil, ghclient.NewHTTPError(http.StatusNotFound, `{"message":"Not Found"}`, "GET /repos/acme/app/pulls/7 returned 404")
	}
	if _, _, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conv(""), restorer, failingFreshBuilder(t)); err != nil {
		t.Fatalf("ensureWorkspace: %v", err)
	}
	assertFileContains(t, filepath.Join(pr, "committed.txt"), "unpushed commit")
	assertFileContains(t, filepath.Join(pr, "README.md"), "uncommitted edit")
	if got := strings.TrimSpace(gitOut(t, pr, "rev-parse", "HEAD")); got != head {
		t.Errorf("PR checkout HEAD = %s, want %s", got, head)
	}
	if remotes := strings.TrimSpace(gitOut(t, pr, "remote")); remotes != "origin" {
		t.Errorf("remotes = %q, want origin alone — no push tracking without the PR", remotes)
	}
}

// TestEnsureWorkspace_FailureAfterCheckoutsTakesThemBack: a rehydrate that
// fails after every checkout is rebuilt — here writing the session transcript
// — removes the root and takes each checkout's push config back out of the
// shared bare, which removing the root alone does not.
func TestEnsureWorkspace_FailureAfterCheckoutsTakesThemBack(t *testing.T) {
	f := newSnapshotFixture(t, "task-late-failure")
	runmode.SetLocalSandboxForTest(t, false)
	f.addCheckout(t, "acme/app", "pr-7")
	const sessionID = "sess-late"
	sessPath := writeSession(t, f.root, sessionID, `{"type":"summary"}`)
	f.snapshot(t, sessionID, domain.ConversationRuntimeSDK)
	f.loseRoot(t)
	bare, err := worktree.RepoDir(testRepositoryID("acme", "app"))
	if err != nil {
		t.Fatal(err)
	}
	// A directory where the transcript goes, so writing it fails.
	if err := os.Remove(sessPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sessPath, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conv(sessionID), f.restorer(), nil); err == nil {
		t.Fatal("ensureWorkspace succeeded with a transcript it could not write")
	}
	if _, err := os.Stat(f.root); !os.IsNotExist(err) {
		t.Errorf("the run root survived a failed restore (stat err %v)", err)
	}
	if remotes := gitOut(t, bare, "remote"); strings.Contains(remotes, "tfpush-") {
		t.Errorf("the failed restore left its push remote in the bare:\n%s", remotes)
	}
}

// TestEnsureWorkspace_RefusedState covers the state an older binary left: a
// blob without the layout version, a blob in the old compression, a warm root
// that is itself a git checkout, and no blob at all. None is converted. A
// native conversation is built a fresh workspace; an SDK resume is refused as
// expired.
func TestEnsureWorkspace_RefusedState(t *testing.T) {
	stage := map[string]func(t *testing.T, f *snapshotFixture){
		"unversioned blob": func(t *testing.T, f *snapshotFixture) {
			putTarBlob(t, f, true, func(tw *tar.Writer) {
				man, _ := json.Marshal(snapshotManifest{SessionID: "sess-old"})
				_ = writeTarBytes(tw, snapManifest, man)
				_ = writeTarBytes(tw, snapScratchPrefix+"notes.txt", []byte("old layout"))
			})
			f.loseRoot(t)
		},
		"gzip blob": func(t *testing.T, f *snapshotFixture) {
			putTarBlob(t, f, false, func(tw *tar.Writer) {
				_ = writeTarBytes(tw, snapScratchPrefix+"notes.txt", []byte("old layout"))
			})
			f.loseRoot(t)
		},
		"warm root that is a checkout": func(t *testing.T, f *snapshotFixture) {
			gitT(t, f.root, "init", "-q")
		},
		"no blob": func(t *testing.T, f *snapshotFixture) {
			f.loseRoot(t)
		},
	}
	for name, setup := range stage {
		t.Run(name, func(t *testing.T) {
			for _, runtime := range []string{domain.ConversationRuntimeNative, domain.ConversationRuntimeSDK} {
				f := newSnapshotFixture(t, "task-refused-"+runtime)
				setup(t, f)
				conv := f.conv("")
				conv.Runtime = runtime
				built := false
				fresh := func(context.Context) (string, error) {
					built = true
					if _, err := os.Stat(filepath.Join(f.root, ".git")); !os.IsNotExist(err) {
						t.Errorf("the fresh build found the older tree still in place (stat err %v)", err)
					}
					return worktree.MakeRunRoot(f.key)
				}
				got, prov, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, conv, f.restorer(), fresh)
				if runtime == domain.ConversationRuntimeSDK {
					if !errors.Is(err, ErrWorkspaceExpired) {
						t.Errorf("SDK resume: err = %v, want ErrWorkspaceExpired", err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("native: ensureWorkspace: %v", err)
				}
				if !built || prov != domain.WorkspaceProvenanceFresh || got != f.root {
					t.Errorf("native: ensureWorkspace = (%q, %q, built=%v), want a fresh %q", got, prov, built, f.root)
				}
				if _, err := os.Stat(filepath.Join(got, worktree.ScratchDir, "notes.txt")); !os.IsNotExist(err) {
					t.Errorf("native: the older blob's scratch was restored (stat err %v)", err)
				}
			}
		})
	}
}

// TestSnapshotWorkspace_RowFromAnotherRootStillCarriesTheCheckout: a row names
// a checkout by repo and slug. One recorded under another root — on another
// host, or before a restore rebuilt the root here — still carries the checkout
// that sits at that place under this root.
func TestSnapshotWorkspace_RowFromAnotherRootStillCarriesTheCheckout(t *testing.T) {
	f := newSnapshotFixture(t, "task-row-elsewhere")
	lib := f.addCheckout(t, "acme/lib", "default")
	dirtyCheckout(t, lib)
	f.ledger.mu.Lock()
	f.ledger.rows[0].Path = filepath.Join("/elsewhere", "triagefactory-runs", f.key, "acme", "lib", "default")
	f.ledger.mu.Unlock()

	f.snapshot(t, "", domain.ConversationRuntimeNative)
	members := snapshotMembers(t, f.s.Storage(), snapshotKey(runmode.LocalDefaultOrgID, f.key))
	if !members[snapCheckoutsPrefix+"acme/lib/default/bundle"] {
		t.Errorf("the snapshot dropped a checkout whose row names another root (members: %v)", members)
	}
}

// TestSetupGitHub_FailsWithoutItsCheckoutRow: the PR checkout's row is how
// every later snapshot finds the checkout, so a setup that cannot write it
// fails instead of starting an agent whose work no snapshot would carry — and
// fails before the clone and the worktree_path stamp, so the next claim finds
// no tree to take for a warm one and runs the setup again.
func TestSetupGitHub_FailsWithoutItsCheckoutRow(t *testing.T) {
	f := newSnapshotFixture(t, "task-setup-row")
	srv := f.prServer(t, nil)
	entityID, _ := f.taskRegistry(t, dbtest.TestGitHubHost, dbtest.TestGitHubHost)
	conversations := &setupConversations{}
	f.s.conversations = conversations
	f.s.conversationWorktrees = refusingLedger{}

	task := domain.Task{ID: f.key, EntityID: entityID, EntitySource: "github", EntitySourceID: "acme/app#7"}
	_, err := f.s.setupGitHub(context.Background(), runmode.LocalDefaultOrgID, fixtureConversation, "claim-1", f.key, "user-1", task, ghclient.NewProxyClient(srv.URL, "placeholder"), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "conversation_worktrees") {
		t.Fatalf("setupGitHub = %v, want the failed row write", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "acme", "app", "pr-7")); !os.IsNotExist(err) {
		t.Errorf("the failed setup cloned its checkout anyway (stat err %v)", err)
	}
	if conversations.stamped != 0 {
		t.Errorf("the failed setup stamped worktree_path %d times; the next claim would take the root for a warm tree", conversations.stamped)
	}
}

// setupConversations accepts the phase and worktree_path writes a setup makes,
// counting the stamps.
type setupConversations struct {
	db.ConversationStore
	stamped int
}

func (*setupConversations) SetClaimPhaseSystem(context.Context, string, string, string, string) (*domain.ExecutorClaim, error) {
	return nil, nil
}

func (c *setupConversations) SetWorktreePathForClaimSystem(context.Context, string, string, string, string) (*domain.Conversation, error) {
	c.stamped++
	return nil, nil
}

// refusingLedger fails every conversation_worktrees write.
type refusingLedger struct{ db.ConversationWorktreeStore }

func (refusingLedger) RecordForClaimSystem(context.Context, string, string, domain.ConversationWorktree) (domain.ConversationWorktree, error) {
	return domain.ConversationWorktree{}, errors.New("database is locked")
}

// TestSetupGitHub_ResolvesTheTaskRepositoryOnTheEntitysHost: the repository a
// PR task's setup clones and records is the row for its owner/repo on the
// GitHub host its pull request was polled from — the entity's scope — not on
// the host the org names now. Both hosts hold an acme/app row here, and the
// org's current host is always the other one, so a setup that read the org's
// host would key the ledger row and the bare by the wrong repository.
func TestSetupGitHub_ResolvesTheTaskRepositoryOnTheEntitysHost(t *testing.T) {
	const ghe = "https://github.corp.example.com"
	for _, tc := range []struct{ entityHost, orgHost string }{
		{entityHost: dbtest.TestGitHubHost, orgHost: ghe},
		{entityHost: ghe, orgHost: dbtest.TestGitHubHost},
	} {
		t.Run("entity on "+tc.entityHost, func(t *testing.T) {
			f := newSnapshotFixture(t, "task-setup-host")
			srv := f.prServer(t, nil)
			entityID, rows := f.taskRegistry(t, tc.entityHost, dbtest.TestGitHubHost, ghe)
			dbtest.SeedOrgSettings(t, f.s.orgs, runmode.LocalDefaultOrgID, domain.OrgSettings{GitHubBaseURL: tc.orgHost})
			f.s.conversations = &setupConversations{}

			task := domain.Task{ID: f.key, EntityID: entityID, EntitySource: "github", EntitySourceID: "acme/app#7"}
			cfg, err := f.s.setupGitHub(context.Background(), runmode.LocalDefaultOrgID, fixtureConversation, "claim-1", f.key, "user-1", task, ghclient.NewProxyClient(srv.URL, "placeholder"), nil, nil)
			if err != nil {
				t.Fatalf("setupGitHub: %v", err)
			}

			want, other := rows[tc.entityHost], rows[tc.orgHost]
			recorded := f.ledger.recordedRows()
			if len(recorded) != 1 {
				t.Fatalf("setup recorded %d ledger rows, want 1: %+v", len(recorded), recorded)
			}
			if got := recorded[0]; got.RepositoryID != want.ID || got.Path != cfg.prCheckout || got.Ref != worktree.PRRefSlug(7) {
				t.Errorf("ledger row = {repository:%q path:%q ref:%q}, want {repository:%q (acme/app on %s) path:%q ref:%q}",
					got.RepositoryID, got.Path, got.Ref, want.ID, tc.entityHost, cfg.prCheckout, worktree.PRRefSlug(7))
			}
			bare, err := worktree.RepoDir(want.ID)
			if err != nil {
				t.Fatalf("RepoDir: %v", err)
			}
			if _, err := os.Stat(bare); err != nil {
				t.Errorf("no bare under the entity host's repository %s: %v", want.ID, err)
			}
			if otherBare, err := worktree.RepoDir(other.ID); err == nil {
				if _, err := os.Stat(otherBare); !os.IsNotExist(err) {
					t.Errorf("setup built a bare under the org host's repository %s (stat err %v)", other.ID, err)
				}
			}
		})
	}
}

// TestSetupGitHub_NoRepositoryOnTheEntitysHostFailsBeforeTheFetch: a PR task
// whose owner/repo has no row on its entity's host has no repository to key a
// ledger row or a bare by. A same-named row on the org's current host is a
// different repository and is not borrowed; the setup fails before it reads
// the pull request or records anything.
func TestSetupGitHub_NoRepositoryOnTheEntitysHostFailsBeforeTheFetch(t *testing.T) {
	const ghe = "https://github.corp.example.com"
	f := newSnapshotFixture(t, "task-setup-norow")
	var fetches atomic.Int32
	srv := f.prServer(t, &fetches)
	entityID, _ := f.taskRegistry(t, ghe, dbtest.TestGitHubHost)
	f.s.conversations = &setupConversations{}

	task := domain.Task{ID: f.key, EntityID: entityID, EntitySource: "github", EntitySourceID: "acme/app#7"}
	_, err := f.s.setupGitHub(context.Background(), runmode.LocalDefaultOrgID, fixtureConversation, "claim-1", f.key, "user-1", task, ghclient.NewProxyClient(srv.URL, "placeholder"), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "acme/app") || !strings.Contains(err.Error(), ghe) {
		t.Fatalf("setupGitHub = %v, want an error naming acme/app on %s", err, ghe)
	}
	if n := fetches.Load(); n != 0 {
		t.Errorf("setup made %d GitHub requests before failing; it resolves the repository first", n)
	}
	if rows := f.ledger.recordedRows(); len(rows) != 0 {
		t.Errorf("setup recorded %v without a repository", rows)
	}
}

// TestSnapshotWorkspace_UnstattableCheckoutFailsThePersist: a checkout the
// capture cannot stat for any reason but its absence fails the snapshot, and
// the blob already under the key, which carries that checkout, stands. A blob
// written without it would replace the last one that had its work.
func TestSnapshotWorkspace_UnstattableCheckoutFailsThePersist(t *testing.T) {
	f := newSnapshotFixture(t, "task-unstattable")
	co := f.addCheckout(t, "acme/app", "default")
	dirtyCheckout(t, co)
	f.snapshot(t, "", domain.ConversationRuntimeNative)

	// A symlink loop where the owner directory was. Resolving the checkout's
	// path fails with ELOOP, which stands in for the permission error a test
	// running as root cannot produce.
	owner := filepath.Join(f.root, "acme")
	if err := os.Rename(owner, owner+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("acme", owner); err != nil {
		t.Fatal(err)
	}
	if err := f.s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, fixtureConversation, f.key, "", f.root, "", domain.ConversationRuntimeNative); err == nil {
		t.Fatal("snapshotWorkspace succeeded with a checkout it could not stat")
	}
	if !snapshotMembers(t, f.s.Storage(), snapshotKey(runmode.LocalDefaultOrgID, f.key))[snapCheckoutsPrefix+"acme/app/default/bundle"] {
		t.Error("the blob carrying the checkout was replaced by one without it")
	}
}

// TestSnapshotWorkspace_StalledCheckoutReadEndsWithTheBound: the read of the
// task's checkouts is part of the capture and takes the persist's bound, so a
// database that stops answering fails the snapshot when the bound runs out
// instead of holding a park or a conclusion open.
func TestSnapshotWorkspace_StalledCheckoutReadEndsWithTheBound(t *testing.T) {
	f := newSnapshotFixture(t, "task-stalled")
	f.s.conversationWorktrees = stalledLedger{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- f.s.snapshotWorkspace(ctx, runmode.LocalDefaultOrgID, fixtureConversation, f.key, "", f.root, "", domain.ConversationRuntimeNative)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("snapshotWorkspace succeeded without the task's checkouts")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("snapshotWorkspace outlived its bound waiting on the checkout read")
	}
}

// stalledLedger answers the task's checkout read only when its context ends, as
// a database that stopped answering does.
type stalledLedger struct{ db.ConversationWorktreeStore }

func (stalledLedger) ListForTaskSystem(ctx context.Context, _, _ string) ([]domain.ConversationWorktree, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestFreshRunRoot_FailsWhenTheOlderTreeStays: a setup building from nothing
// removes a run root an older binary laid out, and when that removal fails the
// setup fails with it rather than building on the older tree.
func TestFreshRunRoot_FailsWhenTheOlderTreeStays(t *testing.T) {
	isolateRunNamespace(t)
	setupGitTestEnv(t)
	const key = "task-older-root"
	root, err := worktree.MakeRunRoot(key)
	if err != nil {
		t.Fatalf("MakeRunRoot: %v", err)
	}
	t.Cleanup(func() { worktree.RemoveRunRoot(key) })
	gitT(t, root, "init", "-q")

	restoreRemoveSeam(t, func(string, string) error { return errors.New("removal refused") })
	if _, err := freshRunRoot(key); err == nil {
		t.Fatal("freshRunRoot succeeded with the older tree still in place")
	}

	restoreRemoveSeam(t, worktree.RemoveAt)
	got, err := freshRunRoot(key)
	if err != nil {
		t.Fatalf("freshRunRoot: %v", err)
	}
	if worktree.IsGitWorktree(got) {
		t.Error("the fresh run root is still a git checkout")
	}
}

// TestSnapshotWorkspace_BlobFormat: the stored blob is zstd, its first member
// is the manifest with this layout's version and the checkouts it carries —
// which is what lets the layout probe answer from one short read — and
// extracted CI logs, which are one download away, contribute nothing to it.
func TestSnapshotWorkspace_BlobFormat(t *testing.T) {
	f := newSnapshotFixture(t, "task-format")
	f.addCheckout(t, "acme/app", "default")
	logs := strings.Repeat("2026-08-20T12:00:00.0000000Z ##[group]Run go test ./...\n", 60_000)
	writeFile(t, filepath.Join(f.root, worktree.ScratchDir, worktree.CILogsDir, "42", "1_build.txt"), logs)
	writeFile(t, filepath.Join(f.root, worktree.ScratchDir, "notes", "keep.txt"), "the agent's own intermediate")
	f.snapshot(t, "", domain.ConversationRuntimeNative)

	key := snapshotKey(runmode.LocalDefaultOrgID, f.key)
	rc, err := f.s.Storage().Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	blob, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !bytes.HasPrefix(blob, zstdMagic) {
		t.Errorf("blob starts with % x, want zstd", blob[:4])
	}
	if len(blob) > 16<<10 {
		t.Errorf("blob = %d bytes for a tree whose only bulk is %d bytes of CI logs", len(blob), len(logs))
	}

	zr, err := zstd.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	hdr, err := tr.Next()
	if err != nil || hdr.Name != snapManifest {
		t.Fatalf("first member = %v (err %v), want the manifest", hdr, err)
	}
	var man snapshotManifest
	if err := json.NewDecoder(tr).Decode(&man); err != nil {
		t.Fatal(err)
	}
	want := manifestCheckout{RepositoryID: testRepositoryID("acme", "app"), RepoID: "acme/app", Slug: "default", Path: "acme/app/default"}
	if man.LayoutVersion != snapshotLayoutVersion || len(man.Checkouts) != 1 ||
		man.Checkouts[0].RepositoryID != want.RepositoryID ||
		man.Checkouts[0].RepoID != want.RepoID || man.Checkouts[0].Slug != want.Slug || man.Checkouts[0].Path != want.Path || man.Checkouts[0].Head == "" {
		t.Errorf("manifest = %+v, want layout %d carrying %+v", man, snapshotLayoutVersion, want)
	}
	if !man.CILogsOmitted {
		t.Error("manifest does not record the omitted CI logs; a restore could not explain their absence")
	}
	members := snapshotMembers(t, f.s.Storage(), key)
	for name := range members {
		if strings.HasPrefix(name, snapScratchPrefix+worktree.CILogsDir+"/") {
			t.Errorf("blob carries %q", name)
		}
	}
	if !members[snapScratchPrefix+"notes/keep.txt"] {
		t.Errorf("blob dropped the agent's own scratch (members: %v)", members)
	}

	if p, err := f.s.snapshotLayoutAt(context.Background(), runmode.LocalDefaultOrgID, f.key); err != nil || p != snapshotRestorable {
		t.Errorf("snapshotLayoutAt = %v, %v; want restorable", p, err)
	}
	if p, err := f.s.snapshotLayoutAt(context.Background(), runmode.LocalDefaultOrgID, "no-such-key"); err != nil || p != snapshotAbsent {
		t.Errorf("snapshotLayoutAt(missing) = %v, %v; want absent", p, err)
	}
}

// TestEnsureWorkspace_NoCILogsNoticeWithoutOmittedLogs: the notice explains a
// specific absence, so a tree whose ci-logs held nothing gets none.
func TestEnsureWorkspace_NoCILogsNoticeWithoutOmittedLogs(t *testing.T) {
	f := newSnapshotFixture(t, "task-no-cilogs")
	writeFile(t, filepath.Join(f.root, worktree.ScratchDir, "notes.txt"), "scratch note")
	if err := os.MkdirAll(filepath.Join(f.root, worktree.ScratchDir, worktree.CILogsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	f.snapshot(t, "", domain.ConversationRuntimeSDK)
	f.loseRoot(t)

	got, _, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conv(""), f.restorer(), nil)
	if err != nil {
		t.Fatalf("ensureWorkspace: %v", err)
	}
	assertFileContains(t, filepath.Join(got, worktree.ScratchDir, "notes.txt"), "scratch note")
	assertMissing(t, filepath.Join(got, worktree.ScratchDir, worktree.CILogsDir))
}

// TestEnsureWorkspace_TranscriptGuard: a snapshot that captured the session
// transcript restores one the resume guard sees; one taken while the
// transcript was missing rebuilds the tree but leaves the guard to report the
// run unresumable, rather than hand the SDK a --resume it cannot honor.
func TestEnsureWorkspace_TranscriptGuard(t *testing.T) {
	for _, withTranscript := range []bool{true, false} {
		f := newSnapshotFixture(t, "task-transcript")
		const sessionID = "sess-guard"
		writeFile(t, filepath.Join(f.root, worktree.ScratchDir, "notes.txt"), "scratch survived")
		if withTranscript {
			writeSession(t, f.root, sessionID, `{"type":"summary"}`)
		}
		f.snapshot(t, sessionID, domain.ConversationRuntimeSDK)
		f.loseRoot(t)

		got, _, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conv(sessionID), f.restorer(), nil)
		if err != nil {
			t.Fatalf("ensureWorkspace: %v", err)
		}
		assertFileContains(t, filepath.Join(got, worktree.ScratchDir, "notes.txt"), "scratch survived")
		if sessionTranscriptExists(got, sessionID) != withTranscript {
			t.Errorf("transcript captured = %v, but the guard reads %v", withTranscript, !withTranscript)
		}
	}
}

// TestEnsureWorkspace_TruncatedZstdErrors: a blob whose zstd checksum is cut
// short fails the rehydrate before the run root is touched. The tar ends before
// the zstd footer, so only the drain after the last member catches it.
func TestEnsureWorkspace_TruncatedZstdErrors(t *testing.T) {
	f := newSnapshotFixture(t, "task-corrupt")
	writeFile(t, filepath.Join(f.root, worktree.ScratchDir, "x.log"), "scratch bytes the zstd frame checksum covers")
	f.snapshot(t, "", domain.ConversationRuntimeSDK)
	f.loseRoot(t)

	key := snapshotKey(runmode.LocalDefaultOrgID, f.key)
	rc, err := f.s.Storage().Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := io.ReadAll(rc)
	_ = rc.Close()
	if err := f.s.Storage().Put(context.Background(), key, bytes.NewReader(blob[:len(blob)-1])); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conv(""), f.restorer(), nil); err == nil {
		t.Fatal("ensureWorkspace accepted a truncated zstd checksum")
	}
	if _, err := os.Stat(f.root); !os.IsNotExist(err) {
		t.Errorf("the run root was written before the integrity check (stat err %v)", err)
	}
}

// TestWriteSnapshotTar_StreamsStagedCaptureMembers: members the capture child
// staged on disk are streamed into the tar under the checkout's member names.
func TestWriteSnapshotTar_StreamsStagedCaptureMembers(t *testing.T) {
	staging := t.TempDir()
	bundlePath := filepath.Join(staging, worktree.CaptureBundleFile)
	patchPath := filepath.Join(staging, worktree.CapturePatchFile)
	transcriptPath := filepath.Join(staging, worktree.CaptureTranscriptFile)
	writeFile(t, bundlePath, "bundle-bytes")
	writeFile(t, patchPath, "patch-bytes")
	writeFile(t, transcriptPath, "transcript-bytes")

	captured := &capturedSnapshot{
		state: worktree.CapturedState{SessionID: "sess-staged", TranscriptPath: transcriptPath},
		checkouts: []capturedCheckout{{
			snapshotCheckout: snapshotCheckout{repoID: "acme/app", slug: "pr-7", rel: "acme/app/pr-7"},
			state: worktree.CapturedState{
				Delta:      &worktree.GitDelta{Branch: "aa/work", Head: "abc123"},
				BundlePath: bundlePath, PatchPath: patchPath,
			},
		}},
	}
	var blob bytes.Buffer
	if err := writeSnapshotTar(context.Background(), &blob, captured, t.TempDir(), snapshotManifest{}); err != nil {
		t.Fatalf("writeSnapshotTar: %v", err)
	}
	want := map[string]string{
		snapCheckoutsPrefix + "acme/app/pr-7/bundle": "bundle-bytes",
		snapCheckoutsPrefix + "acme/app/pr-7/patch":  "patch-bytes",
		snapSession: "transcript-bytes",
	}
	tr := tar.NewReader(&blob)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if body, ok := want[hdr.Name]; ok {
			if got, _ := io.ReadAll(tr); string(got) != body {
				t.Errorf("%s = %q, want %q", hdr.Name, got, body)
			}
			delete(want, hdr.Name)
		}
	}
	if len(want) != 0 {
		t.Errorf("snapshot omitted staged members: %v", want)
	}
}

func TestCapturedBytesSize_RejectsStagedSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "member")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := capturedBytesSize(nil, link); err == nil {
		t.Fatal("capturedBytesSize accepted a staged symlink")
	}
}

// TestFailRun_LeavesTheWorkspaceSnapshotToItsOwner: a failure does not delete
// the task's workspace blob. The blueprint's teardown owns that key, and a
// delete from the conversation's failure would take the workspace out from
// under work the failure has not ended.
func TestFailRun_LeavesTheWorkspaceSnapshotToItsOwner(t *testing.T) {
	isolateRunNamespace(t)
	s, database, conversationID, taskID := setupAdvanceFixture(t, "failrun-discard")
	wireBlobStore(t, s)

	ctx := context.Background()
	key := snapshotKey(runmode.LocalDefaultOrgID, taskIDForConversation(t, database, conversationID))
	if err := s.Storage().Put(ctx, key, strings.NewReader("snapshot")); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	s.failConversation(runmode.LocalDefaultOrgID, conversationID, taskID, holderClaimFor(t, s, runmode.LocalDefaultOrgID, conversationID), "event", "boom", domain.ConversationFailureUnclassified)

	if ok, _ := s.Storage().Exists(ctx, key); !ok {
		t.Error("failConversation deleted the task's workspace snapshot; terminateBlueprint owns that blob")
	}
}

// TestSnapshotWorkspace_PhaseSpans pins the span family a slow park is read
// through: the punctual workspace.snapshot root split into capture / archive /
// put children, each carrying the sizes that explain its own duration and the
// runtime whose snapshot it was.
func TestSnapshotWorkspace_PhaseSpans(t *testing.T) {
	read := recordSpans(t)
	f := newSnapshotFixture(t, "task-spans")
	co := f.addCheckout(t, "acme/app", "default")
	dirtyCheckout(t, co)
	writeFile(t, filepath.Join(f.root, worktree.ScratchDir, "notes", "build.log"), strings.Repeat("scratch note\n", 200))
	const sessionID = "sess-spans"
	writeSession(t, f.root, sessionID, `{"type":"summary","sid":"spans"}`)
	f.snapshot(t, sessionID, domain.ConversationRuntimeSDK)

	spans := read()
	roots := spansNamed(spans, "workspace.snapshot")
	if len(roots) != 1 {
		t.Fatalf("workspace.snapshot spans = %d, want 1", len(roots))
	}
	root := roots[0]
	total := spanAttr(t, root, "size_bytes").AsInt64()
	if total <= 0 {
		t.Errorf("root size_bytes = %d, want > 0", total)
	}
	for _, name := range []string{"workspace.snapshot.capture", "workspace.snapshot.archive", "workspace.snapshot.put"} {
		phases := spansNamed(spans, name)
		if len(phases) != 1 {
			t.Fatalf("%s spans = %d, want 1", name, len(phases))
		}
		if phases[0].Parent().SpanID() != root.SpanContext().SpanID() {
			t.Errorf("%s does not parent to the workspace.snapshot span", name)
		}
		if got := spanAttr(t, phases[0], "runtime").AsString(); got != domain.ConversationRuntimeSDK {
			t.Errorf("%s runtime = %q, want %q", name, got, domain.ConversationRuntimeSDK)
		}
	}
	capture := spansNamed(spans, "workspace.snapshot.capture")[0]
	for _, key := range []string{"snapshot.bundle_bytes", "snapshot.patch_bytes", "snapshot.transcript_bytes"} {
		if got := spanAttr(t, capture, key).AsInt64(); got <= 0 {
			t.Errorf("capture %s = %d, want > 0 for a tree carrying that member", key, got)
		}
	}
	if got := spanAttr(t, capture, "count").AsInt64(); got != 1 {
		t.Errorf("capture count = %d, want the one checkout", got)
	}
	archive := spansNamed(spans, "workspace.snapshot.archive")[0]
	raw := spanAttr(t, archive, "snapshot.raw_bytes").AsInt64()
	compressed := spanAttr(t, archive, "size_bytes").AsInt64()
	if raw <= compressed || compressed != total {
		t.Errorf("archive raw=%d compressed=%d root=%d; want raw > compressed == root", raw, compressed, total)
	}
	if got := spanAttr(t, spansNamed(spans, "workspace.snapshot.put")[0], "size_bytes").AsInt64(); got != compressed {
		t.Errorf("put size_bytes = %d, want the staged blob's %d", got, compressed)
	}
}

// TestSnapshotWorkspace_PhaseSpans_NoCheckoutsNoSession: a native conversation
// with no checkouts has no delta and no transcript, and those sizes report an
// explicit zero rather than vanishing.
func TestSnapshotWorkspace_PhaseSpans_NoCheckoutsNoSession(t *testing.T) {
	read := recordSpans(t)
	f := newSnapshotFixture(t, "task-spans-native")
	writeFile(t, filepath.Join(f.root, worktree.ScratchDir, "notes.txt"), "scratch note")
	f.snapshot(t, "", domain.ConversationRuntimeNative)

	capture := spansNamed(read(), "workspace.snapshot.capture")
	if len(capture) != 1 {
		t.Fatalf("workspace.snapshot.capture spans = %d, want 1", len(capture))
	}
	for _, key := range []string{"snapshot.bundle_bytes", "snapshot.patch_bytes", "snapshot.transcript_bytes", "count"} {
		if got := spanAttr(t, capture[0], key).AsInt64(); got != 0 {
			t.Errorf("capture %s = %d, want an explicit 0", key, got)
		}
	}
}

// TestEnsureWorkspace_ColdRehydrate_RebuildsAgainstTheRecordedRepository: a
// snapshot records each checkout's repository row id beside its owner/repo,
// and a restore rebuilds against that id — the seed is resolved for it, the
// rebuild git is handed carries it (it keys the bare), and the row recorded
// for the restoring conversation names it — without resolving any name.
func TestEnsureWorkspace_ColdRehydrate_RebuildsAgainstTheRecordedRepository(t *testing.T) {
	f := newSnapshotFixture(t, "task-restore-id")
	app := f.addCheckoutOf(t, worktree.Repo{ID: "row-app", Owner: "acme", Name: "app"}, "default")
	lib := f.addCheckoutOf(t, worktree.Repo{ID: "row-lib", Owner: "acme", Name: "lib"}, "ref-main")
	dirtyCheckout(t, app)
	f.snapshot(t, "", domain.ConversationRuntimeNative)
	f.loseRoot(t)

	restorer := f.restorer()
	// The two checkouts restore concurrently, so both hooks record under mu.
	var (
		mu       sync.Mutex
		seeded   []string
		restored []worktree.CheckoutRestore
	)
	seed := restorer.seed
	restorer.seed = func(ctx context.Context, repositoryID, owner, repo string) gitSeed {
		mu.Lock()
		seeded = append(seeded, repositoryID+" "+owner+"/"+repo)
		mu.Unlock()
		return seed(ctx, repositoryID, owner, repo)
	}
	restorer.repository = func(_ context.Context, owner, repo string) (string, error) {
		t.Errorf("resolved %s/%s by name; the manifest records its repository", owner, repo)
		return "", errors.New("no name resolution in this test")
	}
	restore := restoreCheckout
	restoreCheckout = func(ctx context.Context, r worktree.CheckoutRestore) (worktree.RestoredCheckout, error) {
		mu.Lock()
		restored = append(restored, r)
		mu.Unlock()
		return restore(ctx, r)
	}
	t.Cleanup(func() { restoreCheckout = restore })

	if _, _, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conv(""), restorer, failingFreshBuilder(t)); err != nil {
		t.Fatalf("ensureWorkspace: %v", err)
	}
	assertFileContains(t, filepath.Join(app, "committed.txt"), "unpushed commit")

	wantByPath := map[string]string{app: "row-app", lib: "row-lib"}
	if len(restored) != 2 {
		t.Fatalf("restore rebuilt %d checkouts, want 2", len(restored))
	}
	for _, r := range restored {
		path := filepath.Join(r.Root, r.Owner, r.Repo, r.Slug)
		if want := wantByPath[path]; r.RepositoryID != want {
			t.Errorf("rebuild of %s carries repository %q, want %q", path, r.RepositoryID, want)
		}
	}
	slices.Sort(seeded)
	if want := []string{"row-app acme/app", "row-lib acme/lib"}; !slices.Equal(seeded, want) {
		t.Errorf("seeds resolved for %v, want %v", seeded, want)
	}
	recorded := f.ledger.recordedRows()
	if len(recorded) != 2 {
		t.Fatalf("restore recorded %d rows, want 2: %+v", len(recorded), recorded)
	}
	for _, w := range recorded {
		if want := wantByPath[w.Path]; w.RepositoryID != want || w.ConversationID != fixtureConversation {
			t.Errorf("recorded row %+v, want repository %q for %s", w, want, fixtureConversation)
		}
	}
}

// TestEnsureWorkspace_ColdRehydrate_LegacyManifestResolvesOnTheEntitysHost: a
// manifest written before checkouts recorded their repository row names each
// by owner/repo alone. Its restore resolves that name on the GitHub host of
// the task's own entity, through the restorer a claim builds, so with acme/app
// rows on two hosts the one on the entity's scope is rebuilt and recorded —
// whatever host the org names now.
func TestEnsureWorkspace_ColdRehydrate_LegacyManifestResolvesOnTheEntitysHost(t *testing.T) {
	const ghe = "https://github.corp.example.com"
	f := newSnapshotFixture(t, "task-restore-legacy")
	co := f.addCheckout(t, "acme/app", "default")
	dirtyCheckout(t, co)
	f.writeLegacySnapshot(t)
	f.loseRoot(t)

	entityID, rows := f.taskRegistry(t, ghe, dbtest.TestGitHubHost, ghe)
	want := rows[ghe]

	var restored []worktree.CheckoutRestore
	restore := restoreCheckout
	restoreCheckout = func(ctx context.Context, r worktree.CheckoutRestore) (worktree.RestoredCheckout, error) {
		restored = append(restored, r)
		return restore(ctx, r)
	}
	t.Cleanup(func() { restoreCheckout = restore })

	restorer := f.s.checkoutRestorerFor(runmode.LocalDefaultOrgID, entityID, nil, nil)
	if _, _, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conv(""), restorer, failingFreshBuilder(t)); err != nil {
		t.Fatalf("ensureWorkspace: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("restore rebuilt %d checkouts, want 1", len(restored))
	}
	if r := restored[0]; r.RepositoryID != want.ID || r.CloneURL != want.CloneURL {
		t.Errorf("rebuild = {repository:%q clone:%q}, want acme/app on %s {repository:%q clone:%q}", r.RepositoryID, r.CloneURL, ghe, want.ID, want.CloneURL)
	}
	assertFileContains(t, filepath.Join(co, "committed.txt"), "unpushed commit")
	recorded := f.ledger.recordedRows()
	if len(recorded) != 1 || recorded[0].RepositoryID != want.ID || recorded[0].Path != co {
		t.Errorf("recorded rows = %+v, want one for %s naming repository %s", recorded, co, want.ID)
	}
}

// TestEnsureWorkspace_ColdRehydrate_LegacyManifestWithNoRowOnTheEntitysHost: a
// legacy manifest whose owner/repo has no row on the entity's host has nothing
// to rebuild against. A row of that name on another host is another
// repository, so the restore fails, naming the checkout, and leaves nothing
// behind.
func TestEnsureWorkspace_ColdRehydrate_LegacyManifestWithNoRowOnTheEntitysHost(t *testing.T) {
	const ghe = "https://github.corp.example.com"
	f := newSnapshotFixture(t, "task-restore-legacy-norow")
	f.addCheckout(t, "acme/app", "default")
	f.writeLegacySnapshot(t)
	f.loseRoot(t)

	entityID, _ := f.taskRegistry(t, ghe, dbtest.TestGitHubHost)
	restorer := f.s.checkoutRestorerFor(runmode.LocalDefaultOrgID, entityID, nil, nil)
	_, _, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conv(""), restorer, failingFreshBuilder(t))
	if err == nil {
		t.Fatal("ensureWorkspace restored a checkout whose repository has no row on the entity's host")
	}
	if !strings.Contains(err.Error(), "acme/app/default") {
		t.Errorf("error = %v, want it to name the checkout acme/app/default", err)
	}
	if _, err := os.Stat(f.root); !os.IsNotExist(err) {
		t.Errorf("the run root survived a failed restore (stat err %v)", err)
	}
	if rows := f.ledger.recordedRows(); len(rows) != 0 {
		t.Errorf("a failed restore recorded %v", rows)
	}
}

// --- fixture ---------------------------------------------------------------

// snapshotFixture is a task's run root with real checkouts beneath it, on a
// spawner with a blob store and an in-memory checkout ledger: what a snapshot
// reads and a restore rebuilds, with no database.
type snapshotFixture struct {
	s      *Spawner
	ledger *checkoutLedger
	key    string
	root   string
	// upstreams maps "owner/repo" to the origin its checkouts were cloned from.
	upstreams map[string]string
}

// fixtureConversation is the conversation every fixture checkout belongs to.
const fixtureConversation = "conv-1"

func newSnapshotFixture(t *testing.T, key string) *snapshotFixture {
	t.Helper()
	isolateRunNamespace(t)
	setupGitTestEnv(t)
	s := newStorageSpawner(t)
	root, err := worktree.MakeRunRoot(key)
	if err != nil {
		t.Fatalf("MakeRunRoot: %v", err)
	}
	t.Cleanup(func() { worktree.RemoveRunRoot(key) })
	return &snapshotFixture{s: s, ledger: s.conversationWorktrees.(*checkoutLedger), key: key, root: root, upstreams: map[string]string{}}
}

// addCheckout builds a checkout of repoID under the root the way the run
// would: slug pr-7 as setup builds a PR run's checkout, any other slug as
// `workspace add` does. The repository is testRepo's for the slug. It records
// the checkout's row as the store reads one back, carrying both the row id and
// the slug, and returns its path.
func (f *snapshotFixture) addCheckout(t *testing.T, repoID, slug string) string {
	t.Helper()
	owner, repo := parseOwnerRepo(repoID)
	return f.addCheckoutOf(t, testRepo(owner, repo), slug)
}

// addCheckoutOf is addCheckout for a named repository: r.ID keys its bare and
// its ledger row.
func (f *snapshotFixture) addCheckoutOf(t *testing.T, r worktree.Repo, slug string) string {
	t.Helper()
	origin := f.upstream(t, r.Slug())
	var (
		path string
		err  error
	)
	switch slug {
	case "pr-7":
		path, err = worktree.CreateForPRInRoot(context.Background(), r, origin, origin, "feature", 7, fixtureConversation, f.root)
	case "default":
		path, err = worktree.CreateForCheckoutInRoot(context.Background(), r, origin, "", f.key, f.root)
	default:
		path, err = worktree.CreateForCheckoutInRoot(context.Background(), r, origin, strings.TrimPrefix(slug, "ref-"), f.key, f.root)
	}
	if err != nil {
		t.Fatalf("build %s %s: %v", r.Slug(), slug, err)
	}
	f.ledger.add(domain.ConversationWorktree{ConversationID: fixtureConversation, RepositoryID: r.ID, RepoID: r.Slug(), Path: path, Ref: slug})
	return path
}

// upstream is repoID's origin: one commit on main, and pull request #7 whose
// head is the branch feature.
func (f *snapshotFixture) upstream(t *testing.T, repoID string) string {
	t.Helper()
	if origin, ok := f.upstreams[repoID]; ok {
		return origin
	}
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitT(t, "", "init", "-q", "--bare", "-b", "main", origin)
	seed := filepath.Join(t.TempDir(), "seed")
	gitT(t, "", "init", "-q", "-b", "main", seed)
	writeFile(t, filepath.Join(seed, "README.md"), "hello\n")
	gitT(t, seed, "add", "README.md")
	gitT(t, seed, "commit", "-q", "-m", "init")
	gitT(t, seed, "push", "-q", origin, "main")
	writeFile(t, filepath.Join(seed, "feature.txt"), "the pull request's change\n")
	gitT(t, seed, "add", "feature.txt")
	gitT(t, seed, "commit", "-q", "-m", "feature")
	gitT(t, seed, "push", "-q", origin, "HEAD:refs/heads/feature", "HEAD:refs/pull/7/head")
	f.upstreams[repoID] = origin
	return origin
}

// restorer rebuilds the fixture's checkouts from their local origins, reading
// pull request #7 as one whose head is feature on the same origin. It resolves
// no name to a repository: every checkout the fixture writes records its row id.
func (f *snapshotFixture) restorer() checkoutRestorer {
	return checkoutRestorer{
		seed: func(_ context.Context, _, owner, repo string) gitSeed {
			return gitSeed{owner: owner, repo: repo, cloneURL: f.upstreams[owner+"/"+repo]}
		},
		pr: func(_ context.Context, owner, repo string, number int) (*ghclient.PRView, error) {
			origin := f.upstreams[owner+"/"+repo]
			return &ghclient.PRView{Number: number, HeadRef: "feature", BaseRef: "main", CloneURL: origin, SSHURL: origin}, nil
		},
	}
}

// prServer is a GitHub REST stand-in that answers pull request acme/app#7 with
// the fixture's acme/app origin as both base and head, counting the requests
// it serves when fetches is non-nil.
func (f *snapshotFixture) prServer(t *testing.T, fetches *atomic.Int32) *httptest.Server {
	t.Helper()
	origin := f.upstream(t, "acme/app")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fetches != nil {
			fetches.Add(1)
		}
		if r.URL.Path != "/repos/acme/app/pulls/7" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 7,
			"head":   map[string]any{"ref": "feature", "repo": map[string]any{"clone_url": origin}},
			"base":   map[string]any{"ref": "main", "repo": map[string]any{"clone_url": origin}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// taskRegistry backs the fixture's spawner with a database holding the task's
// entity — pull request acme/app#7, polled from entityHost — and an acme/app
// repository row on each of hosts, returning the entity's id and the rows by
// host. The row on entityHost clones from the fixture's acme/app origin; any
// other row's clone URL leads nowhere, so a rebuild seeded from it fails.
func (f *snapshotFixture) taskRegistry(t *testing.T, entityHost string, hosts ...string) (string, map[string]domain.Repository) {
	t.Helper()
	ctx := context.Background()
	stores := sqlitestore.New(newDelegateTestDB(t))
	entity, _, err := stores.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "github", entityHost, "acme/app#7", "", "pr", "T", entityHost+"/acme/app/pull/7")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	rows := map[string]domain.Repository{}
	for _, host := range hosts {
		cloneURL := "file://" + filepath.Join(t.TempDir(), "elsewhere.git")
		if host == entityHost {
			cloneURL = f.upstream(t, "acme/app")
		}
		row, err := stores.Repos.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Repository{Host: host, Owner: "acme", Repo: "app", CloneURL: cloneURL})
		if err != nil {
			t.Fatalf("seed acme/app on %s: %v", host, err)
		}
		rows[host] = row
	}
	f.s.repos = stores.Repos
	f.s.entities = stores.Entities
	f.s.orgs = stores.Orgs
	return entity.ID, rows
}

// writeLegacySnapshot writes the fixture tree's snapshot the way a binary that
// did not record repository row ids wrote it: each manifest checkout names its
// repository by owner/repo alone. It asserts the manifest has that shape.
func (f *snapshotFixture) writeLegacySnapshot(t *testing.T) {
	t.Helper()
	f.ledger.mu.Lock()
	for i := range f.ledger.rows {
		f.ledger.rows[i].RepositoryID = ""
	}
	f.ledger.mu.Unlock()
	f.snapshot(t, "", domain.ConversationRuntimeNative)

	rc, err := f.s.Storage().Get(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, f.key))
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	defer rc.Close()
	zr, err := zstd.NewReader(rc)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	if hdr, err := tr.Next(); err != nil || hdr.Name != snapManifest {
		t.Fatalf("first member = %v (err %v), want the manifest", hdr, err)
	}
	var man struct {
		Checkouts []map[string]any `json:"checkouts"`
	}
	if err := json.NewDecoder(tr).Decode(&man); err != nil {
		t.Fatal(err)
	}
	for _, co := range man.Checkouts {
		if _, ok := co["repository_id"]; ok || co["repo_id"] == "" {
			t.Fatalf("legacy manifest checkout = %v, want repo_id and no repository_id", co)
		}
	}
}

// snapshot writes the fixture tree's snapshot under its task key.
func (f *snapshotFixture) snapshot(t *testing.T, sessionID, runtime string) {
	t.Helper()
	if err := f.s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, fixtureConversation, f.key, "", f.root, sessionID, runtime); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}
}

// loseRoot removes the run root, as a host loss or a /tmp wipe does. The bares
// survive, as they do on a host that keeps its state root.
func (f *snapshotFixture) loseRoot(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(f.root); err != nil {
		t.Fatalf("remove run root: %v", err)
	}
}

// conv is the claimed conversation a resume rebuilds the fixture's tree for.
// Its recorded path is the root itself, so a rebuild does not re-stamp it.
func (f *snapshotFixture) conv(sessionID string) *domain.Conversation {
	return &domain.Conversation{
		ID: fixtureConversation, ClaimID: "claim-1", TaskID: f.key, WorktreePath: f.root,
		SessionID: sessionID, Runtime: domain.ConversationRuntimeSDK,
	}
}

// dirtyCheckout leaves a checkout the three kinds of work a restore has to
// bring back: an unpushed commit, an uncommitted edit and an untracked file.
func dirtyCheckout(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "committed.txt"), "unpushed commit\n")
	gitT(t, dir, "add", "committed.txt")
	gitT(t, dir, "commit", "-q", "-m", "agent work")
	writeFile(t, filepath.Join(dir, "README.md"), "hello\nuncommitted edit\n")
	writeFile(t, filepath.Join(dir, "untracked.txt"), "untracked file\n")
}

// putTarBlob stores a hand-built blob under the fixture's key: zstd, or the
// gzip an older binary wrote.
func putTarBlob(t *testing.T, f *snapshotFixture, zstdBlob bool, members func(*tar.Writer)) {
	t.Helper()
	var buf bytes.Buffer
	var w io.WriteCloser
	if zstdBlob {
		zw, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		w = zw
	} else {
		w = gzip.NewWriter(&buf)
	}
	tw := tar.NewWriter(w)
	members(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Storage().Put(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, f.key), &buf); err != nil {
		t.Fatal(err)
	}
}

// checkoutLedger is conversation_worktrees in memory: the rows a snapshot reads
// for the task, and the rows a restore records through the claim fence.
type checkoutLedger struct {
	db.ConversationWorktreeStore
	mu       sync.Mutex
	rows     []domain.ConversationWorktree
	recorded []domain.ConversationWorktree
}

func (l *checkoutLedger) add(w domain.ConversationWorktree) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, w)
}

func (l *checkoutLedger) recordedRows() []domain.ConversationWorktree {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]domain.ConversationWorktree(nil), l.recorded...)
}

func (l *checkoutLedger) ListForTaskSystem(context.Context, string, string) ([]domain.ConversationWorktree, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]domain.ConversationWorktree(nil), l.rows...), nil
}

func (l *checkoutLedger) RecordForClaimSystem(_ context.Context, _, _ string, w domain.ConversationWorktree) (domain.ConversationWorktree, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recorded = append(l.recorded, w)
	return w, nil
}

// --- helpers ---------------------------------------------------------------

// newStorageSpawner builds a bare Spawner with a blob store and an in-memory
// checkout ledger — enough for the snapshot/rehydrate path, which on the same
// host touches no other table (the rebuilt root equals the stored
// worktree_path, so nothing re-stamps it).
func newStorageSpawner(t *testing.T) *Spawner {
	t.Helper()
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	s := NewSpawner(nil, db.Stores{ConversationWorktrees: &checkoutLedger{}}, nil, nil, "")
	s.SetStorage(blobs)
	return s
}

// setupGitTestEnv isolates HOME (so ~/.claude session writes + git config land
// in a throwaway dir) and pins a git identity so commits don't depend on the
// developer's global config.
func setupGitTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
}

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %q: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// gitOut runs git and returns stdout (for rev-parse and friends).
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s in %q: %v", strings.Join(args, " "), dir, err)
	}
	return string(out)
}

// writeSession writes a fake Claude session transcript for wtPath's cwd and
// returns its path.
func writeSession(t *testing.T, wtPath, sessionID, body string) string {
	t.Helper()
	p, err := worktree.ClaudeSessionPath(worktree.ResolveClaudeProjectCwd(wtPath), sessionID)
	if err != nil {
		t.Fatalf("ClaudeSessionPath: %v", err)
	}
	writeFile(t, p, body)
	return p
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		t.Errorf("%s = %q, want it to contain %q", path, string(data), want)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s exists or errored unexpectedly (%v); it should have been excluded from the snapshot", path, err)
	}
}
