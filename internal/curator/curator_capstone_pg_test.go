package curator

// TFAC-65 capstone. These pgtests are the epic-level (TFAC-63) acceptance
// proof: a multi-mode tenant pins a PRIVATE repo, sends a Curator message,
// and gets a completed turn where every gap the epic closed is exercised
// end-to-end through the real SendMessage → per-project goroutine →
// dispatch → terminal path, against the production two-pool Postgres store
// (admin + RLS-active app pool).
//
// What each sub-issue contributes, and where it's asserted here:
//
//   - TFAC-62 (seed pinned bares with the App token): the pinned bare
//     materializes on demand and the multi-mode clone-credential resolver
//     is invoked → TestCurator_Postgres_Multimode_FullTurn.
//   - TFAC-64 (RLS-safe lifecycle): every turn write lands attributed to
//     the requesting (org, user) with no cross-tenant leakage; cancel /
//     project-delete / simulated-restart leave no non-terminal row →
//     FullTurn + CancelMidFlight + SimulatedRestartSweepsOrphans (plus the
//     existing CancelProject drain in curator_pg_test.go and the admin-pool
//     orphan audit in internal/db/postgres/curator_test.go).
//   - TFAC-130 (org-scoped on-disk state): the knowledge dir + shared
//     worktree resolve under <state-root>/orgs/<orgID>/… → FullTurn.
//   - TFAC-61 (shared read-only worktrees): two concurrent same-org
//     sessions on one repo share a single read-only checkout, bind-mounted
//     via RunOptions.ReadOnlyRepoMounts → SharedReadOnlyWorktree (Linux).
//   - TFAC-60 (bounded, evictable cache): exceeding the per-pod disk budget
//     evicts a cold org's bare and it re-seeds on next use; an in-use bare
//     is never evicted → BoundedEvictableCache.
//
// The agent itself is stubbed via the Curator.runAgent seam so the turn
// reaches terminal without the claude subprocess or the gVisor jail; the
// jail-dependent assertions (shared RO mounts) gate on agentproc.WillSandbox
// (multi + Linux), exactly like the existing sandbox tests. Everything runs
// on the existing pgtest container — no new infra.

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// TestCurator_Postgres_Multimode_FullTurn is the heart of the capstone: a
// full multi-mode turn on a private pinned repo, seed → dispatch → terminal.
// It asserts the seeded bare, the multi-mode auth resolution, org-scoped
// on-disk state, per-(org,user) RLS attribution with no cross-tenant leak,
// the credential/identity threading into RunOptions, and — on Linux — the
// shared read-only repo mount.
func TestCurator_Postgres_Multimode_FullTurn(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	multimodeEnv(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	orgB, bob, _ := pgtest.SeedOrgWithUser(t, h, "bob")

	// A "private" pinned repo for tenant A: a local bare upstream stands in
	// for the private GitHub repo and is recorded in repo_profiles so the
	// curator's admin-pool profile read resolves the clone URL + branch.
	upstream := makeCapstoneUpstream(t)
	const owner, repo = "acme", "private"
	seedRepoProfile(t, h, orgA, owner, repo, upstream, "main")
	projectA := seedPgProjectPinned(t, h, orgA, alice, teamA, "kb", []string{owner + "/" + repo})

	// Stub the agent so the dispatch reaches terminal without claude/gVisor.
	// driveSink replays the real per-turn write set (session-id capture + one
	// streamed message) so the RLS attribution path is genuinely exercised.
	stub := &stubAgent{driveSink: true}
	resolver := &capstoneResolver{token: "ghs_capstone"}

	c := New(stores, nil, "capstone-model")
	t.Cleanup(startTestClaimLoop(t, stores, c))
	c.runAgent = stub.run
	c.SetRunCredentialResolvers(resolver, capstoneSecrets{}, nil)
	t.Cleanup(c.Shutdown)

	reqID, err := c.SendMessage(ctx, projectA, orgA, alice, "what does this repo do?")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	waitForStatus(t, h, reqID, "done")

	// --- TFAC-62: the pinned bare seeded on demand; auth path resolved ---
	bareDir := paths.BareCacheDir(runmode.LocalDefaultOrgID, owner, repo)
	if !dirExists(bareDir) {
		t.Errorf("pinned-repo bare was not seeded at %s — the GetSystem profile read or on-demand seed failed", bareDir)
	}
	if !resolver.calledWith(orgA, owner) {
		t.Errorf("clone-token resolver was not invoked for (%s, %s); the multi-mode auth path did not run (calls=%v)", orgA, owner, resolver.snapshot())
	}

	// --- TFAC-130: on-disk state under the per-org multi state root ---
	wantKB := paths.ProjectKBDir(orgA, projectA)
	if !dirExists(wantKB) {
		t.Errorf("knowledge dir not materialized under the multi state root: %s", wantKB)
	}
	if !strings.Contains(wantKB, filepath.Join("orgs", orgA)) {
		t.Errorf("knowledge dir %q is not org-scoped under orgs/<orgID> (TFAC-130)", wantKB)
	}

	// --- credential + identity threading into the run ---
	opts := stub.firstCall(t)
	if opts.Model != "capstone-model" {
		t.Errorf("RunOptions.Model = %q, want capstone-model", opts.Model)
	}
	if opts.OrgID != orgA {
		t.Errorf("RunOptions.OrgID = %q, want %s", opts.OrgID, orgA)
	}
	if opts.Secrets == nil {
		t.Error("RunOptions.Secrets not threaded (per-org LLM creds seam) — TFAC-133")
	}
	if opts.StartAgentHost == nil {
		t.Error("RunOptions.StartAgentHost not wired (the in-sandbox exec daemon seam) — TFAC-61")
	}

	// --- TFAC-64: every turn write attributed to alice/orgA ---
	if got := turnCreator(t, h, reqID); got != alice {
		t.Errorf("user message user_id = %s, want alice %s", got, alice)
	}
	if n := assistantMessageCount(t, h, reqID); n != 1 {
		t.Errorf("assistant messages = %d, want 1 (sink wrote one under claims)", n)
	}
	if got := firstAssistantMessageCreator(t, h, reqID); got != alice {
		t.Errorf("assistant message user_id = %s, want alice %s", got, alice)
	}
	if sid := conversationSessionID(t, h, projectA); sid != "sess-capstone" {
		t.Errorf("conversations.sdk_session_id = %q, want sess-capstone (session capture under claims)", sid)
	}

	// --- the ledger carries the turn's tokens + the release's cost lump ---
	// The dispatch's sink persisted the streamed message (with its token
	// usage + claim id) and the release settled the reported cost as one
	// lump on the claim's last row; the claim itself keeps the reported
	// duration/turns telemetry. Read back under alice's claims (exercises
	// the app-pool claims read-scan too).
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgA, alice, func(ts db.TxStores) error {
		conv, err := ts.Curator.GetLiveConversation(ctx, orgA, projectA, alice)
		if err != nil {
			return err
		}
		if conv == nil {
			t.Fatal("alice cannot read her own conversation")
			return nil
		}
		claims, err := ts.Curator.ListClaims(ctx, orgA, conv.ID)
		if err != nil {
			return err
		}
		if len(claims) != 1 {
			t.Fatalf("claims = %d, want 1", len(claims))
		}
		cl := claims[0]
		if cl.DurationMs == nil || *cl.DurationMs != 5 || cl.NumTurns == nil || *cl.NumTurns != 1 {
			t.Errorf("claim telemetry = (%v, %v), want (5, 1) — the stub result's reported duration/turns", cl.DurationMs, cl.NumTurns)
		}
		msgs, err := ts.Curator.ListConversationMessages(ctx, orgA, conv.ID)
		if err != nil {
			return err
		}
		var in, out, crd, ccr int
		var cost float64
		for _, m := range msgs {
			if m.InputTokens != nil {
				in += *m.InputTokens
			}
			if m.OutputTokens != nil {
				out += *m.OutputTokens
			}
			if m.CacheReadTokens != nil {
				crd += *m.CacheReadTokens
			}
			if m.CacheCreationTokens != nil {
				ccr += *m.CacheCreationTokens
			}
			if m.CostUSD != nil {
				cost += *m.CostUSD
			}
		}
		if in != capstoneInputTokens || out != capstoneOutputTokens ||
			crd != capstoneCacheReadTokens || ccr != capstoneCacheCreationTokens {
			t.Errorf("ledger tokens = (%d,%d,%d,%d), want (%d,%d,%d,%d)",
				in, out, crd, ccr,
				capstoneInputTokens, capstoneOutputTokens, capstoneCacheReadTokens, capstoneCacheCreationTokens)
		}
		if cost < 0.0009 || cost > 0.0011 {
			t.Errorf("ledger cost = %v, want ~0.001 (the release's settlement lump)", cost)
		}
		return nil
	}); err != nil {
		t.Fatalf("alice read tokens: %v", err)
	}

	// --- no cross-tenant leakage: bob (orgB) cannot read alice's turn ---
	aliceConvID := conversationIDFor(t, h, reqID)
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgB, bob, func(ts db.TxStores) error {
		msgs, err := ts.Curator.ListConversationMessages(ctx, orgB, aliceConvID)
		if err != nil {
			return err
		}
		if len(msgs) != 0 {
			t.Errorf("bob (orgB) read %d of alice's messages — RLS leak across tenants", len(msgs))
		}
		claims, err := ts.Curator.ListClaims(ctx, orgB, aliceConvID)
		if err != nil {
			return err
		}
		if len(claims) != 0 {
			t.Errorf("bob (orgB) read %d of alice's claims — RLS leak across tenants", len(claims))
		}
		return nil
	}); err != nil {
		t.Fatalf("bob cross-tenant read: %v", err)
	}

	// --- TFAC-61 (Linux) / local: the repo materialization path ---
	if agentproc.WillSandbox() {
		wantShared := paths.CuratorSharedRepoDir(orgA, owner, repo)
		if len(opts.ReadOnlyRepoMounts) != 1 {
			t.Fatalf("RunOptions.ReadOnlyRepoMounts = %d, want 1 shared mount on the sandbox path", len(opts.ReadOnlyRepoMounts))
		}
		if got := opts.ReadOnlyRepoMounts[0].Source; got != wantShared {
			t.Errorf("RO mount Source = %q, want the shared worktree %q", got, wantShared)
		}
		if got := opts.ReadOnlyRepoMounts[0].RelPath; got != filepath.Join("repos", owner, repo) {
			t.Errorf("RO mount RelPath = %q, want repos/%s/%s", got, owner, repo)
		}
		if !dirExists(wantShared) {
			t.Errorf("shared read-only worktree not materialized at %s", wantShared)
		}
	} else {
		// Local / non-Linux multi: a per-project worktree under cwd, no jail
		// mounts. Same seed path, different on-disk layout (N=1, no sharing).
		if len(opts.ReadOnlyRepoMounts) != 0 {
			t.Errorf("non-sandbox path must build no RO mounts; got %d", len(opts.ReadOnlyRepoMounts))
		}
		perProject := filepath.Join(wantKB, "repos", owner, repo)
		if !dirExists(perProject) {
			t.Errorf("per-project worktree not materialized at %s", perProject)
		}
	}
}

// TestCurator_Postgres_Multimode_SharedReadOnlyWorktree pins the core
// TFAC-61 property at the capstone level: two concurrent sessions (two
// projects in the SAME org) pinning the SAME repo resolve to ONE shared
// read-only on-disk checkout, bind-mounted into each jail — not a
// per-session duplicate. Multi-mode + Linux only (the shared-RO mount is
// the sandbox path), gated like the existing sandbox tests.
func TestCurator_Postgres_Multimode_SharedReadOnlyWorktree(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	multimodeEnv(t)
	if !agentproc.WillSandbox() {
		t.Skip("shared read-only worktree mounting is multi-mode + Linux only (gVisor); sandbox inactive here")
	}
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	upstream := makeCapstoneUpstream(t)
	const owner, repo = "acme", "shared"
	seedRepoProfile(t, h, orgA, owner, repo, upstream, "main")
	projA := seedPgProjectPinned(t, h, orgA, alice, teamA, "kb-a", []string{owner + "/" + repo})
	projB := seedPgProjectPinned(t, h, orgA, alice, teamA, "kb-b", []string{owner + "/" + repo})

	// The stub parks both dispatches inside runAgent (each holding a reader
	// on the shared worktree) until released, so the two sessions overlap.
	stub := &stubAgent{driveSink: true, inFlight: make(chan struct{}, 2), release: make(chan struct{})}
	c := New(stores, nil, "capstone-model")
	t.Cleanup(startTestClaimLoop(t, stores, c))
	c.runAgent = stub.run
	t.Cleanup(c.Shutdown)

	reqA, err := c.SendMessage(ctx, projA, orgA, alice, "a")
	if err != nil {
		t.Fatalf("send A: %v", err)
	}
	reqB, err := c.SendMessage(ctx, projB, orgA, alice, "b")
	if err != nil {
		t.Fatalf("send B: %v", err)
	}

	// Both materialized their pinned repo and are now parked in runAgent.
	waitInFlight(t, stub, 2)

	wantShared := paths.CuratorSharedRepoDir(orgA, owner, repo)
	srcs := stub.mountSources()
	if len(srcs) != 2 {
		t.Fatalf("captured %d dispatches, want 2", len(srcs))
	}
	for i, s := range srcs {
		if s != wantShared {
			t.Errorf("dispatch %d shared mount Source = %q, want %q (both sessions must share one worktree)", i, s, wantShared)
		}
	}
	// Exactly one linked worktree backs the bare even with two live readers.
	// LocalDefaultOrgID (not orgA): the bare cache is org-global for now;
	// the org-scoped shared worktree is what isolates tenants, not the bare.
	if n := linkedWorktreeCount(t, paths.BareCacheDir(runmode.LocalDefaultOrgID, owner, repo)); n != 1 {
		t.Errorf("linked worktrees against the bare = %d, want 1 (single shared checkout for 2 sessions)", n)
	}

	close(stub.release)
	waitForStatus(t, h, reqA, "done")
	waitForStatus(t, h, reqB, "done")
}

// TestCurator_Postgres_Multimode_BoundedEvictableCache pins TFAC-60 through
// the curator's own repo-materialization path: under a tight per-pod disk
// budget the reaper evicts a cold org's bare while never reclaiming an
// in-use one, and the evicted bare transparently re-seeds on next use.
func TestCurator_Postgres_Multimode_BoundedEvictableCache(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	multimodeEnv(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgCold, _, _ := pgtest.SeedOrgWithUser(t, h, "cold")
	orgHot, _, _ := pgtest.SeedOrgWithUser(t, h, "hot")

	coldUp := makeCapstoneUpstream(t)
	hotUp := makeCapstoneUpstream(t)
	// Distinct repos → distinct bares (the bare cache keys on owner/repo).
	seedRepoProfile(t, h, orgCold, "acme", "cold", coldUp, "main")
	seedRepoProfile(t, h, orgHot, "acme", "hot", hotUp, "main")

	noToken := func(context.Context, string, string) worktree.CloneAuth { return worktree.CloneAuth{} }

	// The hot org's repo is materialized and KEPT mounted (a live reader);
	// the cold org's is materialized then released (quiescent).
	hotMounts, hotRelease := materializeSharedPinnedRepos(ctx, stores.Repos, noToken, orgHot, "proj-hot", []string{"acme/hot"})
	if len(hotMounts) != 1 {
		t.Fatalf("hot materialize built %d mounts, want 1", len(hotMounts))
	}
	_, coldRelease := materializeSharedPinnedRepos(ctx, stores.Repos, noToken, orgCold, "proj-cold", []string{"acme/cold"})
	coldRelease()

	// LocalDefaultOrgID, not orgCold/orgHot: the bare cache is still org-global
	// (repoDir resolves the sentinel org for now), so both orgs' bares
	// live under the one <state-root>/repos tree. The shared *worktree* each
	// session mounts is org-scoped — that's the tenant-isolation boundary, not
	// the bare — which is why eviction here keys on the global bare path.
	coldBare := paths.BareCacheDir(runmode.LocalDefaultOrgID, "acme", "cold")
	hotBare := paths.BareCacheDir(runmode.LocalDefaultOrgID, "acme", "hot")
	if !dirExists(coldBare) || !dirExists(hotBare) {
		t.Fatalf("precondition: both bares should be seeded (cold=%v hot=%v)", dirExists(coldBare), dirExists(hotBare))
	}

	// Reaper under a 1-byte budget: it must reclaim the quiescent cold bare
	// and skip the in-use hot one (the hot bare is the coldest by last-use,
	// so only the live-reader guard can be saving it).
	worktree.EnforceBudget(ctx, worktree.Policy{MaxBytes: 1})

	if dirExists(coldBare) {
		t.Error("cold org's quiescent bare survived a 1-byte budget — TFAC-60 requires it to be evictable")
	}
	if !dirExists(hotBare) {
		t.Error("hot org's in-use bare was evicted — TFAC-60 requires an in-use bare to never be reclaimed")
	}

	hotRelease()

	// The evicted cold bare re-seeds transparently on the next materialize.
	_, coldRelease2 := materializeSharedPinnedRepos(ctx, stores.Repos, noToken, orgCold, "proj-cold", []string{"acme/cold"})
	defer coldRelease2()
	if !dirExists(coldBare) {
		t.Error("cold bare did not transparently re-seed on next use after eviction (TFAC-60)")
	}
}

// TestCurator_Postgres_Multimode_CancelMidFlight pins the in-flight cancel
// half of TFAC-64 through the real goroutine: a running turn that is
// cancelled mid-dispatch flips to a terminal cancelled status (under the
// requesting user's claims, the app-pool path) and never dangles.
func TestCurator_Postgres_Multimode_CancelMidFlight(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	multimodeEnv(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	upstream := makeCapstoneUpstream(t)
	seedRepoProfile(t, h, orgA, "acme", "repo", upstream, "main")
	projA := seedPgProjectPinned(t, h, orgA, alice, teamA, "kb", []string{"acme/repo"})

	stub := &stubAgent{inFlight: make(chan struct{}, 1), release: make(chan struct{})}
	c := New(stores, nil, "capstone-model")
	t.Cleanup(startTestClaimLoop(t, stores, c))
	c.runAgent = stub.run
	t.Cleanup(c.Shutdown)

	reqID, err := c.SendMessage(ctx, projA, orgA, alice, "long-running")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	// Parked in runAgent ⇒ the row is already 'running'.
	waitInFlight(t, stub, 1)

	c.Cancel(orgA, projA)

	waitForStatus(t, h, reqID, "cancelled")
	close(stub.release) // release any racing parked dispatch (no-op once returned)

	// Defense-in-depth: nothing left non-terminal for this project.
	if n := nonTerminalCount(t, h, projA); n != 0 {
		t.Errorf("project has %d non-terminal curator_requests after cancel, want 0", n)
	}
}

// TestCurator_Postgres_Multimode_SimulatedRestartSweepsOrphans drives the
// boot-recovery sequence: a process restart strands every RUNNING turn (a
// live claim whose session goroutine died), and the startup sweep (admin
// pool) must release them across ALL tenants before the new Curator comes
// up. Queued turns are unowned undelivered messages and deliberately
// survive. Exercised end to end with a live post-restart turn.
func TestCurator_Postgres_Multimode_SimulatedRestartSweepsOrphans(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	multimodeEnv(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	orgB, bob, teamB := pgtest.SeedOrgWithUser(t, h, "bob")
	projA := seedPgProject(t, h, orgA, alice, teamA, "a")
	projB := seedPgProject(t, h, orgB, bob, teamB, "b")

	// Each tenant leaves a live engagement behind, as if its per-project
	// goroutine was killed mid-turn by the restart.
	convA, msgA := seedTurn(t, ctx, stores, orgA, alice, projA, "stranded-a")
	convB, msgB := seedTurn(t, ctx, stores, orgB, bob, projB, "stranded-b")
	beginSeededTurn(t, ctx, stores, orgA, alice, projA, convA, msgA)
	beginSeededTurn(t, ctx, stores, orgB, bob, projB, convB, msgB)

	// Boot recovery: the sweep runs BEFORE the new Curator is constructed
	// (internal/app/subsystems.go) with no JWT-claims context. It must reach
	// BOTH tenants' stranded engagements via the admin pool.
	n, err := stores.Curator.CancelOrphanedTurnsSystem(ctx)
	if err != nil {
		t.Fatalf("orphan sweep: %v", err)
	}
	if n < 2 {
		t.Errorf("sweep released %d claims, want >= 2 (both tenants' stranded engagements)", n)
	}
	if got := turnStatusPg(t, h, msgA); got != "cancelled" {
		t.Errorf("orgA stranded turn = %q, want cancelled", got)
	}
	if got := turnStatusPg(t, h, msgB); got != "cancelled" {
		t.Errorf("orgB stranded turn = %q, want cancelled (cross-tenant boot sweep)", got)
	}

	// Recovery complete: a freshly-constructed Curator accepts a new turn.
	c := New(stores, nil, "capstone-model")
	t.Cleanup(startTestClaimLoop(t, stores, c))
	c.runAgent = (&stubAgent{}).run
	t.Cleanup(c.Shutdown)
	newReq, err := c.SendMessage(ctx, projA, orgA, alice, "post-restart")
	if err != nil {
		t.Fatalf("post-restart send: %v", err)
	}
	waitForStatus(t, h, newReq, "done")
}

// --- stub agent -----------------------------------------------------------

// Token usage the driveSink stub streams on its one assistant message, so
// tests can assert the ledger carries the turn's tokens.
const (
	capstoneInputTokens         = 111
	capstoneOutputTokens        = 22
	capstoneCacheReadTokens     = 3333
	capstoneCacheCreationTokens = 44
)

func intPtr(n int) *int { return &n }

// stubAgent stands in for agentproc.Run via the Curator.runAgent seam. It
// captures each turn's RunOptions, optionally replays the sink writes, and
// optionally parks the dispatch (inFlight signal + release gate) so tests
// can observe overlapping in-flight turns and fire cancellations.
type stubAgent struct {
	mu        sync.Mutex
	calls     []agentproc.RunOptions
	driveSink bool
	result    *agentproc.Result

	inFlight chan struct{} // one send per dispatch entry (nil = don't signal)
	release  chan struct{} // dispatch blocks until closed (nil = don't block)
}

func (s *stubAgent) run(ctx context.Context, opts agentproc.RunOptions, sink agentproc.Sink) (*agentproc.Outcome, error) {
	s.mu.Lock()
	s.calls = append(s.calls, opts)
	s.mu.Unlock()

	if s.driveSink {
		// The real per-turn RLS-attributed write set: session-id capture
		// onto the conversation + one streamed message row, both wrapped in
		// SyntheticClaimsWithTx by the curator sink. The message carries a
		// token usage block the sink persists claim-stamped; the release
		// then rolls the per-claim SUM onto the claim's token columns.
		_ = sink.OnSession("sess-capstone")
		_ = sink.OnMessage(&domain.Message{
			Role: "assistant", Content: "capstone ack",
			InputTokens:         intPtr(capstoneInputTokens),
			OutputTokens:        intPtr(capstoneOutputTokens),
			CacheReadTokens:     intPtr(capstoneCacheReadTokens),
			CacheCreationTokens: intPtr(capstoneCacheCreationTokens),
		})
	}
	if s.inFlight != nil {
		s.inFlight <- struct{}{}
	}
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	res := s.result
	if res == nil {
		res = &agentproc.Result{IsError: false, Result: "ok", NumTurns: 1, DurationMs: 5, CostUSD: 0.001}
	}
	return &agentproc.Outcome{Result: res, SessionID: "sess-capstone"}, nil
}

func (s *stubAgent) firstCall(t *testing.T) agentproc.RunOptions {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		t.Fatal("stub agent was never invoked")
	}
	return s.calls[0]
}

// mountSources returns each captured dispatch's first read-only repo-mount
// source ("" when a dispatch built no mount).
func (s *stubAgent) mountSources() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.calls))
	for _, c := range s.calls {
		if len(c.ReadOnlyRepoMounts) > 0 {
			out = append(out, c.ReadOnlyRepoMounts[0].Source)
		} else {
			out = append(out, "")
		}
	}
	return out
}

// --- fakes ----------------------------------------------------------------

// capstoneResolver records TokenFor calls and hands back a fixed App token,
// so the test can confirm the multi-mode clone-credential path ran. Only
// TokenFor is exercised by the curator; the other Resolver methods are
// unused no-ops.
type capstoneResolver struct {
	mu    sync.Mutex
	calls [][2]string // (orgID, target)
	token string
}

var _ ghclient.Resolver = (*capstoneResolver)(nil)

func (r *capstoneResolver) ClientFor(context.Context, string, string) (*ghclient.Client, error) {
	return nil, nil
}
func (r *capstoneResolver) ClientForRepo(context.Context, string, string, string) (*ghclient.Client, error) {
	return nil, nil
}
func (r *capstoneResolver) TokenFor(_ context.Context, orgID, target string) (githubapp.Token, error) {
	r.mu.Lock()
	r.calls = append(r.calls, [2]string{orgID, target})
	r.mu.Unlock()
	return githubapp.Token{Value: r.token}, nil
}
func (r *capstoneResolver) BaseURLFor(context.Context, string) (string, error) {
	return "https://github.com", nil
}
func (r *capstoneResolver) OrgIdentityFor(context.Context, string) (string, string, bool) {
	return "", "", false
}
func (r *capstoneResolver) calledWith(orgID, target string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if c[0] == orgID && c[1] == target {
			return true
		}
	}
	return false
}
func (r *capstoneResolver) snapshot() [][2]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][2]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// capstoneSecrets is a non-nil SecretsReader so RunOptions.Secrets is
// threaded; it is never read because the agent is stubbed.
type capstoneSecrets struct{}

var _ agentproc.SecretsReader = capstoneSecrets{}

func (capstoneSecrets) Get(context.Context, string, string) (string, error) { return "", nil }

// --- fixtures + readers ---------------------------------------------------

// multimodeEnv flips the process into multi mode and relocates all on-disk
// state onto a fresh per-test tempdir, both restored via t.Cleanup. Serial
// use only (it mutates process globals) — the pg suite does not parallelize.
func multimodeEnv(t *testing.T) {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)
	paths.SetForTest(t, t.TempDir())
}

// seedRepoProfile records a repo_profiles row (admin pool) so the curator's
// GetSystem read resolves a clone URL + branch for the pinned repo.
func seedRepoProfile(t *testing.T, h *pgtest.Harness, orgID, owner, repo, cloneURL, defaultBranch string) {
	t.Helper()
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO repo_profiles (org_id, owner, repo, clone_url, default_branch, clone_status, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'ok', now())
	`, orgID, owner, repo, cloneURL, defaultBranch)
}

// seedPgProjectPinned inserts a project with pinned_repos set (admin pool —
// no JWT claims in a bare test context). Mirrors seedPgProject in
// curator_pg_test.go but threads the pins through.
func seedPgProjectPinned(t *testing.T, h *pgtest.Harness, orgID, userID, teamID, name string, pins []string) string {
	t.Helper()
	id := uuid.New().String()
	pinsJSON, err := json.Marshal(pins)
	if err != nil {
		t.Fatalf("marshal pins: %v", err)
	}
	pgtest.MustExec(t, h.AdminDB, `
		INSERT INTO projects (id, org_id, creator_user_id, team_id, name, pinned_repos, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, now(), now())
	`, id, orgID, userID, teamID, name, string(pinsJSON))
	return id
}

// makeCapstoneUpstream builds a local bare git repo (one commit on main)
// usable as a clone_url, standing in for the private GitHub repo.
func makeCapstoneUpstream(t *testing.T) string {
	t.Helper()
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	mustGit(t, "", "init", "--bare", "--initial-branch=main", upstream)

	work := filepath.Join(t.TempDir(), "work")
	mustGit(t, "", "init", "-b", "main", work)
	mustGit(t, work, "config", "user.email", "test@example.com")
	mustGit(t, work, "config", "user.name", "Capstone")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("capstone upstream\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	mustGit(t, work, "add", "README.md")
	mustGit(t, work, "commit", "-m", "initial")
	mustGit(t, work, "remote", "add", "origin", upstream)
	mustGit(t, work, "push", "origin", "main")
	return upstream
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// linkedWorktreeCount counts the worktrees registered against a bare (one
// subdir per linked worktree under <bare>/worktrees/).
func linkedWorktreeCount(t *testing.T, bareDir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(bareDir, "worktrees"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read worktrees dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			n++
		}
	}
	return n
}

// waitForStatus polls the (admin-pool) derived turn status until it equals
// want or the deadline elapses. id is the wire request id — the turn's user
// message id rendered decimal.
func waitForStatus(t *testing.T, h *pgtest.Harness, id, want string) {
	t.Helper()
	msgID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		t.Fatalf("parse request id %q: %v", id, err)
	}
	deadline := time.Now().Add(20 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = turnStatusPg(t, h, msgID)
		if last == want {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("turn %s status = %q, want %q (timed out)", id, last, want)
}

// waitInFlight drains n in-flight signals from the stub (i.e. waits for n
// dispatches to be parked inside runAgent).
func waitInFlight(t *testing.T, s *stubAgent, n int) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-s.inFlight:
		case <-deadline:
			t.Fatalf("timed out waiting for %d in-flight dispatch(es); got %d", n, i)
		}
	}
}

// turnID parses a wire request id back to the user message id.
func turnID(t *testing.T, id string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		t.Fatalf("parse request id %q: %v", id, err)
	}
	return n
}

func conversationIDFor(t *testing.T, h *pgtest.Harness, requestID string) string {
	t.Helper()
	var conv string
	if err := h.AdminDB.QueryRow(
		`SELECT conversation_id::text FROM messages WHERE id = $1`, turnID(t, requestID),
	).Scan(&conv); err != nil {
		t.Fatalf("read conversation for turn %s: %v", requestID, err)
	}
	return conv
}

func turnCreator(t *testing.T, h *pgtest.Harness, requestID string) string {
	t.Helper()
	var got string
	if err := h.AdminDB.QueryRow(
		`SELECT user_id::text FROM messages WHERE id = $1`, turnID(t, requestID),
	).Scan(&got); err != nil {
		t.Fatalf("read turn creator %s: %v", requestID, err)
	}
	return got
}

func assistantMessageCount(t *testing.T, h *pgtest.Harness, requestID string) int {
	t.Helper()
	var n int
	if err := h.AdminDB.QueryRow(`
		SELECT count(*) FROM messages
		WHERE conversation_id = (SELECT conversation_id FROM messages WHERE id = $1)
		  AND role = 'assistant' AND subtype = ''
	`, turnID(t, requestID)).Scan(&n); err != nil {
		t.Fatalf("count messages %s: %v", requestID, err)
	}
	return n
}

func firstAssistantMessageCreator(t *testing.T, h *pgtest.Harness, requestID string) string {
	t.Helper()
	var got string
	if err := h.AdminDB.QueryRow(`
		SELECT user_id::text FROM messages
		WHERE conversation_id = (SELECT conversation_id FROM messages WHERE id = $1)
		  AND role = 'assistant' AND subtype = ''
		ORDER BY id LIMIT 1
	`, turnID(t, requestID)).Scan(&got); err != nil {
		t.Fatalf("read message creator %s: %v", requestID, err)
	}
	return got
}

func conversationSessionID(t *testing.T, h *pgtest.Harness, projectID string) string {
	t.Helper()
	var sid sql.NullString
	if err := h.AdminDB.QueryRow(`
		SELECT sdk_session_id FROM conversations
		WHERE project_id = $1 AND type = 'curator' AND archived_at IS NULL
		ORDER BY started_at ASC LIMIT 1
	`, projectID).Scan(&sid); err != nil {
		t.Fatalf("read conversation session %s: %v", projectID, err)
	}
	return sid.String
}

// nonTerminalCount counts live turn state on the project's curator
// conversations: undelivered user messages (queued turns) + active claims
// (running engagements).
func nonTerminalCount(t *testing.T, h *pgtest.Harness, projectID string) int {
	t.Helper()
	var n int
	if err := h.AdminDB.QueryRow(`
		SELECT (
		    SELECT count(*) FROM messages m
		    JOIN conversations c ON c.id = m.conversation_id
		    WHERE c.project_id = $1 AND c.type = 'curator'
		      AND m.role = 'user' AND m.subtype = '' AND m.delivered = false
		) + (
		    SELECT count(*) FROM claims cl
		    JOIN conversations c ON c.id = cl.conversation_id
		    WHERE c.project_id = $1 AND c.type = 'curator' AND cl.released_at IS NULL
		)
	`, projectID).Scan(&n); err != nil {
		t.Fatalf("count non-terminal %s: %v", projectID, err)
	}
	return n
}
