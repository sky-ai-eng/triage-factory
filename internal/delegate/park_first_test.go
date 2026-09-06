package delegate

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// TestStop_LiveLocalEngagement_ParksBeforeTheSnapshot is the incident, pinned:
// a stop of a run whose process is on THIS pod used to leave the whole park to
// the dying goroutine, which snapshotted before it flipped, so the run kept
// reporting WORKING for as long as the capture took and a follow-up in that
// window was refused for a row that was not parked yet.
//
// The persist is held open at its upload for the whole assertion, so the
// ordering is a fact rather than a race the test might win. The follow-up goes
// out in exactly that window under the SDK runtime, which is what makes the
// pending record load-bearing: without it the gate has no tree and no blob and
// can only answer 410.
func TestStop_LiveLocalEngagement_ParksBeforeTheSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s, database, conversationID, _ := setupAdvanceFixture(t, "park-first-stop")
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	held := &heldPutStorage{Storage: blobs, entered: make(chan struct{}, 1), release: make(chan struct{})}
	s.SetStorage(held)
	// The engagement the stop kills is a real one: the record its teardown
	// opens names this claim, and the wake gate follows that claim to a live
	// executor before it treats the persist as coming.
	killedClaim := markEngaged(t, database, conversationID)
	stageInstance(t, database, "test-engagement", time.Now())
	namespace := blueprintRunIDForConversation(t, database, conversationID)

	// The engagement: a real worktree with uncommitted work, and a cancel
	// handle registered as a live run's is, so the stop verb finds a local
	// process to kill.
	wt := t.TempDir()
	writeFile(t, filepath.Join(wt, "_tfac", "notes.txt"), "half-finished work")
	runCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancels[conversationID] = cancel
	s.mu.Unlock()
	tornDown := make(chan struct{})
	go func() {
		defer close(tornDown)
		<-runCtx.Done()
		s.parkConversationOpen(context.Background(), liveParkContext{
			orgID:          runmode.LocalDefaultOrgID,
			conversationID: conversationID,
			namespace:      namespace,
			claudeCwd:      wt,
			triggerType:    "event",
			claimID:        killedClaim,
			reason:         db.ParkStopped(domain.ParkReasonUserCancelled, ""),
		}, "")
	}()

	if err := s.Stop(runmode.LocalDefaultOrgID, conversationID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// The verb's own answer, read the instant it returns — the goroutine above
	// is still inside its teardown.
	if got := storedStatus(t, database, conversationID); got != "open" {
		t.Fatalf("status after Stop = %q, want open — the verb must park rather than wait on the teardown", got)
	}
	if hasActiveClaim(t, database, conversationID) {
		t.Error("the claim is still live after Stop; the executor slot stays occupied until the teardown finishes")
	}

	// Wait until the teardown is inside its upload, so what follows is asserted
	// against a persist that is provably still running.
	select {
	case <-held.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the teardown never reached its upload")
	}
	if ok, err := blobs.Exists(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, namespace)); err != nil || ok {
		t.Fatalf("blob present (%v, %v) before the upload was released; the window under test does not exist", ok, err)
	}
	assertSnapshotState(t, s, namespace, domain.WorkspaceSnapshotPending, killedClaim)

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, conversationID, runmode.LocalDefaultUserID, "actually, try the other approach"); err != nil {
		t.Fatalf("follow-up during the persist: %v (a 409/410 here is the bug this ticket exists for)", err)
	}
	if got := pendingRows(t, s, conversationID); len(got) != 1 {
		t.Errorf("queued rows = %d, want the follow-up waiting for the next claim", len(got))
	}

	close(held.release)
	select {
	case <-tornDown:
	case <-time.After(10 * time.Second):
		t.Fatal("the teardown never finished")
	}
	// The teardown's contribution lands either way: the blob, and the record
	// that says so. (Its own status flip is refused by the fence — the stop
	// released the claim first — which is the ordinary shape and changes
	// nothing about the persist.)
	assertSnapshotPresent(t, s, namespace, true)
	assertSnapshotState(t, s, namespace, domain.WorkspaceSnapshotWritten, killedClaim)
}

// TestParkConversationOpen_FlipsBeforeTheSnapshot pins the same ordering for
// the park a user never asked for — the idle / turn-end one, whose "still
// WORKING a second after the agent finishes" tail was the same serialization.
//
// Asserted from inside the upload, the only place the invariant is visible: by
// the time the park returns, pending has become written and a test reading
// afterwards could not tell whether the record ever preceded the flip.
func TestParkConversationOpen_FlipsBeforeTheSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s, database, conversationID, taskID := setupAdvanceFixture(t, "park-first-idle")
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	namespace := blueprintRunIDForConversation(t, database, conversationID)

	var statusAtUpload string
	var stateAtUpload *domain.WorkspaceSnapshotState
	s.SetStorage(&onPutStorage{Storage: blobs, onPut: func() {
		statusAtUpload = storedStatus(t, database, conversationID)
		stateAtUpload = snapshotState(t, s, namespace)
	}})

	wt := t.TempDir()
	writeFile(t, filepath.Join(wt, "_tfac", "notes.txt"), "work in progress")
	claimID := dbtest.SeedActiveClaim(t, database, conversationID, "exec-idle", 1)

	if fenced := s.parkConversationOpen(context.Background(), liveParkContext{
		orgID:          runmode.LocalDefaultOrgID,
		conversationID: conversationID,
		taskID:         taskID,
		namespace:      namespace,
		claudeCwd:      wt,
		triggerType:    "event",
		claimID:        claimID,
		reason:         db.ParkIdle(),
	}, ""); fenced {
		t.Fatal("parkConversationOpen reported a fence trip on a live claim")
	}

	if statusAtUpload != "open" {
		t.Errorf("status during the upload = %q, want open — the flip must not wait on the capture", statusAtUpload)
	}
	if stateAtUpload == nil || stateAtUpload.State != domain.WorkspaceSnapshotPending {
		t.Errorf("state during the upload = %+v, want pending under this engagement — the record has to precede the flip, or a watcher sees a resumable row with nothing behind it", stateAtUpload)
	}
	assertSnapshotState(t, s, namespace, domain.WorkspaceSnapshotWritten, claimID)
	assertSnapshotPresent(t, s, namespace, true)
}

// TestParkConversationOpen_RetriesALostLifecycleOpen: the pre-flip open is
// best-effort — a store hiccup must not hold up a stop — but it must not leave
// the persist untracked either. Untracked is the one shape a resume cannot
// read: no pending to wait on, no written to trust, and an SDK conversation
// reading permanently expired until the blob lands. So the persist re-opens
// what the park could not.
func TestParkConversationOpen_RetriesALostLifecycleOpen(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s, database, conversationID, taskID := setupAdvanceFixture(t, "park-lost-open")
	wireBlobStore(t, s)
	namespace := blueprintRunIDForConversation(t, database, conversationID)
	flaky := &flakyBeginSnapshotStore{WorkspaceSnapshotStore: s.workspaceSnapshots, failFirst: true}
	s.workspaceSnapshots = flaky

	wt := t.TempDir()
	writeFile(t, filepath.Join(wt, "_tfac", "notes.txt"), "work the record almost lost track of")
	claimID := dbtest.SeedActiveClaim(t, database, conversationID, "exec-flaky", 1)

	if fenced := s.parkConversationOpen(context.Background(), liveParkContext{
		orgID:          runmode.LocalDefaultOrgID,
		conversationID: conversationID,
		taskID:         taskID,
		namespace:      namespace,
		claudeCwd:      wt,
		triggerType:    "event",
		claimID:        claimID,
		reason:         db.ParkIdle(),
	}, ""); fenced {
		t.Fatal("parkConversationOpen reported a fence trip on a live claim")
	}

	if flaky.begins < 2 {
		t.Errorf("BeginSnapshotSystem called %d time(s); the persist must retry an open the park lost", flaky.begins)
	}
	assertSnapshotState(t, s, namespace, domain.WorkspaceSnapshotWritten, claimID)
	assertSnapshotPresent(t, s, namespace, true)
}

// TestAwaitSnapshotBlob_TakesABlobThatAlreadyLanded: the upload finishes
// before the record's completing CAS and before a dying writer stops
// answering, so every condition that ends the wait is a moment the blob may
// already be there. Giving up on the writer without looking would discard a
// snapshot sitting in the store — a fresh workspace for a native conversation,
// a 410 for an SDK one, both for nothing.
func TestAwaitSnapshotBlob_TakesABlobThatAlreadyLanded(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, conversationID, _ := setupAdvanceFixture(t, "wait-already-landed")
	wireBlobStore(t, s)
	s.snapshotWaitPollInterval = 5 * time.Millisecond
	namespace := blueprintRunIDForConversation(t, database, conversationID)

	// The state the crash leaves behind: pending forever, its writer gone
	// (no claim row at all, so the liveness read answers "not coming"), and
	// the blob nonetheless present.
	seedSnapshotState(t, s, namespace, "claim-vanished", domain.WorkspaceSnapshotPending)
	putTestSnapshot(t, s, namespace)

	appeared, _ := s.awaitSnapshotBlob(context.Background(), runmode.LocalDefaultOrgID, namespace)
	if !appeared {
		t.Error("the wait reported no snapshot while one was in the store; a resume would rebuild over work that survived")
	}
}

// TestWorkspaceRecoverable_LadderBelowTheBlob covers the answers the wake gate
// gives when neither a warm tree nor a blob exists — the situation park-first
// makes ordinary rather than exceptional. A pending record is two of them: the
// gate follows its writer to an executor, the way the claim-time wait does,
// so a persist whose engagement died is not one anybody is waiting for.
func TestWorkspaceRecoverable_LadderBelowTheBlob(t *testing.T) {
	cases := []struct {
		name string
		// state is the lifecycle record to stage, or "" for none at all.
		state string
		// writerBeat is the pending writer's last heartbeat; zero stages no
		// executor behind the record at all.
		writerBeat time.Time
		wantSDK    bool
		wantNative bool
		wantReason string
	}{
		{
			name:       "persist in flight",
			state:      domain.WorkspaceSnapshotPending,
			writerBeat: time.Now(),
			wantSDK:    true,
			wantNative: true,
		},
		{
			name:       "persist owed by a dead writer",
			state:      domain.WorkspaceSnapshotPending,
			writerBeat: time.Now().Add(-time.Hour),
			wantSDK:    false,
			wantNative: true,
			wantReason: ResumeBlockedWorkspaceExpired,
		},
		{
			name:       "persist failed",
			state:      domain.WorkspaceSnapshotFailed,
			wantSDK:    false,
			wantNative: true,
			wantReason: ResumeBlockedWorkspaceExpired,
		},
		{
			name:       "no persist was ever owed",
			state:      "",
			wantSDK:    false,
			wantNative: true,
			wantReason: ResumeBlockedWorkspaceExpired,
		},
	}
	for _, tc := range cases {
		for _, runtime := range []string{"sdk", "native"} {
			t.Run(tc.name+"/"+runtime, func(t *testing.T) {
				paths.SetForTest(t, t.TempDir())
				database := newDelegateTestDB(t)
				const conversationID = "r-wake-ladder"
				// A path that is not on disk: the warm rung must miss.
				seedConversation(t, database, conversationID, "sess-"+conversationID, filepath.Join(t.TempDir(), "gone"))
				setConversationStatus(t, database, conversationID, "open")
				s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
				if runtime == "native" {
					markNative(t, database, conversationID)
				}
				blobs, err := storage.New()
				if err != nil {
					t.Fatalf("storage.New: %v", err)
				}
				s.SetStorage(blobs)
				namespace := blueprintRunIDForConversation(t, database, conversationID)
				if tc.state != "" {
					const writerClaim = "b1b2c3d4-0000-4000-8000-000000000001"
					seedSnapshotState(t, s, namespace, writerClaim, tc.state)
					if !tc.writerBeat.IsZero() {
						stageClaim(t, database, conversationID, writerClaim, "exec-writer")
						stageInstance(t, database, "exec-writer", tc.writerBeat)
					}
				}

				conv, err := s.conversations.GetSystem(context.Background(), runmode.LocalDefaultOrgID, conversationID)
				if err != nil || conv == nil {
					t.Fatalf("load conversation: %v", err)
				}
				want := tc.wantSDK
				if runtime == "native" {
					want = tc.wantNative
				}
				if got := s.workspaceRecoverable(context.Background(), runmode.LocalDefaultOrgID, conv); got != want {
					t.Fatalf("workspaceRecoverable = %v, want %v", got, want)
				}
				// The gate's answer and the composer's have to be the same one.
				ok, reason := s.ResumabilityFor(context.Background(), runmode.LocalDefaultOrgID, conv)
				if ok != want {
					t.Errorf("ResumabilityFor ok = %v, want %v (reason %q)", ok, want, reason)
				}
				if !want && reason != tc.wantReason {
					t.Errorf("ResumabilityFor reason = %q, want %q", reason, tc.wantReason)
				}
			})
		}
	}
}

// TestEnsureWorkspace_WaitsOutAnInFlightPersist is the claim-time half of the
// pending rung: the wake let the follow-up through because a persist was in
// flight, so the claim has to actually wait for it rather than declare the
// workspace gone.
func TestEnsureWorkspace_WaitsOutAnInFlightPersist(t *testing.T) {
	// Its own run namespace: the rebuild lands under the process temp dir,
	// which outlives the test.
	isolateRunNamespace(t)
	setupGitTestEnv(t)
	s, database, conversationID, _ := setupAdvanceFixture(t, "wait-inflight")
	wireBlobStore(t, s)
	s.snapshotWaitPollInterval = 5 * time.Millisecond
	s.SetSnapshotWaitTimeout(10 * time.Second)
	namespace := blueprintRunIDForConversation(t, database, conversationID)
	markNative(t, database, conversationID)

	const writerClaim = "claim-slow-writer"
	seedSnapshotState(t, s, namespace, writerClaim, domain.WorkspaceSnapshotPending)
	stageClaim(t, database, conversationID, writerClaim, "exec-writer")
	stageInstance(t, database, "exec-writer", time.Now())

	// The persist lands a beat into the wait, and it is a real one — the blob
	// this rehydrates from has to be a blob a park would really have written.
	writerTree := t.TempDir()
	writeFile(t, filepath.Join(writerTree, "_tfac", "notes.txt"), "the work the writer was still storing")
	persisted := make(chan error, 1)
	go func() {
		time.Sleep(30 * time.Millisecond)
		persisted <- s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, conversationID,
			namespace, writerClaim, writerTree, "", domain.ConversationRuntimeNative)
	}()
	t.Cleanup(func() {
		if err := <-persisted; err != nil {
			t.Errorf("the writer's persist failed, so the wait ended for the wrong reason: %v", err)
		}
	})

	conv := &domain.Conversation{
		ID: conversationID, BlueprintRunID: namespace, Runtime: domain.ConversationRuntimeNative,
		WorktreePath: filepath.Join(t.TempDir(), "swept-away"),
	}
	got, prov, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, conv, gitSeed{}, failingFreshBuilder(t))
	if err != nil {
		t.Fatalf("ensureWorkspace: %v", err)
	}
	if prov != domain.WorkspaceProvenanceRehydrated {
		t.Errorf("provenance = %q, want rehydrated — the wait exists so a resume gets the real workspace", prov)
	}
	if got != worktree.RunRoot(namespace) {
		t.Errorf("cwd = %q, want the run root for %s", got, namespace)
	}
}

// TestEnsureWorkspace_FallsBackWhenTheWriterIsGone is the other end of the
// wait: the engagement that owed the snapshot died without writing it, and the
// runtimes part company — native gets a workspace built from nothing plus an
// honest provenance, SDK gets the expired answer because the session file it
// would resume from was inside the blob.
func TestEnsureWorkspace_FallsBackWhenTheWriterIsGone(t *testing.T) {
	for _, runtime := range []string{"native", "sdk"} {
		t.Run(runtime, func(t *testing.T) {
			paths.SetForTest(t, t.TempDir())
			setupGitTestEnv(t)
			s, database, conversationID, _ := setupAdvanceFixture(t, "wait-dead-"+runtime)
			wireBlobStore(t, s)
			s.snapshotWaitPollInterval = 5 * time.Millisecond
			namespace := blueprintRunIDForConversation(t, database, conversationID)

			const writerClaim = "claim-dead-writer"
			seedSnapshotState(t, s, namespace, writerClaim, domain.WorkspaceSnapshotPending)
			stageClaim(t, database, conversationID, writerClaim, "exec-dead")
			// Last beat well outside the liveness window: the executor is gone,
			// so nothing is producing this blob however long anyone waits.
			stageInstance(t, database, "exec-dead", time.Now().Add(-time.Hour))

			convRuntime := domain.ConversationRuntimeNative
			if runtime == "sdk" {
				convRuntime = domain.ConversationRuntimeSDK
			}
			conv := &domain.Conversation{
				ID: conversationID, BlueprintRunID: namespace, Runtime: convRuntime,
				WorktreePath: filepath.Join(t.TempDir(), "swept-away"),
			}
			freshDir := t.TempDir()
			built := 0
			fresh := func(context.Context) (string, error) { built++; return freshDir, nil }

			started := time.Now()
			got, prov, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, conv, gitSeed{}, fresh)
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Errorf("waited %s on a dead writer; the liveness read is supposed to end the wait long before the bound", elapsed)
			}

			if runtime == "sdk" {
				if !errors.Is(err, ErrWorkspaceExpired) {
					t.Fatalf("err = %v, want ErrWorkspaceExpired — an SDK resume has no transcript without the blob", err)
				}
				if built != 0 {
					t.Error("built a fresh workspace for an SDK conversation; there is nothing for --resume to reconnect to in it")
				}
				return
			}
			if err != nil {
				t.Fatalf("ensureWorkspace: %v", err)
			}
			if prov != domain.WorkspaceProvenanceFresh {
				t.Errorf("provenance = %q, want fresh — the agent is told what a from-nothing tree cost it", prov)
			}
			if got != freshDir || built != 1 {
				t.Errorf("cwd = %q (built %d times), want the freshly built tree %q once", got, built, freshDir)
			}
			// The fresh tree is where the next claim has to look, and the
			// re-stamp is this engagement's write to make.
			var stored string
			if err := database.QueryRow(`SELECT COALESCE(worktree_path,'') FROM conversations WHERE id = ?`, conversationID).Scan(&stored); err != nil {
				t.Fatalf("read worktree_path: %v", err)
			}
			if stored != freshDir {
				t.Errorf("worktree_path = %q, want the fresh tree %q", stored, freshDir)
			}
		})
	}
}

// TestEnsureWorkspace_HonorsTheWaitCap covers the writer that neither finishes
// nor dies: its executor keeps heartbeating and its record stays pending, so
// the bound is the only thing left to end the wait — which is what
// TF_SNAPSHOT_WAIT_SEC configures and why it exists.
func TestEnsureWorkspace_HonorsTheWaitCap(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s, database, conversationID, _ := setupAdvanceFixture(t, "wait-cap")
	wireBlobStore(t, s)
	s.snapshotWaitPollInterval = 5 * time.Millisecond
	s.SetSnapshotWaitTimeout(50 * time.Millisecond)
	namespace := blueprintRunIDForConversation(t, database, conversationID)

	const writerClaim = "claim-hung-writer"
	seedSnapshotState(t, s, namespace, writerClaim, domain.WorkspaceSnapshotPending)
	stageClaim(t, database, conversationID, writerClaim, "exec-hung")
	stageInstance(t, database, "exec-hung", time.Now())

	conv := &domain.Conversation{
		ID: conversationID, BlueprintRunID: namespace, Runtime: domain.ConversationRuntimeNative,
		WorktreePath: filepath.Join(t.TempDir(), "swept-away"),
	}
	freshDir := t.TempDir()
	started := time.Now()
	_, prov, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, conv, gitSeed{},
		func(context.Context) (string, error) { return freshDir, nil })
	if err != nil {
		t.Fatalf("ensureWorkspace: %v", err)
	}
	if prov != domain.WorkspaceProvenanceFresh {
		t.Errorf("provenance = %q, want fresh — the bound is what stops a hung writer holding a resume forever", prov)
	}
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond {
		t.Errorf("returned after %s, before the configured bound elapsed; the wait was not really taken", elapsed)
	}
}

// TestParseSnapshotWaitTimeout pins the knob's contract, including the one
// value it deliberately refuses to honor.
func TestParseSnapshotWaitTimeout(t *testing.T) {
	cases := []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{raw: "", want: DefaultSnapshotWait},
		{raw: " 120 ", want: 2 * time.Minute},
		{raw: "0", want: DefaultSnapshotWait, wantErr: true},
		{raw: "-5", want: DefaultSnapshotWait, wantErr: true},
		{raw: "soon", want: DefaultSnapshotWait, wantErr: true},
	}
	for _, tc := range cases {
		got, err := ParseSnapshotWaitTimeout(tc.raw)
		if got != tc.want {
			t.Errorf("ParseSnapshotWaitTimeout(%q) = %s, want %s", tc.raw, got, tc.want)
		}
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseSnapshotWaitTimeout(%q) err = %v, wantErr %v", tc.raw, err, tc.wantErr)
		}
	}
}

// --- fixtures ---

// flakyBeginSnapshotStore fails the first attempt to open a key's lifecycle
// record and counts every attempt, so a test can drive the park's
// best-effort open into the persist's retry.
type flakyBeginSnapshotStore struct {
	db.WorkspaceSnapshotStore
	failFirst bool
	begins    int
}

func (f *flakyBeginSnapshotStore) BeginSnapshotSystem(ctx context.Context, orgID, blueprintRunID, claimID string) error {
	f.begins++
	if f.failFirst {
		f.failFirst = false
		return errors.New("workspace_snapshots write failed")
	}
	return f.WorkspaceSnapshotStore.BeginSnapshotSystem(ctx, orgID, blueprintRunID, claimID)
}

// heldPutStorage blocks inside Put until released, so a test can hold a
// persist open and assert what the rest of the system reads while it runs.
type heldPutStorage struct {
	storage.Storage
	entered chan struct{}
	release chan struct{}
}

func (h *heldPutStorage) Put(ctx context.Context, key string, r io.Reader) error {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	<-h.release
	return h.Storage.Put(ctx, key, r)
}

// onPutStorage runs a callback at the moment of upload — the one instant from
// which the flip-before-persist ordering is observable.
type onPutStorage struct {
	storage.Storage
	onPut func()
}

func (o *onPutStorage) Put(ctx context.Context, key string, r io.Reader) error {
	o.onPut()
	return o.Storage.Put(ctx, key, r)
}

// failingFreshBuilder is the builder a test hands ensureWorkspace when the
// fallback arm must NOT be reached: taking it is the failure.
func failingFreshBuilder(t *testing.T) freshWorkspaceBuilder {
	t.Helper()
	return func(context.Context) (string, error) {
		t.Error("built a fresh workspace; the snapshot this resume was waiting for did land")
		return "", errors.New("fresh workspace builder must not be reached")
	}
}

// stageClaim mints a released claim — the shape a snapshot's writer has by the
// time anyone waits on its blob.
func stageClaim(t *testing.T, database *sql.DB, conversationID, claimID, executorID string) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch, claimed_at, released_at, outcome)
		VALUES (?, ?, ?, ?, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 'parked')
	`, claimID, runmode.LocalDefaultOrgID, conversationID, executorID); err != nil {
		t.Fatalf("stage claim %s: %v", claimID, err)
	}
}

// stageInstance registers an executor with a chosen last heartbeat, the only
// input to "is that writer still going".
func stageInstance(t *testing.T, database *sql.DB, executorID string, lastBeat time.Time) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT INTO instances (id, role, version, boot_epoch, started_at, last_heartbeat_at)
		VALUES (?, 'executor', 'test', 1, ?, ?)
	`, executorID, lastBeat.UTC(), lastBeat.UTC()); err != nil {
		t.Fatalf("stage instance %s: %v", executorID, err)
	}
}
