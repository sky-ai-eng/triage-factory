package memoryprovision

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
)

const testModel = "claude-haiku-4-5-20251001"

// fakeCompleter stands in for the recorder's completion. It writes the ledger
// row the real one would (from the caller-minted RunID), so the attempt's link
// to its spend is exercised rather than assumed.
type fakeCompleter struct {
	mu    sync.Mutex
	calls int
	opts  systemllm.CompleteOptions

	text  string
	err   error
	block chan struct{} // when non-nil, Complete waits on it (or on ctx) first
	runs  db.SystemLLMRunStore
}

func (f *fakeCompleter) Complete(ctx context.Context, opts systemllm.CompleteOptions) (*systemllm.CompleteResult, error) {
	f.mu.Lock()
	f.calls++
	f.opts = opts
	block, runs, text, err := f.block, f.runs, f.text, f.err
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if runs != nil && opts.RunID != "" {
		if rerr := runs.Record(ctx, domain.SystemLLMRun{
			ID: opts.RunID, OrgID: opts.OrgID, Job: opts.Job, Model: opts.Model,
			StartedAt: time.Now().UTC(), CompletedAt: time.Now().UTC(),
		}); rerr != nil {
			return nil, rerr
		}
	}
	if err != nil {
		return nil, err
	}
	return &systemllm.CompleteResult{Text: text}, nil
}

func (f *fakeCompleter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fixture is one ended conversation on one task, plus the Manager that owes it
// a memory.
type fixture struct {
	t              *testing.T
	database       *sql.DB
	stores         db.Stores
	mgr            *Manager
	fake           *fakeCompleter
	orgID          string
	conversationID string
	taskID         string
	entityID       string
	notified       []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	database, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = database.Close() })
	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}

	stores := sqlitestore.New(database)
	orgID := runmode.LocalDefaultOrgID
	ctx := t.Context()

	entity, _, err := stores.Entities.FindOrCreate(ctx, orgID, "github", "owner/repo#7", "pr", "Fix the build", "https://example.com/7")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	eventID, err := stores.Events.Record(ctx, orgID, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed,
		EntityID:  &entity.ID,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := stores.Tasks.FindOrCreate(ctx, orgID, runmode.LocalDefaultTeamID, entity.ID, domain.EventGitHubPRCICheckFailed, "", eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	conversationID := "conv-owing"
	dbtest.SeedConversation(t, database, domain.Conversation{
		ID:     conversationID,
		TaskID: task.ID,
		Model:  testModel,
	})

	f := &fixture{
		t: t, database: database, stores: stores,
		orgID: orgID, conversationID: conversationID, taskID: task.ID, entityID: entity.ID,
	}
	f.fake = &fakeCompleter{text: "The branch aa/fix-build was pushed and a pull request opened.", runs: stores.SystemLLMRuns}
	f.mgr = NewManager(stores, nil, func(context.Context, string) (string, error) { return testModel, nil }, nil, nil,
		func(orgID, taskID, taskStatus string) {
			f.notified = append(f.notified, taskID+":"+taskStatus)
		})
	f.mgr.complete = f.fake
	return f
}

// end stamps the boundary that makes the memory owed.
func (f *fixture) end() {
	f.t.Helper()
	if _, err := f.stores.Conversations.EndConversationSystem(f.t.Context(), f.orgID, f.conversationID, domain.EndedRequeued); err != nil {
		f.t.Fatalf("end conversation: %v", err)
	}
}

func (f *fixture) message(role, subtype, content string) {
	f.t.Helper()
	if _, err := f.stores.Conversations.InsertMessageSystem(f.t.Context(), f.orgID, &domain.Message{
		ConversationID: f.conversationID, Role: role, Subtype: subtype, Content: content,
	}); err != nil {
		f.t.Fatalf("insert %s message: %v", role, err)
	}
}

func (f *fixture) memory() *domain.TaskMemory {
	f.t.Helper()
	mem, err := f.stores.TaskMemory.GetForConversationSystem(f.t.Context(), f.orgID, f.conversationID)
	if err != nil {
		f.t.Fatalf("read memory: %v", err)
	}
	return mem
}

func (f *fixture) newestAttempt() *domain.MemoryAttempt {
	f.t.Helper()
	a, err := f.stores.MemoryAttempts.NewestAttemptForConversationSystem(f.t.Context(), f.orgID, f.conversationID)
	if err != nil {
		f.t.Fatalf("read newest attempt: %v", err)
	}
	return a
}

func (f *fixture) attemptCount() int {
	f.t.Helper()
	var n int
	if err := f.database.QueryRow(`SELECT COUNT(*) FROM conversation_memory_attempts WHERE conversation_id = ?`, f.conversationID).Scan(&n); err != nil {
		f.t.Fatalf("count attempts: %v", err)
	}
	return n
}

func (f *fixture) memoryEntityRoles() map[string]string {
	f.t.Helper()
	rows, err := f.database.Query(`SELECT entity_id, role FROM conversation_memory_entities WHERE conversation_id = ?`, f.conversationID)
	if err != nil {
		f.t.Fatalf("read memory entities: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var entityID, role string
		if err := rows.Scan(&entityID, &role); err != nil {
			f.t.Fatalf("scan memory entity: %v", err)
		}
		out[entityID] = role
	}
	return out
}

func (f *fixture) taskPending() bool {
	f.t.Helper()
	task, err := f.stores.Tasks.GetSystem(f.t.Context(), f.orgID, f.taskID)
	if err != nil || task == nil {
		f.t.Fatalf("read task: %v", err)
	}
	return task.MemoryPending
}

// TestFulfil_GeneratesFromTheTranscript is the whole happy path: the memory
// lands marked as a reconstruction, the join row makes it reachable from the
// task's entity, the attempt names the spend it bought, and the task stops
// waiting.
func TestFulfil_GeneratesFromTheTranscript(t *testing.T) {
	f := newFixture(t)
	f.message(roleUser, "", "the opening turn")
	f.message(roleAssistant, "", "pushed aa/fix-build")
	f.end()

	if !f.taskPending() {
		t.Fatal("the fixture must start with the task waiting on its memory")
	}
	if err := f.mgr.Fulfil(t.Context(), f.orgID, f.conversationID); err != nil {
		t.Fatalf("Fulfil: %v", err)
	}

	mem := f.memory()
	if mem == nil {
		t.Fatal("want a memory row")
	}
	if mem.Source != domain.MemorySourceGenerated {
		t.Errorf("source = %q, want %q", mem.Source, domain.MemorySourceGenerated)
	}
	firstLine, _, _ := strings.Cut(mem.Content, "\n")
	// Literal rather than generatedHeader(2, 2): the wording is the reader's
	// cue that what follows is a reconstruction, so a test that recomputed it
	// would agree with any rewording at all.
	want := "_Generated by Triage Factory from the conversation transcript (2 of 2 rows); the agent did not write its own memory._"
	if firstLine != want {
		t.Errorf("first line = %q, want the generated marker %q", firstLine, want)
	}
	if !strings.Contains(mem.Content, "aa/fix-build") {
		t.Errorf("the model's text must follow the marker; got %q", mem.Content)
	}
	if got := f.memoryEntityRoles()[f.entityID]; got != domain.MemoryRolePrimary {
		t.Errorf("the task's entity is attached as %q, want %q", got, domain.MemoryRolePrimary)
	}

	attempt := f.newestAttempt()
	if attempt == nil || attempt.Outcome != domain.MemoryAttemptGenerated {
		t.Fatalf("attempt = %+v, want one with outcome %q", attempt, domain.MemoryAttemptGenerated)
	}
	if attempt.CompletedAt == nil {
		t.Error("a settled attempt carries a completed_at")
	}
	if attempt.SystemLLMRunID != f.fake.opts.RunID || attempt.SystemLLMRunID == "" {
		t.Errorf("system_llm_run_id = %q, want the id the call was minted with (%q)", attempt.SystemLLMRunID, f.fake.opts.RunID)
	}
	if attempt.WindowRowsTotal != 2 || attempt.WindowRowsSent != 2 {
		t.Errorf("window counts = %d/%d, want 2/2", attempt.WindowRowsSent, attempt.WindowRowsTotal)
	}
	if f.fake.opts.Job != systemllm.JobMemory {
		t.Errorf("job = %q, want %q", f.fake.opts.Job, systemllm.JobMemory)
	}
	if !strings.Contains(f.fake.opts.UserMessage, "pushed aa/fix-build") || !strings.Contains(f.fake.opts.Message, "pushed aa/fix-build") {
		t.Error("both prompt shapes must carry the windowed transcript")
	}
	if f.taskPending() {
		t.Error("the task must stop waiting once its memory lands")
	}
	if len(f.notified) != 1 || !strings.HasPrefix(f.notified[0], f.taskID+":") {
		t.Errorf("notifications = %v, want one for the task", f.notified)
	}
}

// TestFulfil_EmptyRowWhenNothingWasEverSaid: a conversation that never reached
// an assistant turn has nothing a memory could hold, so it is settled
// mechanically — no model call, no wait.
func TestFulfil_EmptyRowWhenNothingWasEverSaid(t *testing.T) {
	f := newFixture(t)
	f.message(roleUser, "", "do the thing")
	f.end()

	if err := f.mgr.Fulfil(t.Context(), f.orgID, f.conversationID); err != nil {
		t.Fatalf("Fulfil: %v", err)
	}

	if f.fake.callCount() != 0 {
		t.Errorf("model was called %d times, want 0", f.fake.callCount())
	}
	mem := f.memory()
	if mem == nil || mem.Source != domain.MemorySourceNone || mem.Content != "" {
		t.Fatalf("memory = %+v, want an empty row with source %q", mem, domain.MemorySourceNone)
	}
	if got := f.memoryEntityRoles()[f.entityID]; got != domain.MemoryRolePrimary {
		t.Errorf("an empty row is still attached to its entity; role = %q", got)
	}
	attempt := f.newestAttempt()
	if attempt == nil || attempt.Outcome != domain.MemoryAttemptEmpty {
		t.Fatalf("attempt = %+v, want outcome %q", attempt, domain.MemoryAttemptEmpty)
	}
	if attempt.WindowRowsTotal != 1 || attempt.WindowRowsSent != 0 {
		t.Errorf("window counts = %d/%d, want 0 sent of 1", attempt.WindowRowsSent, attempt.WindowRowsTotal)
	}
	if f.taskPending() {
		t.Error("an empty row settles the debt like any other")
	}
}

// TestFulfil_SkipsWhatIsNotOwed covers both ways a conversation owes nothing:
// it has not ended, and it already has a memory. Neither writes an attempt row
// — the ledger explains waits, and there is no wait here.
func TestFulfil_SkipsWhatIsNotOwed(t *testing.T) {
	f := newFixture(t)
	f.message(roleAssistant, "", "worked on it")

	if err := f.mgr.Fulfil(t.Context(), f.orgID, f.conversationID); err != nil {
		t.Fatalf("Fulfil on a live conversation: %v", err)
	}
	if f.memory() != nil || f.attemptCount() != 0 {
		t.Fatal("a conversation that has not ended owes nothing")
	}

	f.end()
	if err := f.mgr.Fulfil(t.Context(), f.orgID, f.conversationID); err != nil {
		t.Fatalf("Fulfil: %v", err)
	}
	if f.attemptCount() != 1 {
		t.Fatalf("attempts = %d, want the one that settled it", f.attemptCount())
	}

	// Second pass over a settled conversation: the sweep and the doorbell both
	// reach here, and neither may spend again.
	if err := f.mgr.Fulfil(t.Context(), f.orgID, f.conversationID); err != nil {
		t.Fatalf("second Fulfil: %v", err)
	}
	if f.attemptCount() != 1 {
		t.Errorf("attempts = %d, want no second one on a settled conversation", f.attemptCount())
	}
	if f.fake.callCount() != 1 {
		t.Errorf("model called %d times, want once", f.fake.callCount())
	}
}

// TestFulfil_ClassifiesEveryFailure pins the error_kind a reader sees for each
// way generation can stop, that no memory row is written for any of them, and
// that none of them is an error the caller has to do anything about — the
// sweep is the retry.
func TestFulfil_ClassifiesEveryFailure(t *testing.T) {
	noModel := fmt.Errorf("%w: the background jobs model setting is empty — pick one in Settings", systemllm.ErrNoModel)
	for _, tc := range []struct {
		name      string
		resolve   func(context.Context, string) (string, error)
		callErr   error
		text      string
		wantKind  domain.MemoryAttemptErrorKind
		wantCalls int
	}{
		{
			name:     "no model configured",
			resolve:  func(context.Context, string) (string, error) { return "", noModel },
			wantKind: domain.MemoryAttemptErrNoModel,
		},
		{
			name:      "provider in backoff",
			callErr:   &systemllm.ErrProviderBackoff{Provider: "anthropic", ResumeAt: time.Now().Add(time.Minute)},
			wantKind:  domain.MemoryAttemptErrProviderBackoff,
			wantCalls: 1,
		},
		{
			name:      "provider error",
			callErr:   errors.New("500 from upstream: {\"error\":\"secret internals\"}"),
			wantKind:  domain.MemoryAttemptErrProviderError,
			wantCalls: 1,
		},
		{
			name:      "an answer with nothing in it",
			text:      "   \n",
			wantKind:  domain.MemoryAttemptErrProviderError,
			wantCalls: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.message(roleAssistant, "", "worked on it")
			f.end()
			if tc.resolve != nil {
				f.mgr.models = tc.resolve
			}
			f.fake.err, f.fake.text = tc.callErr, tc.text

			if err := f.mgr.Fulfil(t.Context(), f.orgID, f.conversationID); err != nil {
				t.Fatalf("a classified failure is not the caller's error: %v", err)
			}

			if f.fake.callCount() != tc.wantCalls {
				t.Errorf("model called %d times, want %d", f.fake.callCount(), tc.wantCalls)
			}
			if f.memory() != nil {
				t.Error("a failed attempt writes no memory row")
			}
			attempt := f.newestAttempt()
			if attempt == nil || attempt.Outcome != domain.MemoryAttemptFailed || attempt.ErrorKind != tc.wantKind {
				t.Fatalf("attempt = %+v, want failed/%s", attempt, tc.wantKind)
			}
			if strings.Contains(attempt.ErrorMessage, "secret internals") {
				t.Errorf("the row must carry TF's wording, never the upstream body; got %q", attempt.ErrorMessage)
			}
			if attempt.ErrorMessage == "" {
				t.Error("a failed attempt says why")
			}
			if !f.taskPending() {
				t.Error("a failed attempt leaves the task waiting")
			}
		})
	}
}

// TestFulfil_TimesOutTheModelCall: the generation carries its own bound, and
// the row a person reads names it.
func TestFulfil_TimesOutTheModelCall(t *testing.T) {
	f := newFixture(t)
	f.message(roleAssistant, "", "worked on it")
	f.end()
	f.mgr.attemptTimeout = 20 * time.Millisecond
	f.fake.block = make(chan struct{}) // never released

	if err := f.mgr.Fulfil(t.Context(), f.orgID, f.conversationID); err != nil {
		t.Fatalf("a timeout is not the caller's error: %v", err)
	}

	attempt := f.newestAttempt()
	if attempt == nil || attempt.ErrorKind != domain.MemoryAttemptErrTimeout {
		t.Fatalf("attempt = %+v, want failed/%s", attempt, domain.MemoryAttemptErrTimeout)
	}
	if f.memory() != nil {
		t.Error("a timed-out attempt writes no memory row")
	}
}

// TestFulfil_CancelledBrainLeavesTheAttemptOpen: stopBrain cancels mid-attempt,
// and the row keeps completed_at NULL — abandonment is derived from started_at,
// never stamped, and a verdict here would claim an answer nobody reached.
func TestFulfil_CancelledBrainLeavesTheAttemptOpen(t *testing.T) {
	f := newFixture(t)
	f.message(roleAssistant, "", "worked on it")
	f.end()
	f.fake.block = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.mgr.Fulfil(ctx, f.orgID, f.conversationID) }()

	waitFor(t, func() bool { return f.fake.callCount() == 1 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Fulfil: %v", err)
	}

	attempt := f.newestAttempt()
	if attempt == nil {
		t.Fatal("the attempt row must still be there")
	}
	if attempt.CompletedAt != nil || attempt.Outcome != "" {
		t.Errorf("attempt = %+v, want it left open", attempt)
	}
	if f.memory() != nil {
		t.Error("an abandoned attempt writes no memory row")
	}
}

// TestFulfil_OneAttemptPerConversationAtATime: the doorbell and the sweep can
// both name one conversation at the same moment, and only one of them may
// spend on it.
func TestFulfil_OneAttemptPerConversationAtATime(t *testing.T) {
	f := newFixture(t)
	f.message(roleAssistant, "", "worked on it")
	f.end()
	release := make(chan struct{})
	f.fake.block = release

	first := make(chan error, 1)
	go func() { first <- f.mgr.Fulfil(t.Context(), f.orgID, f.conversationID) }()
	waitFor(t, func() bool { return f.fake.callCount() == 1 })

	if err := f.mgr.Fulfil(t.Context(), f.orgID, f.conversationID); err != nil {
		t.Fatalf("the losing caller returns quietly: %v", err)
	}
	if f.fake.callCount() != 1 {
		t.Errorf("model called %d times while one attempt was in flight, want once", f.fake.callCount())
	}
	if f.attemptCount() != 1 {
		t.Errorf("attempts = %d while one was in flight, want 1", f.attemptCount())
	}

	close(release)
	if err := <-first; err != nil {
		t.Fatalf("Fulfil: %v", err)
	}
}

// TestSweep_HonoursTheBackoffAndTheDoorbellIgnoresIt: a failed attempt is not
// retried on the next tick five seconds later, and the org-wide doorbell — the
// re-kick a configuration save rings — does not wait for it to age out.
func TestSweep_HonoursTheBackoffAndTheDoorbellIgnoresIt(t *testing.T) {
	f := newFixture(t)
	f.message(roleAssistant, "", "worked on it")
	f.end()
	f.mgr.models = func(context.Context, string) (string, error) {
		return "", fmt.Errorf("%w: nothing is picked", systemllm.ErrNoModel)
	}

	f.mgr.sweep(t.Context(), AttemptBackoff, "")
	if f.attemptCount() != 1 {
		t.Fatalf("attempts = %d, want the sweep's first", f.attemptCount())
	}

	f.mgr.sweep(t.Context(), AttemptBackoff, "")
	if f.attemptCount() != 1 {
		t.Errorf("attempts = %d, want the backoff to have held the second", f.attemptCount())
	}

	// The save of a background-jobs model is the re-kick, so the doorbell's
	// org-wide form must not wait out a backoff the old configuration earned.
	f.mgr.models = func(context.Context, string) (string, error) { return testModel, nil }
	f.mgr.sweep(t.Context(), 0, f.orgID)
	if f.attemptCount() != 2 {
		t.Fatalf("attempts = %d, want the doorbell's", f.attemptCount())
	}
	if f.memory() == nil {
		t.Error("the re-kicked attempt settles the debt")
	}

	// Another org's save is not a reason to sweep this one.
	f.mgr.sweep(t.Context(), 0, "org-somebody-else")
	if f.attemptCount() != 2 {
		t.Errorf("attempts = %d, want the other org's doorbell to have been ignored", f.attemptCount())
	}
}

// TestNudge_SettlesOneConversation covers the doorbell's per-conversation form
// end to end, goroutine included.
func TestNudge_SettlesOneConversation(t *testing.T) {
	f := newFixture(t)
	f.message(roleAssistant, "", "worked on it")
	f.end()

	f.mgr.Nudge(f.orgID, f.conversationID)
	waitFor(t, func() bool { return f.memory() != nil })

	if f.memory().Source != domain.MemorySourceGenerated {
		t.Errorf("source = %q, want %q", f.memory().Source, domain.MemorySourceGenerated)
	}
}

// TestRunSweep_StopsWithTheBrain: stopBrain cancels brainCtx, and the sweep
// must stop rather than tick on against a pod that no longer holds the lease.
func TestRunSweep_StopsWithTheBrain(t *testing.T) {
	RunSweep(t.Context(), nil, time.Millisecond) // nil manager is a no-op, not a panic

	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { RunSweep(ctx, f.mgr, time.Millisecond); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunSweep did not return after its context was cancelled")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not reached in time")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestDroppedDoorbell_LeavesTheConversationForTheNextSweep pins the contract
// the relay rests on: the doorbell is latency and nothing else. It writes
// nothing, marks nothing, and claims nothing — so a message the tf_ctl relay
// dropped (a holderless gap mid-failover, a LISTEN reconnect, a publish that
// errored) costs one sweep interval and never a memory.
//
// Proven by ringing nothing at all, which is exactly what a pod sees when a
// notification never arrives, and then letting the sweep run.
func TestDroppedDoorbell_LeavesTheConversationForTheNextSweep(t *testing.T) {
	f := newFixture(t)
	f.message(roleAssistant, "", "worked on it")
	f.end()

	owed, err := f.stores.Conversations.ListMemoryOwedSystem(t.Context(), "", AttemptBackoff, 100)
	if err != nil {
		t.Fatalf("list owed: %v", err)
	}
	if len(owed) != 1 || owed[0].ConversationID != f.conversationID {
		t.Fatalf("owed = %v, want the ended conversation with no memory row", owed)
	}

	// The sweep settles what the dropped doorbell would have.
	f.mgr.sweep(t.Context(), AttemptBackoff, "")
	if f.memory() == nil {
		t.Fatal("the backstop sweep did not settle the debt a dropped doorbell left")
	}

	// And once settled it leaves the list, so the next tick does not re-spend
	// on a conversation the doorbell's twin already paid for.
	owed, err = f.stores.Conversations.ListMemoryOwedSystem(t.Context(), "", AttemptBackoff, 100)
	if err != nil {
		t.Fatalf("list owed after the sweep: %v", err)
	}
	if len(owed) != 0 {
		t.Errorf("owed = %v after the memory landed, want none", owed)
	}
}

// TestNudge_RefusesAnEmptyOrg: an empty org means EVERY org to sweep, so a
// doorbell carrying one would amplify a single notification — a malformed relay
// message, a publisher bug — into a fleet-wide pass with the backoff disabled.
// Both forms are refused, because both reach a scan: the org-wide form lands in
// sweep directly, and the targeted form reads a conversation under no tenant.
//
// Asserted against a conversation that IS owed, so a guard that silently let
// the call through would settle it and fail this.
func TestNudge_RefusesAnEmptyOrg(t *testing.T) {
	f := newFixture(t)
	f.message(roleAssistant, "", "worked on it")
	f.end()

	f.mgr.Nudge("", "")               // would have swept every tenant, backoff ignored
	f.mgr.Nudge("", f.conversationID) // would have read a conversation under no org
	time.Sleep(50 * time.Millisecond) // the goroutine the guard must never start

	if f.attemptCount() != 0 {
		t.Errorf("attempts = %d, want 0 — an org-less doorbell must not scan", f.attemptCount())
	}
	if f.memory() != nil {
		t.Error("an org-less doorbell settled a memory")
	}

	// The same conversation with its real org still settles, so the guard
	// refuses the empty value rather than the doorbell.
	f.mgr.Nudge(f.orgID, f.conversationID)
	waitFor(t, func() bool { return f.memory() != nil })
}
