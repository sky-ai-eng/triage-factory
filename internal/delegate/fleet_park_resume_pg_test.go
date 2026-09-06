package delegate

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// The park-first contract composed at the fleet level: two executor identities
// and a control pod against one Postgres, one shared object store, and a real
// git run tree with uncommitted work in it. Each child of the epic proved its
// own piece in isolation; these scenarios prove the pieces compose — a stop
// that arrives from another pod, a persist still uploading while the next
// engagement claims, a resume that lands on the executor that does not hold
// the tree, a sweep that reclaims a tree the blob has to stand in for.
//
// Two executors in one process share a filesystem, which a fleet does not.
// Where a scenario needs the tree to exist on X and not on Y, it deletes the
// tree after X's capture has read it: from Y's point of view that is exactly
// what a tree on another machine looks like.

// parkFleet is the fixture every scenario in this file starts from.
type parkFleet struct {
	fleetFixture
	h *pgtest.Harness
	// x and y are the executors; control is the pod a stop and a follow-up
	// arrive on, which holds no process and no tree.
	x, y, control *Spawner
	blobs         storage.Storage
	gate          *gatedPutStorage
	// wtPath is the run tree, keyed by the blueprint run id — one tree for
	// every step of the blueprint, and the snapshot key.
	wtPath, owner, repo, keyID string
}

func seedParkFleet(t *testing.T) *parkFleet {
	t.Helper()
	isolateRunNamespace(t)
	setupGitTestEnv(t)
	h := pgtest.Shared(t)
	h.Reset(t)
	fx := seedFleetFixture(t, h)

	wtPath, owner, repo := setupTestWorktree(t, fx.brID)
	t.Cleanup(func() { _ = worktree.RemoveAt(wtPath, fx.brID) })
	// The agent's remembered work: an uncommitted edit (the patch member) and
	// scratch under _tfac (the tar members). Both have to come back from the
	// blob for a cold resume to be the same conversation.
	writeFile(t, filepath.Join(wtPath, "README.md"), "hello\nuncommitted edit\n")
	writeFile(t, filepath.Join(wtPath, "_tfac", "notes.md"), "half-finished work")
	pgtest.MustExec(t, h.AdminDB, `UPDATE blueprint_runs SET worktree_path = $2 WHERE id = $1`, fx.brID, wtPath)
	pgtest.MustExec(t, h.AdminDB, `UPDATE conversations SET worktree_path = $2 WHERE id = $1`, fx.conversationID, wtPath)
	// A real resume is of a conversation minted long before the aging window:
	// every scenario here runs on that shape, so the affinity a wake stamps has
	// to open its exclusive window from the wake, not from a mint the fixture
	// happens to have made milliseconds ago.
	pgtest.MustExec(t, h.AdminDB,
		`UPDATE conversations SET started_at = now() - interval '10 minutes', queued_at = now() - interval '10 minutes' WHERE id = $1`,
		fx.conversationID)

	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	gate := &gatedPutStorage{Storage: blobs, entered: make(chan struct{}, 1), release: make(chan struct{})}

	placement := db.ClaimPlacement{Enabled: true, AgingInterval: 20 * time.Second, Liveness: 12 * time.Second}
	short := uuid.New().String()[:8]
	x := newFleetSpawner(t, h, fx, "park-x-"+short)
	y := newFleetSpawner(t, h, fx, "park-y-"+short)
	control := NewSpawner(h.AdminDB, fx.stores, nil, nil, "")
	for _, s := range []*Spawner{x, y, control} {
		s.SetStorage(gate)
		s.SetConversationSignals(fx.stores.ConversationSignals, nil)
		s.snapshotWaitPollInterval = 5 * time.Millisecond
		s.SetSnapshotWaitTimeout(10 * time.Second)
	}
	x.SetPlacement(nil, placement)
	y.SetPlacement(nil, placement)

	return &parkFleet{
		fleetFixture: fx, h: h, x: x, y: y, control: control,
		blobs: blobs, gate: gate,
		wtPath: wtPath, owner: owner, repo: repo, keyID: fx.brID,
	}
}

// liveEngagement is one executor's live claim on the fixture's conversation: the
// cancel handle a stop reaches, and the teardown that follows the kill — the
// same parkConversationOpen a killed run's goroutine takes, with its cwd the
// real tree.
type liveEngagement struct {
	conv   *domain.Conversation
	ctx    context.Context
	done   chan struct{}
	fenced bool
}

func (f *parkFleet) engage(t *testing.T, s *Spawner) *liveEngagement {
	t.Helper()
	claimed := f.claim(t, s)
	if claimed == nil {
		t.Fatal("the executor could not claim the fixture's conversation")
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancels[claimed.ID] = cancel
	s.mu.Unlock()
	e := &liveEngagement{conv: claimed, ctx: runCtx, done: make(chan struct{})}
	go func() {
		defer close(e.done)
		<-runCtx.Done()
		s.mu.Lock()
		delete(s.cancels, claimed.ID)
		s.mu.Unlock()
		e.fenced = s.parkConversationOpen(context.Background(), liveParkContext{
			orgID:          f.orgID,
			conversationID: claimed.ID,
			taskID:         claimed.TaskID,
			namespace:      f.keyID,
			claudeCwd:      f.wtPath,
			triggerType:    claimed.TriggerType,
			creatorUserID:  f.userID,
			claimID:        claimed.ClaimID,
			runtime:        claimed.Runtime,
			reason:         db.ParkStopped(domain.ParkReasonUserCancelled, ""),
		}, "")
	}()
	return e
}

// stopFromControl is the cross-pod stop: the verb runs on a pod with no
// process handle, parks the row, and hastens the kill with a signal that X's
// own drain applies. Returns once the kill has reached X's engagement.
func (f *parkFleet) stopFromControl(t *testing.T, e *liveEngagement) {
	t.Helper()
	if err := f.control.Stop(f.orgID, f.conversationID, f.userID); err != nil {
		t.Fatalf("stop from control: %v", err)
	}
	// The signal is inserted by a goroutine the verb does not wait on, so
	// the drain is pumped rather than called once.
	deadline := time.Now().Add(5 * time.Second)
	for e.ctx.Err() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the cancel signal never reached executor X")
		}
		f.x.drainSignals(context.Background())
		time.Sleep(10 * time.Millisecond)
	}
}

// parkIdle is an engagement's own park, run to completion: the turn ended
// with nothing further to do.
func (f *parkFleet) parkIdle(t *testing.T, s *Spawner, conv *domain.Conversation, cwd string) {
	t.Helper()
	if fenced := s.parkConversationOpen(context.Background(), liveParkContext{
		orgID:          f.orgID,
		conversationID: conv.ID,
		taskID:         conv.TaskID,
		namespace:      f.keyID,
		claudeCwd:      cwd,
		triggerType:    conv.TriggerType,
		creatorUserID:  f.userID,
		claimID:        conv.ClaimID,
		runtime:        conv.Runtime,
		reason:         db.ParkIdle(),
	}, ""); fenced {
		t.Fatalf("the idle park under claim %s was refused by the fence", conv.ClaimID)
	}
}

func (f *parkFleet) claim(t *testing.T, s *Spawner) *domain.Conversation {
	t.Helper()
	id, epoch := s.executorIdentity()
	got, err := f.stores.ConversationQueue.ClaimNextConversation(context.Background(), id, epoch, s.claimPlacement())
	if err != nil {
		t.Fatalf("claim by %s: %v", id, err)
	}
	if got != nil && got.ID != f.conversationID {
		t.Fatalf("claim by %s took %s, want the fixture's conversation %s", id, got.ID, f.conversationID)
	}
	return got
}

func (f *parkFleet) followUp(text string) error {
	return f.control.SendMessage(context.Background(), f.orgID, f.conversationID, f.userID, text)
}

// read is the display read — the row as the board and the station see it.
func (f *parkFleet) read(t *testing.T) *domain.Conversation {
	t.Helper()
	conv, err := f.stores.Conversations.GetSystem(context.Background(), f.orgID, f.conversationID)
	if err != nil || conv == nil {
		t.Fatalf("read conversation: %v", err)
	}
	return conv
}

func (f *parkFleet) activeClaims(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.h.AdminDB.QueryRow(`SELECT count(*) FROM claims WHERE conversation_id = $1 AND released_at IS NULL`, f.conversationID).Scan(&n); err != nil {
		t.Fatalf("count active claims: %v", err)
	}
	return n
}

func (f *parkFleet) preferredExecutor(t *testing.T) string {
	t.Helper()
	var id string
	if err := f.h.AdminDB.QueryRow(`SELECT COALESCE(preferred_executor_id, '') FROM conversations WHERE id = $1`, f.conversationID).Scan(&id); err != nil {
		t.Fatalf("read preferred executor: %v", err)
	}
	return id
}

func (f *parkFleet) storedWorktreePath(t *testing.T) string {
	t.Helper()
	var p string
	if err := f.h.AdminDB.QueryRow(`SELECT COALESCE(worktree_path, '') FROM conversations WHERE id = $1`, f.conversationID).Scan(&p); err != nil {
		t.Fatalf("read worktree_path: %v", err)
	}
	return p
}

func (f *parkFleet) snapshotState(t *testing.T) *domain.WorkspaceSnapshotState {
	t.Helper()
	state, err := f.stores.WorkspaceSnapshots.GetSnapshotStateSystem(context.Background(), f.orgID, f.keyID)
	if err != nil {
		t.Fatalf("read snapshot state: %v", err)
	}
	return state
}

func (f *parkFleet) assertState(t *testing.T, wantState, wantWriter string) {
	t.Helper()
	got := f.snapshotState(t)
	if got == nil {
		t.Fatalf("no snapshot state for %s, want %s under %s", f.keyID, wantState, wantWriter)
	}
	if got.State != wantState || got.WriterClaimID != wantWriter {
		t.Errorf("snapshot state = {%s, %s}, want {%s, %s}", got.State, got.WriterClaimID, wantState, wantWriter)
	}
}

func (f *parkFleet) blobPresent(t *testing.T) bool {
	t.Helper()
	ok, err := f.blobs.Exists(context.Background(), snapshotKey(f.orgID, f.keyID))
	if err != nil {
		t.Fatalf("blob exists: %v", err)
	}
	return ok
}

func (f *parkFleet) pendingInput(t *testing.T) []domain.Message {
	t.Helper()
	rows, err := f.stores.Conversations.ListForAssemblySystem(context.Background(), f.orgID, f.conversationID)
	if err != nil {
		t.Fatalf("list transcript: %v", err)
	}
	var out []domain.Message
	for _, r := range rows {
		if r.Delivered != nil && !*r.Delivered {
			out = append(out, r)
		}
	}
	return out
}

// deliver flushes the pending input under the engagement's claim — the
// engine's first drain, which is what turns a resumed conversation's display
// from queued to running and lets the next park read as parked rather than
// as still owing a turn.
func (f *parkFleet) deliver(t *testing.T, conv *domain.Conversation) []domain.Message {
	t.Helper()
	rows := f.pendingInput(t)
	if len(rows) == 0 {
		return nil
	}
	ids := make([]int, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	if err := f.stores.Conversations.MarkDeliveredForClaimSystem(context.Background(), f.orgID, f.conversationID, conv.ClaimID, ids, ""); err != nil {
		t.Fatalf("deliver under claim %s: %v", conv.ClaimID, err)
	}
	return rows
}

func (f *parkFleet) transcriptHas(t *testing.T, content string) bool {
	t.Helper()
	var n int
	if err := f.h.AdminDB.QueryRow(`SELECT count(*) FROM messages WHERE conversation_id = $1 AND content = $2`, f.conversationID, content).Scan(&n); err != nil {
		t.Fatalf("search transcript: %v", err)
	}
	return n > 0
}

// removeTree is "the tree is on another machine": Y's disk has no copy.
func (f *parkFleet) removeTree(t *testing.T) {
	t.Helper()
	if err := worktree.RemoveAt(f.wtPath, f.keyID); err != nil {
		t.Fatalf("remove run tree: %v", err)
	}
	if _, err := os.Stat(f.wtPath); !os.IsNotExist(err) {
		t.Fatalf("run tree still present after removal: %v", err)
	}
}

// awaitUpload blocks until the held persist is inside its upload: capture
// done, blob not yet written, record pending. The window every scenario in
// this file is about.
func (f *parkFleet) awaitUpload(t *testing.T) {
	t.Helper()
	select {
	case <-f.gate.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the teardown never reached its upload")
	}
	if f.blobPresent(t) {
		t.Fatal("blob present before the upload was released; the window under test does not exist")
	}
}

func (f *parkFleet) ensureOn(t *testing.T, s *Spawner, conv *domain.Conversation, fresh freshWorkspaceBuilder) (string, domain.WorkspaceProvenance, error) {
	t.Helper()
	return s.ensureWorkspace(context.Background(), f.orgID, &domain.Conversation{
		ID:             f.conversationID,
		ClaimID:        conv.ClaimID,
		WorktreePath:   f.storedWorktreePath(t),
		BlueprintRunID: f.keyID,
		Runtime:        conv.Runtime,
	}, gitSeed{owner: f.owner, repo: f.repo}, fresh)
}

// assertInvariants carries the cross-cutting checks through every scenario:
// no engagement is left holding a parked conversation, and no record says a
// persist is in flight for a blob that already exists under a writer nobody
// will hear from again — the shape a resume would wait out for nothing.
func (f *parkFleet) assertInvariants(t *testing.T) {
	t.Helper()
	var parkedButClaimed int
	if err := f.h.AdminDB.QueryRow(`
		SELECT count(*) FROM claims cl
		JOIN conversations c ON c.id = cl.conversation_id
		WHERE cl.released_at IS NULL AND c.status = 'open'
	`).Scan(&parkedButClaimed); err != nil {
		t.Fatalf("invariant read: %v", err)
	}
	if parkedButClaimed != 0 {
		t.Errorf("%d claim(s) active on a parked conversation", parkedButClaimed)
	}
	state := f.snapshotState(t)
	if state == nil || state.State != domain.WorkspaceSnapshotPending {
		return
	}
	if !f.blobPresent(t) {
		return
	}
	if !f.x.snapshotWriterAlive(context.Background(), f.orgID, state.WriterClaimID) {
		t.Errorf("snapshot state is pending under dead writer %s while the blob exists", state.WriterClaimID)
	}
}

// gatedPutStorage holds the next upload open until released, so a scenario
// can assert what the rest of the fleet reads while a persist is provably in
// flight. One-shot: uploads after the held one pass straight through, so a
// successor's own park is never caught in the gate. The hold yields to the
// upload's own context, so a persist that is cancelled while held fails the
// way a real store would rather than pinning the suite.
type gatedPutStorage struct {
	storage.Storage
	mu      sync.Mutex
	armed   bool
	fail    bool
	entered chan struct{}
	release chan struct{}
}

// hold arms the gate for the next upload.
func (g *gatedPutStorage) hold() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed = true
	g.release = make(chan struct{})
}

// let lets the held upload land.
func (g *gatedPutStorage) let() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fail = false
	close(g.release)
}

// abort ends the held upload with a store error instead — the persist that
// never lands.
func (g *gatedPutStorage) abort() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fail = true
	close(g.release)
}

func (g *gatedPutStorage) Put(ctx context.Context, key string, r io.Reader) error {
	g.mu.Lock()
	armed := g.armed
	g.armed = false
	release := g.release
	g.mu.Unlock()
	if !armed {
		return g.Storage.Put(ctx, key, r)
	}
	select {
	case g.entered <- struct{}{}:
	default:
	}
	select {
	case <-release:
	case <-ctx.Done():
		return ctx.Err()
	}
	g.mu.Lock()
	fail := g.fail
	g.mu.Unlock()
	if fail {
		return errors.New("object store refused the upload")
	}
	return g.Storage.Put(ctx, key, r)
}

// --- scenarios ---------------------------------------------------------------

// TestFleet_ParkFirst_StopParksBeforeTheExecutorPersists is the instant park
// and the steer that follows it, across pods: control parks the row the
// moment the user asks, the executor's teardown arrives after and records the
// persist it owes, a follow-up typed while that persist is still uploading is
// queued rather than refused, and the next claim delivers it. The late fenced
// flip is refused, and refused quietly — it is the ordinary shape of every
// cross-pod stop.
func TestFleet_ParkFirst_StopParksBeforeTheExecutorPersists(t *testing.T) {
	f := seedParkFleet(t)
	first := f.engage(t, f.x)
	f.gate.hold()

	f.stopFromControl(t, first)
	// The verb's own answer, before the executor has done anything: the
	// board derives IDLE from exactly this read.
	if got := f.read(t).Status; got != domain.StatusOpen {
		t.Fatalf("display status after the stop = %q, want open — the flip must not wait on the executor", got)
	}
	if f.activeClaims(t) != 0 {
		t.Error("the claim is still live after the stop; the executor slot stays occupied until the teardown finishes")
	}
	if !f.transcriptHas(t, stopNoteByUser) {
		t.Error("the stop note is not on the transcript")
	}

	f.awaitUpload(t)
	f.assertState(t, domain.WorkspaceSnapshotPending, first.conv.ClaimID)
	if got := f.read(t).Status; got != domain.StatusOpen {
		t.Errorf("display status during the persist = %q, want open", got)
	}

	// Steer-after-stop, inside the window.
	if err := f.followUp("actually, try the other approach"); err != nil {
		t.Fatalf("follow-up during the persist: %v (a 409/410 here is the bug the epic exists for)", err)
	}
	if got := f.read(t).Status; got != domain.StatusQueued {
		t.Errorf("display status after the follow-up = %q, want queued", got)
	}
	if got := f.pendingInput(t); len(got) != 1 {
		t.Fatalf("pending rows = %d, want the follow-up waiting for the next claim", len(got))
	}
	xID, _ := f.x.executorIdentity()
	if got := f.preferredExecutor(t); got != xID {
		t.Errorf("preferred executor after the resume = %q, want X (%s), where the tree is", got, xID)
	}

	f.gate.let()
	<-first.done
	if !first.fenced {
		t.Error("the killed engagement's late flip was not refused; it would re-park a conversation the user already resumed")
	}
	f.assertState(t, domain.WorkspaceSnapshotWritten, first.conv.ClaimID)
	if !f.blobPresent(t) {
		t.Error("the teardown's blob never landed")
	}
	if got := f.read(t).Status; got != domain.StatusQueued {
		t.Errorf("display status after the refused flip = %q, want queued — the fence is what keeps the resume from being undone", got)
	}

	// The subsequent claim delivers it.
	next := f.claim(t, f.x)
	if next == nil {
		t.Fatal("X could not claim the resumed conversation")
	}
	rows := f.deliver(t, next)
	if len(rows) != 1 || rows[0].Content != "actually, try the other approach" {
		t.Fatalf("pending input at claim = %+v, want the follow-up", rows)
	}
	if got := f.pendingInput(t); len(got) != 0 {
		t.Errorf("pending rows after delivery = %d, want 0", len(got))
	}
	if got := f.read(t).Status; got != domain.StatusRunning {
		t.Errorf("display status under the new claim = %q, want running", got)
	}

	f.parkIdle(t, f.x, next, f.wtPath)
	f.assertState(t, domain.WorkspaceSnapshotWritten, next.ClaimID)
	f.assertInvariants(t)
}

// TestFleet_WarmResume_SameExecutorNeverWaitsOnItsOwnPersist is the common
// resume: stop, then "wait, one more thing" seconds later. The re-stamped
// affinity puts the claim back on X, whose tree is still on disk, so the
// workspace resolves warm with no wait at all — while the predecessor's
// persist is still uploading. That persist completes without disturbing the
// live tree, and the successor's own park then supersedes it.
func TestFleet_WarmResume_SameExecutorNeverWaitsOnItsOwnPersist(t *testing.T) {
	f := seedParkFleet(t)
	first := f.engage(t, f.x)
	f.gate.hold()
	f.stopFromControl(t, first)
	f.awaitUpload(t)

	if err := f.followUp("one more thing"); err != nil {
		t.Fatalf("follow-up: %v", err)
	}
	xID, _ := f.x.executorIdentity()
	if got := f.preferredExecutor(t); got != xID {
		t.Fatalf("preferred executor = %q, want X (%s)", got, xID)
	}
	// Inside its affinity window the conversation is X's alone. The window
	// opened at the wake, so the conversation's age is irrelevant.
	if got := f.claim(t, f.y); got != nil {
		t.Fatal("Y claimed the resumed conversation inside the aging window while X is live")
	}
	next := f.claim(t, f.x)
	if next == nil {
		t.Fatal("X could not claim its own resumed conversation at once")
	}
	f.deliver(t, next)

	spans := recordSpans(t)
	started := time.Now()
	cwd, prov, err := f.ensureOn(t, f.x, next, failingFreshBuilder(t))
	if err != nil {
		t.Fatalf("ensureWorkspace on X: %v", err)
	}
	if prov != domain.WorkspaceProvenanceWarm || cwd != f.wtPath {
		t.Fatalf("workspace = (%q, %q), want the warm tree %q", cwd, prov, f.wtPath)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("warm resolution took %s; it must not wait on the predecessor's persist", elapsed)
	}
	ensure := spansNamed(spans(), "engagement.workspace.ensure")
	if len(ensure) != 1 {
		t.Fatalf("engagement.workspace.ensure spans = %d, want 1", len(ensure))
	}
	if got := spanAttr(t, ensure[0], "workspace.provenance").AsString(); got != string(domain.WorkspaceProvenanceWarm) {
		t.Errorf("span provenance = %q, want warm", got)
	}
	for _, kv := range ensure[0].Attributes() {
		if string(kv.Key) == "snapshot.waited_ms" {
			t.Errorf("warm span carries snapshot.waited_ms = %v; nothing was waited for", kv.Value)
		}
	}

	// The successor works in the tree while the predecessor is still
	// uploading a picture of it.
	writeFile(t, filepath.Join(f.wtPath, "_tfac", "successor.md"), "written by the second engagement")
	f.gate.let()
	<-first.done
	f.assertState(t, domain.WorkspaceSnapshotWritten, first.conv.ClaimID)
	if !f.blobPresent(t) {
		t.Error("the predecessor's blob never landed")
	}
	assertFileContains(t, filepath.Join(f.wtPath, "_tfac", "successor.md"), "written by the second engagement")
	assertFileContains(t, filepath.Join(f.wtPath, "README.md"), "uncommitted edit")

	// The successor's park re-owns the key: its record, its blob.
	f.parkIdle(t, f.x, next, f.wtPath)
	f.assertState(t, domain.WorkspaceSnapshotWritten, next.ClaimID)
	if got := f.read(t).Status; got != domain.StatusOpen || f.activeClaims(t) != 0 {
		t.Fatalf("after the idle park: status %q, active claims %d", got, f.activeClaims(t))
	}

	// And that blob is the one a cold resume gets: sweep the tree, resume,
	// find the successor's work in the rebuilt one.
	f.removeTree(t)
	if err := f.followUp("and another"); err != nil {
		t.Fatalf("follow-up after the sweep: %v", err)
	}
	again := f.claim(t, f.x)
	if again == nil {
		t.Fatal("X could not claim after the sweep")
	}
	f.deliver(t, again)
	cwd, prov, err = f.ensureOn(t, f.x, again, failingFreshBuilder(t))
	if err != nil {
		t.Fatalf("ensureWorkspace after the sweep: %v", err)
	}
	if prov != domain.WorkspaceProvenanceRehydrated {
		t.Errorf("provenance after the sweep = %q, want rehydrated", prov)
	}
	assertFileContains(t, filepath.Join(cwd, "_tfac", "successor.md"), "written by the second engagement")
	assertFileContains(t, filepath.Join(cwd, "README.md"), "uncommitted edit")

	f.parkIdle(t, f.x, again, cwd)
	f.assertInvariants(t)
}

// TestFleet_CrossExecutorResume_WaitsOutTheLiveWriter: the resume lands on Y,
// whose disk has no tree, while X is still uploading the snapshot Y needs.
// The record says pending under a writer whose heartbeat is live, so Y waits;
// the blob lands mid-wait; Y rehydrates from it with the uncommitted delta
// intact, and its span says how long that took.
func TestFleet_CrossExecutorResume_WaitsOutTheLiveWriter(t *testing.T) {
	f := seedParkFleet(t)
	first := f.engage(t, f.x)
	f.gate.hold()
	f.stopFromControl(t, first)
	f.awaitUpload(t)
	f.removeTree(t)

	if err := f.followUp("carry on, wherever you land"); err != nil {
		t.Fatalf("follow-up: %v", err)
	}
	// X is draining, so placement spills the resume to Y at once; the
	// writer's heartbeat stays live, so the persist is still coming.
	xID, _ := f.x.executorIdentity()
	if matched, err := f.stores.Instances.SetDraining(context.Background(), xID, true); err != nil || !matched {
		t.Fatalf("drain X: matched=%v err=%v", matched, err)
	}
	next := f.claim(t, f.y)
	if next == nil {
		t.Fatal("Y could not claim the resume of a draining executor's conversation")
	}
	f.deliver(t, next)

	spans := recordSpans(t)
	go func() {
		time.Sleep(150 * time.Millisecond)
		f.gate.let()
	}()
	cwd, prov, err := f.ensureOn(t, f.y, next, failingFreshBuilder(t))
	if err != nil {
		t.Fatalf("ensureWorkspace on Y: %v", err)
	}
	if prov != domain.WorkspaceProvenanceRehydrated {
		t.Fatalf("provenance on Y = %q, want rehydrated — the wait exists so the resume gets the real workspace", prov)
	}
	if cwd != worktree.RunRoot(f.keyID) {
		t.Errorf("cwd on Y = %q, want the run root for %s", cwd, f.keyID)
	}
	assertFileContains(t, filepath.Join(cwd, "README.md"), "uncommitted edit")
	assertFileContains(t, filepath.Join(cwd, "_tfac", "notes.md"), "half-finished work")
	<-first.done
	f.assertState(t, domain.WorkspaceSnapshotWritten, first.conv.ClaimID)

	ensure := spansNamed(spans(), "engagement.workspace.ensure")
	if len(ensure) != 1 {
		t.Fatalf("engagement.workspace.ensure spans = %d, want 1", len(ensure))
	}
	if got := spanAttr(t, ensure[0], "workspace.provenance").AsString(); got != string(domain.WorkspaceProvenanceRehydrated) {
		t.Errorf("span provenance = %q, want rehydrated", got)
	}
	if waited := spanAttr(t, ensure[0], "snapshot.waited_ms").AsInt64(); waited < 100 {
		t.Errorf("span snapshot.waited_ms = %d, want the wait it actually sat out (>= 100)", waited)
	}
	// The one fact the notice cannot read off the tree: the predecessor ran
	// elsewhere.
	if !f.y.executorChangedSince(context.Background(), f.orgID, f.conversationID, next.ClaimID, prov) {
		t.Error("Y does not report the executor changed; the rebuilt-workspace notice would understate what moved")
	}

	f.parkIdle(t, f.y, next, cwd)
	f.assertInvariants(t)
}

// TestFleet_CrossExecutorResume_FallsBackWhenTheWriterIsDead is the other end
// of the wait: the executor that owed the snapshot stopped heartbeating with
// its upload never landing. The runtimes part company. A native conversation
// keeps its continuity in the transcript, so it is rebuilt from nothing on Y,
// told so, and re-stamped there. An SDK conversation's continuity was the
// session file inside the blob, so the wake gate refuses the follow-up as
// expired rather than accept a message no claim could act on — and the
// claim-time backstop answers the same way.
func TestFleet_CrossExecutorResume_FallsBackWhenTheWriterIsDead(t *testing.T) {
	for _, runtime := range []string{domain.ConversationRuntimeNative, domain.ConversationRuntimeSDK} {
		t.Run(runtime, func(t *testing.T) {
			f := seedParkFleet(t)
			if runtime == domain.ConversationRuntimeSDK {
				pgtest.MustExec(t, f.h.AdminDB, `UPDATE conversations SET runtime = 'sdk', sdk_session_id = 'sess-dead' WHERE id = $1`, f.conversationID)
			}
			first := f.engage(t, f.x)
			f.gate.hold()
			f.stopFromControl(t, first)
			f.awaitUpload(t)
			f.removeTree(t)
			xID, _ := f.x.executorIdentity()
			backdateFleetHeartbeat(t, f.h, xID, time.Hour)

			err := f.followUp("are you still there")
			if runtime == domain.ConversationRuntimeSDK {
				if !errors.Is(err, ErrWorkspaceExpired) {
					t.Fatalf("follow-up on an SDK conversation with a dead writer = %v, want ErrWorkspaceExpired at the wake gate", err)
				}
				if got := f.read(t).Status; got != domain.StatusOpen {
					t.Errorf("display status after the refusal = %q, want open — a refused wake moves nothing", got)
				}
				if got := f.pendingInput(t); len(got) != 0 {
					t.Errorf("pending rows after the refusal = %d, want 0", len(got))
				}
				// The claim-time backstop, for a claim that slipped past the gate.
				if _, _, err := f.ensureOn(t, f.y, &domain.Conversation{Runtime: runtime}, nil); !errors.Is(err, ErrWorkspaceExpired) {
					t.Errorf("ensureWorkspace for the SDK conversation = %v, want ErrWorkspaceExpired", err)
				}
				f.gate.abort()
				<-first.done
				f.assertState(t, domain.WorkspaceSnapshotFailed, first.conv.ClaimID)
				f.assertInvariants(t)
				return
			}
			if err != nil {
				t.Fatalf("follow-up on a native conversation with a dead writer: %v", err)
			}
			next := f.claim(t, f.y)
			if next == nil {
				t.Fatal("Y could not claim the resume of a dead executor's conversation")
			}
			f.deliver(t, next)

			spans := recordSpans(t)
			freshDir := t.TempDir()
			built := 0
			started := time.Now()
			cwd, prov, err := f.ensureOn(t, f.y, next, func(context.Context) (string, error) { built++; return freshDir, nil })
			if err != nil {
				t.Fatalf("ensureWorkspace on Y: %v", err)
			}
			if elapsed := time.Since(started); elapsed > 3*time.Second {
				t.Errorf("waited %s on a dead writer; the liveness read is supposed to end the wait long before the bound", elapsed)
			}
			if prov != domain.WorkspaceProvenanceFresh || cwd != freshDir || built != 1 {
				t.Fatalf("workspace = (%q, %q, built %d), want the fresh tree %q once", cwd, prov, built, freshDir)
			}
			if got := f.storedWorktreePath(t); got != freshDir {
				t.Errorf("worktree_path = %q, want the fresh tree %q re-stamped under Y's claim", got, freshDir)
			}
			ensure := spansNamed(spans(), "engagement.workspace.ensure")
			if len(ensure) != 1 {
				t.Fatalf("engagement.workspace.ensure spans = %d, want 1", len(ensure))
			}
			if got := spanAttr(t, ensure[0], "workspace.provenance").AsString(); got != string(domain.WorkspaceProvenanceFresh) {
				t.Errorf("span provenance = %q, want fresh", got)
			}
			spanAttr(t, ensure[0], "snapshot.waited_ms")
			if !f.y.executorChangedSince(context.Background(), f.orgID, f.conversationID, next.ClaimID, prov) {
				t.Error("Y does not report the executor changed")
			}

			// The dead writer's upload never lands.
			f.gate.abort()
			<-first.done
			f.assertState(t, domain.WorkspaceSnapshotFailed, first.conv.ClaimID)
			if f.blobPresent(t) {
				t.Error("a blob exists for a persist that failed")
			}
			f.parkIdle(t, f.y, next, freshDir)
			f.assertState(t, domain.WorkspaceSnapshotWritten, next.ClaimID)
			f.assertInvariants(t)
		})
	}
}

// TestFleet_Eviction_RoundTripsTheUncommittedDelta: a tree parked past the
// eviction window is reclaimed once its snapshot is verifiably written, and
// the resume that follows rebuilds it from the blob with the uncommitted work
// intact. The two refusals that matter most are spot-checked on the way: a
// persist in flight, and a sibling engagement under the same key.
func TestFleet_Eviction_RoundTripsTheUncommittedDelta(t *testing.T) {
	f := seedParkFleet(t)
	first := f.claim(t, f.x)
	if first == nil {
		t.Fatal("X could not claim")
	}
	f.parkIdle(t, f.x, first, f.wtPath)
	f.assertState(t, domain.WorkspaceSnapshotWritten, first.ClaimID)
	pgtest.MustExec(t, f.h.AdminDB, `UPDATE conversations SET parked_at = now() - interval '7 hours' WHERE id = $1`, f.conversationID)

	const after = 6 * time.Hour
	treePresent := func() bool { _, err := os.Stat(f.wtPath); return err == nil }

	// A persist in flight re-owned the key: the tree is the only copy.
	successor := uuid.New().String()
	if err := f.stores.WorkspaceSnapshots.BeginSnapshotSystem(context.Background(), f.orgID, f.keyID, successor); err != nil {
		t.Fatalf("begin successor persist: %v", err)
	}
	f.x.EvictIdleWorkspaces(context.Background(), after)
	if !treePresent() {
		t.Fatal("evicted a tree whose snapshot state is pending")
	}
	if _, err := f.stores.WorkspaceSnapshots.FinishSnapshotSystem(context.Background(), f.orgID, f.keyID, successor, true); err != nil {
		t.Fatalf("finish successor persist: %v", err)
	}

	// A sibling step of the same blueprint is claimed on Y: someone is in
	// this directory.
	sibling := uuid.New().String()
	step1 := 1
	if _, err := f.stores.ConversationQueue.EnqueueConversation(context.Background(), f.orgID, domain.Conversation{
		ID: sibling, TaskID: f.taskID, PromptID: f.promptID, Model: "m",
		TriggerType: "manual", CreatorUserID: f.userID, BlueprintRunID: f.brID, BlueprintStepIndex: &step1,
	}); err != nil {
		t.Fatalf("enqueue sibling step: %v", err)
	}
	yID, yEpoch := f.y.executorIdentity()
	siblingClaim := uuid.New().String()
	pgtest.MustExec(t, f.h.AdminDB, `
		INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch)
		VALUES ($1, $2, $3, $4, $5)
	`, siblingClaim, f.orgID, sibling, yID, yEpoch)
	f.x.EvictIdleWorkspaces(context.Background(), after)
	if !treePresent() {
		t.Fatal("evicted a tree a sibling engagement is working in")
	}
	pgtest.MustExec(t, f.h.AdminDB, `UPDATE claims SET released_at = now(), outcome = 'parked' WHERE id = $1`, siblingClaim)
	pgtest.MustExec(t, f.h.AdminDB, `UPDATE conversations SET status = 'completed', completed_at = now() - interval '7 hours' WHERE id = $1`, sibling)

	// Nothing in the way: the tree goes, the blob stays.
	f.x.EvictIdleWorkspaces(context.Background(), after)
	if treePresent() {
		t.Fatal("the idle parked tree was not evicted")
	}
	if !f.blobPresent(t) {
		t.Fatal("the blob is gone along with the tree; there is nothing to resume from")
	}
	if got := f.storedWorktreePath(t); got != f.wtPath {
		t.Errorf("worktree_path after eviction = %q, want the recorded path %q left in place", got, f.wtPath)
	}

	if err := f.followUp("pick it back up"); err != nil {
		t.Fatalf("follow-up after eviction: %v", err)
	}
	next := f.claim(t, f.x)
	if next == nil {
		t.Fatal("X could not claim the resume")
	}
	f.deliver(t, next)
	cwd, prov, err := f.ensureOn(t, f.x, next, failingFreshBuilder(t))
	if err != nil {
		t.Fatalf("ensureWorkspace after eviction: %v", err)
	}
	if prov != domain.WorkspaceProvenanceRehydrated || cwd != f.wtPath {
		t.Fatalf("workspace = (%q, %q), want %q rehydrated", cwd, prov, f.wtPath)
	}
	assertFileContains(t, filepath.Join(cwd, "README.md"), "uncommitted edit")
	assertFileContains(t, filepath.Join(cwd, "_tfac", "notes.md"), "half-finished work")

	f.parkIdle(t, f.x, next, cwd)
	f.assertInvariants(t)
}

// TestFleet_Affinity_ResumeChasesTheLastExecutor pins the tier behavior a
// resume relies on, driven through the follow-up rather than the store: X
// claims its own resumed conversation at once, Y cannot inside the aging
// window, and Y can the moment X's heartbeat is stale past liveness. The
// conversation is minutes old throughout: each wake opens a fresh window.
func TestFleet_Affinity_ResumeChasesTheLastExecutor(t *testing.T) {
	f := seedParkFleet(t)
	xID, _ := f.x.executorIdentity()
	yID, _ := f.y.executorIdentity()

	first := f.claim(t, f.x)
	if first == nil {
		t.Fatal("X could not claim")
	}
	f.parkIdle(t, f.x, first, f.wtPath)
	if err := f.followUp("again"); err != nil {
		t.Fatalf("follow-up: %v", err)
	}
	if got := f.preferredExecutor(t); got != xID {
		t.Fatalf("preferred executor after the resume = %q, want X (%s)", got, xID)
	}
	if got := f.claim(t, f.y); got != nil {
		t.Fatal("Y claimed inside the aging window while X is live")
	}
	next := f.claim(t, f.x)
	if next == nil {
		t.Fatal("X could not claim its own resumed conversation at once")
	}
	f.deliver(t, next)
	f.parkIdle(t, f.x, next, f.wtPath)

	// Same stamp, but X has gone quiet: the warm tree is on a machine nobody
	// can reach, so the stamp yields immediately.
	if err := f.followUp("and again"); err != nil {
		t.Fatalf("follow-up: %v", err)
	}
	backdateFleetHeartbeat(t, f.h, xID, 30*time.Second)
	onY := f.claim(t, f.y)
	if onY == nil {
		t.Fatal("Y could not claim a stale executor's resumed conversation")
	}
	f.deliver(t, onY)
	f.parkIdle(t, f.y, onY, f.wtPath)

	// The stamp follows the engagement: Y drove it last, so the next resume
	// is Y's.
	if err := f.followUp("once more"); err != nil {
		t.Fatalf("follow-up: %v", err)
	}
	if got := f.preferredExecutor(t); got != yID {
		t.Errorf("preferred executor after Y's engagement = %q, want Y (%s)", got, yID)
	}
	backdateFleetHeartbeat(t, f.h, xID, 0)
	if got := f.claim(t, f.x); got != nil {
		t.Fatal("X claimed a conversation Y drove last, inside the aging window")
	}
	// Past the window, anyone may take it. The window is the wake's stamp, so
	// that is what ages — the mint stamp has been minutes old all along.
	pgtest.MustExec(t, f.h.AdminDB, `UPDATE conversations SET queued_at = now() - interval '21 seconds' WHERE id = $1`, f.conversationID)
	aged := f.claim(t, f.x)
	if aged == nil {
		t.Fatal("X could not claim once the aging window elapsed")
	}
	f.deliver(t, aged)
	f.parkIdle(t, f.x, aged, f.wtPath)
	f.assertInvariants(t)
}
