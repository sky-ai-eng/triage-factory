package delegate

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// stepFixture is one blueprint_run spanning several conversation rows on a
// single task — the shape every assertion below is about.
type stepFixture struct {
	s        *Spawner
	database *sql.DB
	task     domain.Task
	brID     string
	runIDs   []string
}

// seedStepFixture stands up a `steps`-long blueprint on a task of the given
// source, already past its first claim: the shared workspace is stamped on the
// blueprint_run, and step 0's own row carries it too (its source setup wrote it
// there). Every later row starts with no path at all, which is exactly the
// state a claim of it has to fix.
//
// The blueprint_run id is "bpr-"+suffix, so a caller that needs the workspace
// path the cold rehydrate will rebuild at can derive it before seeding.
func seedStepFixture(t *testing.T, source, suffix string, steps int, sharedWT string) stepFixture {
	t.Helper()
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	database := newDelegateTestDB(t)
	stores := sqlitestore.New(database)

	var sourceID, entityKind, eventType string
	switch source {
	case "github":
		sourceID, entityKind, eventType = "owner/repo#7", "pr", domain.EventGitHubPRCICheckFailed
	case "jira":
		sourceID, entityKind, eventType = "SKY-7", "issue", domain.EventJiraIssueAssigned
	case "slack":
		sourceID, entityKind, eventType = "C1/1700000000.000100", "message", domain.EventSlackMessage
	default:
		t.Fatalf("unknown task source %q", source)
	}

	entity, _, err := stores.Entities.FindOrCreate(ctx, org, source, sourceID, entityKind, "T", "https://example.com/"+suffix)
	if err != nil {
		t.Fatalf("create %s entity: %v", source, err)
	}
	eventID, err := stores.Events.Record(ctx, org, domain.Event{
		EventType: eventType, EntityID: &entity.ID, MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	task, _, err := testTaskStore(database).FindOrCreate(ctx, org, runmode.LocalDefaultTeamID, entity.ID, eventType, suffix, eventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	bpID := "bp-" + suffix
	if err := stores.Blueprints.Create(ctx, org, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: bpID, Name: bpID, Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("create blueprint: %v", err)
	}
	promptIDs := make([]string, steps)
	plan := make([]domain.BlueprintPlanStep, steps)
	for i := range steps {
		pid := fmt.Sprintf("%s-p%d", suffix, i)
		ensureTestPrompt(t, database, domain.Prompt{ID: pid, Name: pid, Body: "b", Source: "user"})
		promptIDs[i] = pid
		plan[i] = domain.BlueprintPlanStep{StepIndex: i, PromptID: pid, PromptName: pid, PromptBody: "b", Source: "user"}
	}
	if err := stores.Blueprints.ReplaceSteps(ctx, org, bpID, promptIDs, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "bpr-" + suffix, BlueprintID: bpID, TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		StepPlan: plan,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := database.Exec(`UPDATE blueprint_runs SET worktree_path = ? WHERE id = ?`, sharedWT, brID); err != nil {
		t.Fatalf("stamp the blueprint's shared worktree: %v", err)
	}

	runIDs := make([]string, steps)
	for i := range steps {
		idx := i
		runIDs[i] = fmt.Sprintf("%s-step%d", suffix, i)
		stepWT := ""
		if i == 0 {
			stepWT = sharedWT // the source setup stamped it on the first claim
		}
		dbtest.SeedConversation(t, database, domain.Conversation{
			ID: runIDs[i], TaskID: task.ID, PromptID: promptIDs[i], Status: "running",
			Model: "claude-sonnet-4-6", SessionID: "sess-" + runIDs[i], WorktreePath: stepWT,
			BlueprintRunID: brID, BlueprintStepIndex: &idx,
		})
	}

	return stepFixture{
		s:        NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6"),
		database: database,
		task:     *task,
		brID:     brID,
		runIDs:   runIDs,
	}
}

// blueprintRun re-reads the fixture's blueprint_run, which is what the
// dispatcher hands buildStepConfig.
func (f stepFixture) blueprintRun(t *testing.T) *domain.BlueprintRun {
	t.Helper()
	br, err := f.s.blueprints.GetRunSystem(context.Background(), runmode.LocalDefaultOrgID, f.brID)
	if err != nil || br == nil {
		t.Fatalf("GetRunSystem: (%v, %v)", br, err)
	}
	return br
}

// claimStep runs one step's claim through buildStepConfig's later-step arm,
// exactly as dispatchClaimedRun does, and returns the config it resolved.
func (f stepFixture) claimStep(t *testing.T, br *domain.BlueprintRun, step int) runConfig {
	t.Helper()
	run := domain.Conversation{ID: f.runIDs[step], TaskID: f.task.ID, BlueprintRunID: f.brID}
	cfg, err := f.s.buildStepConfig(context.Background(), runmode.LocalDefaultOrgID, br, f.task, run, nil, nil)
	if err != nil {
		t.Fatalf("buildStepConfig for step %d: %v", step, err)
	}
	return cfg
}

func storedWorktreePath(t *testing.T, database *sql.DB, convID string) string {
	t.Helper()
	var path sql.NullString
	if err := database.QueryRow(`SELECT worktree_path FROM conversations WHERE id = ?`, convID).Scan(&path); err != nil {
		t.Fatalf("read worktree_path for %s: %v", convID, err)
	}
	return path.String
}

// TestBuildStepConfig_StampsWorktreePathOnEveryStepRow is the fix's core
// property: a blueprint leaves EVERY step's conversation row carrying the tree
// that step ran in, not just the first.
//
// The path is per-row state for a reason the blueprint_run's copy cannot
// serve — `claude --resume` keys its session storage by cwd, so a wake reads
// the cwd off the conversation it is waking. Only the first claim used to write
// it (inside the source setups); later steps carried an empty path forever, and
// the refusal ladder checks that field before it asks whether the blueprint
// would drive the step at all. So a finished blueprint's final step — the one
// the whole follow-up feature exists for — was refused for a missing field
// rather than offered a composer.
//
// All three sources, because the later-step arm resolves the path three
// different ways and the write sits after the switch that chooses between them.
func TestBuildStepConfig_StampsWorktreePathOnEveryStepRow(t *testing.T) {
	for _, source := range []string{"github", "jira", "slack"} {
		t.Run(source, func(t *testing.T) {
			paths.SetForTest(t, t.TempDir())
			wt := t.TempDir()
			f := seedStepFixture(t, source, source+"-stamp", 3, wt)
			br := f.blueprintRun(t)

			for step := 1; step < len(f.runIDs); step++ {
				if cfg := f.claimStep(t, br, step); cfg.wtPath != wt {
					t.Fatalf("step %d resolved wtPath = %q, want the shared %q", step, cfg.wtPath, wt)
				}
			}

			for step, runID := range f.runIDs {
				if got := storedWorktreePath(t, f.database, runID); got != wt {
					t.Errorf("step %d conversation worktree_path = %q, want %q — a follow-up to it would be refused for a missing field", step, got, wt)
				}
			}
		})
	}
}

// TestBuildStepConfig_StampsNativeStepRowsToo pins that the stamp is
// runtime-agnostic. A native conversation's resume state is its message rows,
// so the ladder never asks it for a path — but the write is on the shared claim
// path and must not grow a runtime branch it doesn't need. Writing the true
// path onto a native row is harmless; skipping it would make the column mean
// two different things depending on which engine ran.
func TestBuildStepConfig_StampsNativeStepRowsToo(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	wt := t.TempDir()
	f := seedStepFixture(t, "jira", "native-stamp", 2, wt)
	markNative(t, f.database, f.runIDs[1])

	f.claimStep(t, f.blueprintRun(t), 1)

	if got := storedWorktreePath(t, f.database, f.runIDs[1]); got != wt {
		t.Errorf("native step worktree_path = %q, want %q", got, wt)
	}
}

// TestBuildStepConfig_ReportsAWarmTreeAsWarm is the claim path's half of the
// resume notice's honesty: a later step whose shared worktree is still on disk
// resolves warm, so the loop it feeds says nothing about a restore that did not
// happen. Nothing downstream can re-derive this — past here the reused tree and
// a rebuilt one are the same directory.
func TestBuildStepConfig_ReportsAWarmTreeAsWarm(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	wt := t.TempDir()
	f := seedStepFixture(t, "jira", "warm-provenance", 2, wt)

	cfg := f.claimStep(t, f.blueprintRun(t), 1)
	if cfg.wtPath != wt {
		t.Fatalf("cwd = %q, want the intact shared worktree %q", cfg.wtPath, wt)
	}
	if cfg.workspace != domain.WorkspaceProvenanceWarm {
		t.Errorf("workspace provenance = %q, want warm", cfg.workspace)
	}
}

// TestBuildStepConfig_ColdRehydrateStampsTheTreeTheSessionRunsIn covers the
// case where the workspace is not on disk at all and ensureWorkspace rebuilds
// it from the durable snapshot. Whatever path that rebuild lands on is where
// the resumed session will run, so it is what the row has to say.
//
// Two shapes, because the rebuild path is derived from the blueprint key and
// may or may not equal the one the row already had:
//
//   - stable: the rebuild lands exactly where the stale path pointed (same
//     host, same key). ensureWorkspace has nothing to correct and writes
//     nothing, so the claim's own stamp is the only thing that fills the row.
//   - moved: the rebuild lands somewhere else (the tree came up on a host whose
//     run-root differs). The row must follow the tree, not the memory of it.
func TestBuildStepConfig_ColdRehydrateStampsTheTreeTheSessionRunsIn(t *testing.T) {
	cases := []struct {
		name       string
		staleMoved bool
	}{
		{name: "rebuild lands on the same path"},
		{name: "rebuild lands on a fresh path", staleMoved: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths.SetForTest(t, t.TempDir())
			setupGitTestEnv(t)
			ctx := context.Background()
			org := runmode.LocalDefaultOrgID

			suffix := "cold-same"
			if tc.staleMoved {
				suffix = "cold-moved"
			}
			// The rebuild target is derived from the blueprint_run id, which
			// seedStepFixture composes deterministically.
			rebuilt := worktree.RunRoot("bpr-" + suffix)
			t.Cleanup(func() { _ = os.RemoveAll(rebuilt) })

			stale := rebuilt
			if tc.staleMoved {
				stale = filepath.Join(t.TempDir(), "elsewhere")
			}
			f := seedStepFixture(t, "jira", suffix, 2, stale)

			blobs, err := storage.New()
			if err != nil {
				t.Fatalf("storage.New: %v", err)
			}
			f.s.SetStorage(blobs)

			// Capture a workspace under the blueprint's key, then lose it — the
			// host-loss shape a cold rehydrate exists for.
			src := filepath.Join(t.TempDir(), "workspace")
			writeFile(t, filepath.Join(src, "_tfac", "notes.txt"), "scratch survived")
			if err := f.s.snapshotWorkspace(ctx, org, f.brID, f.brID, src, ""); err != nil {
				t.Fatalf("snapshotWorkspace: %v", err)
			}
			if err := os.RemoveAll(src); err != nil {
				t.Fatalf("rm source workspace: %v", err)
			}
			_ = os.RemoveAll(stale)

			cfg := f.claimStep(t, f.blueprintRun(t), 1)
			if cfg.wtPath != rebuilt {
				t.Fatalf("rehydrated cwd = %q, want %q", cfg.wtPath, rebuilt)
			}
			if cfg.workspace != domain.WorkspaceProvenanceRehydrated {
				t.Errorf("workspace provenance = %q, want rehydrated — the loop's resume notice is written off this", cfg.workspace)
			}
			if got := storedWorktreePath(t, f.database, f.runIDs[1]); got != rebuilt {
				t.Errorf("conversation worktree_path = %q, want the rebuilt %q — the row must point at the tree the resumed session runs in", got, rebuilt)
			}
		})
	}
}

// TestFinishedThreeStepBlueprint_RefusesEarlierStepsHonestlyAndResumesTheLast
// is what the fix restores, per step, for the case a user actually met: a
// three-step blueprint that finished.
//
// Before it, every step but the first failed the ladder's worktree rung. That
// made step 2 lie ("this run didn't record the state a resume needs" — it had,
// nobody wrote it down) and made step 3 — the final step, the only one the
// follow-up feature is about — unreachable. Past the rungs, each step gets the
// answer its position earns.
func TestFinishedThreeStepBlueprint_RefusesEarlierStepsHonestlyAndResumesTheLast(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	wt := t.TempDir()
	f := seedStepFixture(t, "github", "finished", 3, wt)

	br := f.blueprintRun(t)
	for step := 1; step < len(f.runIDs); step++ {
		f.claimStep(t, br, step)
	}

	// The blueprint ran to the end: every step concluded, the plan finished,
	// and it came to rest on its last step.
	if _, err := f.database.Exec(`UPDATE conversations SET status='completed', outcome='finish'`); err != nil {
		t.Fatalf("conclude the steps: %v", err)
	}
	finishBlueprint(t, f.database, f.brID, "completed", len(f.runIDs)-1)

	for step, runID := range f.runIDs {
		run, err := f.s.agentRuns.GetSystem(ctx, org, runID)
		if err != nil || run == nil {
			t.Fatalf("load step %d: %v", step, err)
		}
		ok, reason := f.s.ResumabilityFor(ctx, org, run)
		if step == len(f.runIDs)-1 {
			if !ok {
				t.Fatalf("final step: ResumabilityFor = (false, %q), want resumable", reason)
			}
			continue
		}
		if ok || reason != ResumeBlockedBlueprintConcluded {
			t.Errorf("step %d: ResumabilityFor = (%v, %q), want (false, %q) — the blueprint moved past it, which is the honest reason",
				step, ok, reason, ResumeBlockedBlueprintConcluded)
		}
	}

	// And the capability itself: a follow-up to the final step is accepted,
	// claimed off the ordinary queue, and driven down the resume path. The
	// delivery fails fast on a session transcript this bare worktree never
	// held; reaching a terminal at all is the proof it was driven.
	final := f.runIDs[len(f.runIDs)-1]
	if err := f.s.SendMessage(ctx, org, final, runmode.LocalDefaultUserID, "one more thing"); err != nil {
		t.Fatalf("SendMessage to the final step of a finished blueprint: %v", err)
	}
	claimAndDispatch(t, f.s, f.database)
	if got := storedStatus(t, f.database, final); got == "" || got == "open" {
		t.Fatalf("final step stored status = %q, want a terminal — the claim never reached delivery", got)
	}
}

// recordingHandler captures emitted log records so a test can assert on what
// was logged rather than on a side effect the log line has no other trace of.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// warnedFields returns the "field" attribute of every warning the resume
// contract raised, in emission order.
func (h *recordingHandler) warnedFields(t *testing.T) []string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var fields []string
	for _, r := range h.records {
		if r.Level != slog.LevelWarn || r.Message != "SDK conversation is missing a resume coordinate; resume will refuse this row" {
			continue
		}
		var field, run string
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "field":
				field = a.Value.String()
			case "run":
				run = a.Value.String()
			}
			return true
		})
		if run == "" {
			t.Error("the warning must name the conversation; without it there is nothing to look up")
		}
		if field == "" {
			t.Error("the warning must name the missing field; without it there is nothing to grep for")
		}
		fields = append(fields, field)
	}
	return fields
}

func captureDelegateLog(t *testing.T) *recordingHandler {
	t.Helper()
	h := &recordingHandler{}
	prev := delegateLog
	delegateLog = slog.New(h)
	t.Cleanup(func() { delegateLog = prev })
	return h
}

// TestAssertResumeCoordinates_NamesEveryMissingField is the write-side check on
// the coordinates an SDK wake needs. The gap it exists for shipped silently and
// surfaced days later as a refusal in a user's composer; a claim that runs
// against an incomplete row must cost a grep of the logs instead.
//
// Pinned here rather than left as a comment because an assertion nobody reads
// is an assertion that can be deleted without anyone noticing.
func TestAssertResumeCoordinates_NamesEveryMissingField(t *testing.T) {
	cases := []struct {
		name   string
		mutate string
		at     resumeCheckpoint
		want   []string
	}{
		{
			name:   "complete row at claim time is silent",
			mutate: `UPDATE conversations SET worktree_path='/tmp/wt', model='m' WHERE id=?`,
			at:     resumeCheckClaim,
			want:   nil,
		},
		{
			// The bug this whole change is about, seen from the claim that
			// caused it rather than the follow-up that met it.
			name:   "missing worktree path",
			mutate: `UPDATE conversations SET worktree_path=NULL WHERE id=?`,
			at:     resumeCheckClaim,
			want:   []string{"worktree_path"},
		},
		{
			name:   "missing model",
			mutate: `UPDATE conversations SET model=NULL WHERE id=?`,
			at:     resumeCheckClaim,
			want:   []string{"model"},
		},
		{
			name:   "both, each named",
			mutate: `UPDATE conversations SET worktree_path=NULL, model=NULL WHERE id=?`,
			at:     resumeCheckClaim,
			want:   []string{"worktree_path", "model"},
		},
		{
			// The session id is legitimately absent until the stream's first
			// system/init, so it is only asked for once the row has come to
			// rest — where its absence really does end the conversation.
			name:   "no session id is not a finding at claim time",
			mutate: `UPDATE conversations SET sdk_session_id=NULL WHERE id=?`,
			at:     resumeCheckClaim,
			want:   nil,
		},
		{
			name:   "no session id on a row at rest is",
			mutate: `UPDATE conversations SET sdk_session_id=NULL WHERE id=?`,
			at:     resumeCheckRest,
			want:   []string{"sdk_session_id"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := newDelegateTestDB(t)
			const runID = "r-contract"
			seedRun(t, database, runID, "sess-contract", "/tmp/wt")
			if _, err := database.Exec(tc.mutate, runID); err != nil {
				t.Fatalf("stage the row: %v", err)
			}
			s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

			logs := captureDelegateLog(t)
			s.assertResumeCoordinates(context.Background(), runmode.LocalDefaultOrgID, runID, tc.at)

			got := logs.warnedFields(t)
			if len(got) != len(tc.want) {
				t.Fatalf("warned fields = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("warned field %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestAssertResumeCoordinates_UnreadableRowIsNotAFinding: a read that failed
// says nothing about what was written, so it must not raise the alarm the real
// gap raises — an assertion that cries wolf on transient DB trouble is one
// people learn to filter out.
func TestAssertResumeCoordinates_UnreadableRowIsNotAFinding(t *testing.T) {
	database := newDelegateTestDB(t)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	logs := captureDelegateLog(t)
	s.assertResumeCoordinates(context.Background(), runmode.LocalDefaultOrgID, "no-such-conversation", resumeCheckRest)

	if got := logs.warnedFields(t); len(got) != 0 {
		t.Errorf("warned fields = %v, want none for a row that could not be read", got)
	}
}
