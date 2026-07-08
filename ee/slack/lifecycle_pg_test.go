package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// ---------- fake Slack API (reactions.add, chat.postMessage, setStatus) ----------

type reactionCall struct{ Channel, TS, Emoji string }
type postCall struct{ Channel, ThreadTS, Text string }
type statusCall struct{ Channel, ThreadTS, Status string }

// fakeLifecycleSlack is a minimal fake covering the three endpoints the
// lifecycle adapter drives: reactions.add (the 👀 ack), chat.postMessage
// (the no-match reply + the failure note), and assistant.threads.setStatus
// (the live indicator). All access is mutex-guarded — per-run workers run on
// their own goroutines and can call concurrently with the dispatch goroutine
// or each other.
type fakeLifecycleSlack struct {
	mu            sync.Mutex
	reactions     []reactionCall
	posts         []postCall
	statuses      []statusCall
	reactionError string // if set, reactions.add responds {"ok":false,"error":reactionError}
	// statusClearDelay, when set, makes a setStatus("") call (a clearing
	// call — status=="") sleep before this fake records/answers it —
	// TestLifecycleAdapter_RunStatus_TerminalJoin_PreventsResumeInterleaving's
	// way of forcing the old-worker-still-mid-flight window the join fix
	// closes.
	statusClearDelay time.Duration
}

func newFakeLifecycleSlack(t *testing.T) *fakeLifecycleSlack {
	t.Helper()
	f := &fakeLifecycleSlack{}
	withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/reactions.add"):
			_ = r.ParseForm()
			f.mu.Lock()
			f.reactions = append(f.reactions, reactionCall{
				Channel: r.FormValue("channel"), TS: r.FormValue("timestamp"), Emoji: r.FormValue("name"),
			})
			errCode := f.reactionError
			f.mu.Unlock()
			if errCode != "" {
				writeSlackFakeJSON(w, map[string]any{"ok": false, "error": errCode})
				return
			}
			writeSlackFakeJSON(w, map[string]any{"ok": true})
		case strings.HasSuffix(r.URL.Path, "/chat.postMessage"):
			var body struct {
				Channel  string `json:"channel"`
				ThreadTS string `json:"thread_ts"`
				Text     string `json:"text"`
			}
			_ = decodeSlackFakeBody(r, &body)
			f.mu.Lock()
			f.posts = append(f.posts, postCall{Channel: body.Channel, ThreadTS: body.ThreadTS, Text: body.Text})
			f.mu.Unlock()
			writeSlackFakeJSON(w, map[string]any{"ok": true, "ts": "1700000099.000001"})
		case strings.HasSuffix(r.URL.Path, "/assistant.threads.setStatus"):
			_ = r.ParseForm()
			channel, threadTS, status := r.FormValue("channel_id"), r.FormValue("thread_ts"), r.FormValue("status")
			f.mu.Lock()
			delay := f.statusClearDelay
			f.mu.Unlock()
			if delay > 0 && status == "" {
				time.Sleep(delay)
			}
			// Recorded AFTER the delay (and always before the response is
			// written) so the slice's order reflects when each call actually
			// RESOLVED, not when its request arrived — the thing that
			// determines the indicator's final visible state.
			f.mu.Lock()
			f.statuses = append(f.statuses, statusCall{Channel: channel, ThreadTS: threadTS, Status: status})
			f.mu.Unlock()
			writeSlackFakeJSON(w, map[string]any{"ok": true})
		default:
			writeSlackFakeJSON(w, map[string]any{"ok": false, "error": "unhandled_path:" + r.URL.Path})
		}
	})
	return f
}

func (f *fakeLifecycleSlack) reactionCalls() []reactionCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]reactionCall, len(f.reactions))
	copy(out, f.reactions)
	return out
}

func (f *fakeLifecycleSlack) postCalls() []postCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]postCall, len(f.posts))
	copy(out, f.posts)
	return out
}

func (f *fakeLifecycleSlack) statusCalls() []statusCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]statusCall, len(f.statuses))
	copy(out, f.statuses)
	return out
}

func (f *fakeLifecycleSlack) setStatusClearDelay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusClearDelay = d
}

// ---------- rig / seeding ----------

// newLifecycleTestRig sets up a shared pgtest harness + a Postgres-backed
// store bundle + a fresh org, with the Slack entitlement on. Every lifecycle
// test builds its own lifecycleAdapter on top (publicURL/bus vary per test).
func newLifecycleTestRig(t *testing.T) (h *pgtest.Harness, stores db.Stores, fake *fakeLifecycleSlack, orgID, owner, teamID string) {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureSlack))
	t.Cleanup(entitlements.Reset)

	h = pgtest.Shared(t)
	h.Reset(t)
	stores = pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	fake = newFakeLifecycleSlack(t)
	orgID, owner, teamID = pgtest.SeedOrgWithUser(t, h, "slack-lifecycle")
	return h, stores, fake, orgID, owner, teamID
}

// newTestLifecycleAdapter builds an adapter with a nil bus accessor (tests
// feed synthetic domain.Event sentinels straight to dispatch, never through
// a real bus) and the given publicURL.
func newTestLifecycleAdapter(stores db.Stores, publicURL func() string) *lifecycleAdapter {
	return newLifecycleAdapter(stores, slackHTTPClient, publicURL, nil)
}

func staticURL(url string) func() string { return func() string { return url } }

// seedLifecycleWorkspace connects a Slack workspace (bot token + row) for
// orgID — mirrors exec_host_pg_test.go's seedWorkspace.
func seedLifecycleWorkspace(t *testing.T, stores db.Stores, orgID, userID, workspaceID, apiAppID, botToken string) {
	t.Helper()
	botTokenRef := "slack_ws_" + workspaceID + "_" + apiAppID + "_bot_token"
	if err := stores.Tx.WithTx(t.Context(), orgID, userID, func(tx db.TxStores) error {
		if err := tx.Secrets.Put(t.Context(), orgID, botTokenRef, botToken, "test bot token"); err != nil {
			return err
		}
		return slackstore.FromTx(tx).Workspaces.Upsert(t.Context(), slackstore.Workspace{
			WorkspaceID: workspaceID, APIAppID: apiAppID, OrgID: orgID, WorkspaceName: "Acme",
			Transport: "socket", BotUserID: "U0BOT", BotTokenRef: botTokenRef, RegisteredByUserID: userID,
		})
	}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
}

// seedSlackMentionEvent seeds a bare entity + slack:mention event (no task,
// no run) — enough for the disposition consumer, which only ever reads
// event metadata.
func seedSlackMentionEvent(t *testing.T, h *pgtest.Harness, orgID, workspaceID, apiAppID, channel, ts, threadTS string) (eventID string, meta SlackMentionMetadata) {
	t.Helper()
	entityID := uuid.New().String()
	root := threadTS
	if root == "" {
		root = ts
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title)
		VALUES ($1, $2, 'slack', $3, 'message', 'test thread')
	`, entityID, orgID, domain.SlackSourceID(channel, root)); err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	meta = SlackMentionMetadata{
		WorkspaceID: workspaceID, APIAppID: apiAppID, Channel: channel, TS: ts, ThreadTS: threadTS,
		SenderID: "U1", Text: "hey <@BOT>", EventID: "Ev" + uuid.New().String(),
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	eventID = uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, metadata_json)
		VALUES ($1, $2, $3, $4, $5)
	`, eventID, orgID, entityID, domain.EventSlackMention, string(metaJSON)); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	return eventID, meta
}

// slackRunFixture is what seedSlackMentionRun returns: the ids a run-status
// test needs to fire synthetic sentinels against.
type slackRunFixture struct {
	EventID, TaskID, RunID string
	Meta                   SlackMentionMetadata
}

// seedSlackMentionRun seeds the full chain resolveRunEntry walks: entity
// (source='slack') -> event (slack:mention, carrying SlackMentionMetadata)
// -> task (event_type=slack:mention, primary_event_id=event) -> run
// (task_id=task). Mirrors exec_host_pg_test.go's seedNonSlackTask/seedRun
// shape, but for the Slack-sourced case those tests deliberately avoid.
func seedSlackMentionRun(t *testing.T, h *pgtest.Harness, orgID, creatorID, teamID, workspaceID, apiAppID, channel, ts, threadTS string) slackRunFixture {
	t.Helper()
	eventID, meta := seedSlackMentionEvent(t, h, orgID, workspaceID, apiAppID, channel, ts, threadTS)

	// seedSlackMentionEvent already created the entity; find it via the event
	// row so the task can point at the same entity_id.
	var entityID string
	if err := h.AdminDB.QueryRow(`SELECT entity_id FROM events WHERE id = $1`, eventID).Scan(&entityID); err != nil {
		t.Fatalf("look up seeded event's entity: %v", err)
	}

	taskID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, entity_id, event_type, primary_event_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, taskID, orgID, creatorID, teamID, entityID, domain.EventSlackMention, eventID); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	runID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO runs (id, org_id, team_id, task_id, trigger_type, origin, status, visibility)
		VALUES ($1, $2, $3, $4, 'event', 'interactive', 'running', 'team')
	`, runID, orgID, teamID, taskID); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	return slackRunFixture{EventID: eventID, TaskID: taskID, RunID: runID, Meta: meta}
}

// seedGitHubTaskAndRun seeds a plain GitHub-sourced task + run — the
// non-Slack shape the cache tests need.
func seedGitHubTaskAndRun(t *testing.T, h *pgtest.Harness, orgID, creatorID, teamID string) (taskID, runID string) {
	t.Helper()
	entityID := uuid.New().String()
	sourceID := "octo/repo#" + uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO entities (id, org_id, source, source_id, kind, title)
		VALUES ($1, $2, 'github', $3, 'pull_request', 'test pr')
	`, entityID, orgID, sourceID); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	eventID := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO events (id, org_id, entity_id, event_type, metadata_json)
		VALUES ($1, $2, $3, $4, '{}')
	`, eventID, orgID, entityID, domain.EventGitHubPRCICheckFailed); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	taskID = uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO tasks (id, org_id, creator_user_id, team_id, entity_id, event_type, primary_event_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, taskID, orgID, creatorID, teamID, entityID, domain.EventGitHubPRCICheckFailed, eventID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	runID = uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO runs (id, org_id, team_id, task_id, trigger_type, origin, status, visibility)
		VALUES ($1, $2, $3, $4, 'event', 'interactive', 'running', 'team')
	`, runID, orgID, teamID, taskID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return taskID, runID
}

// ---------- synthetic sentinel builders ----------

func dispositionEvent(orgID, eventID, eventType, disposition string) domain.Event {
	meta, _ := json.Marshal(events.SystemRoutingDispositionMetadata{
		EventID: eventID, EventType: eventType, Disposition: disposition,
	})
	return domain.Event{OrgID: orgID, EventType: domain.EventSystemRoutingDisposition, MetadataJSON: string(meta)}
}

func runStatusEvent(orgID, runID, status string) domain.Event {
	meta, _ := json.Marshal(events.SystemRunStatusMetadata{RunID: runID, Status: status})
	return domain.Event{OrgID: orgID, EventType: domain.EventSystemRunStatus, MetadataJSON: string(meta)}
}

func runActivityEvent(orgID, runID string, tools []events.RunActivityTool) domain.Event {
	meta, _ := json.Marshal(events.SystemRunActivityMetadata{RunID: runID, Tools: tools})
	return domain.Event{OrgID: orgID, EventType: domain.EventSystemRunActivity, MetadataJSON: string(meta)}
}

// ---------- test helpers ----------

// withFastLifecycleTimings lowers the worker's tunables so tests never sleep
// real production durations (45s refresh, 3s debounce, 30m idle) — the
// ticket's own instruction ("injectable ticker interval — do not
// sleep-test real 45s").
func withFastLifecycleTimings(t *testing.T) {
	t.Helper()
	origRefresh, origDebounce, origIdle := slackLifecycleStatusRefreshInterval, slackLifecycleActivityDebounce, slackLifecycleIdleTimeout
	slackLifecycleStatusRefreshInterval = 60 * time.Millisecond
	slackLifecycleActivityDebounce = 40 * time.Millisecond
	slackLifecycleIdleTimeout = 5 * time.Second
	t.Cleanup(func() {
		slackLifecycleStatusRefreshInterval, slackLifecycleActivityDebounce, slackLifecycleIdleTimeout = origRefresh, origDebounce, origIdle
	})
}

// waitForCondition polls cond every 5ms until it's true or timeout elapses,
// failing the test on timeout — mirrors socket_test.go's inline poll-loop
// idiom, factored out since this file needs it repeatedly.
func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// stopAllLifecycleWorkers stops every live worker in runs — test cleanup so
// a worker's ticker doesn't keep firing (into a closed httptest server)
// after the test itself has ended. worker.stop already blocks until the
// worker has fully quiesced (see its doc); this just runs each stop off the
// test goroutine so a regression that hangs stop() fails with a t.Error
// instead of hanging the whole test.
func stopAllLifecycleWorkers(t *testing.T, runs map[string]*runEntry) {
	t.Helper()
	for _, e := range runs {
		if e.worker == nil {
			continue
		}
		done := make(chan struct{})
		go func(w *runStatusWorker) {
			w.stop(false)
			close(done)
		}(e.worker)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("worker did not stop within 2s of stop()")
		}
	}
}

// ---------- disposition consumer: 👀 acknowledge ----------

func TestLifecycleAdapter_Disposition_TaskCreatedOrBumped_ReactsWithEyes(t *testing.T) {
	for _, disp := range []string{events.DispositionTaskCreated, events.DispositionTaskBumped} {
		t.Run(disp, func(t *testing.T) {
			h, stores, fake, orgID, owner, _ := newLifecycleTestRig(t)
			seedLifecycleWorkspace(t, stores, orgID, owner, "T1", "A1", "xoxb-test")
			eventID, meta := seedSlackMentionEvent(t, h, orgID, "T1", "A1", "C1", "1700000000.000100", "")

			adapter := newTestLifecycleAdapter(stores, staticURL(""))
			adapter.dispatch(context.Background(), dispositionEvent(orgID, eventID, domain.EventSlackMention, disp), map[string]*runEntry{})

			calls := fake.reactionCalls()
			if len(calls) != 1 {
				t.Fatalf("reactions.add calls = %d, want 1: %+v", len(calls), calls)
			}
			if calls[0].Channel != meta.Channel || calls[0].TS != meta.TS || calls[0].Emoji != "eyes" {
				t.Errorf("reaction = %+v, want channel=%q ts=%q emoji=eyes", calls[0], meta.Channel, meta.TS)
			}
			if len(fake.postCalls()) != 0 {
				t.Error("must not post a message for task_created/task_bumped")
			}
		})
	}
}

// TestLifecycleAdapter_Disposition_AlreadyReacted_StillSuccess pins that
// Slack's own "already_reacted" idempotency (slackReactionsAdd's contract)
// flows through the adapter cleanly — the call happens, nothing panics or
// retries.
func TestLifecycleAdapter_Disposition_AlreadyReacted_StillSuccess(t *testing.T) {
	h, stores, fake, orgID, owner, _ := newLifecycleTestRig(t)
	seedLifecycleWorkspace(t, stores, orgID, owner, "T1", "A1", "xoxb-test")
	eventID, _ := seedSlackMentionEvent(t, h, orgID, "T1", "A1", "C1", "1700000000.000100", "")
	fake.reactionError = "already_reacted"

	adapter := newTestLifecycleAdapter(stores, staticURL(""))
	adapter.dispatch(context.Background(), dispositionEvent(orgID, eventID, domain.EventSlackMention, events.DispositionTaskCreated), map[string]*runEntry{})

	if len(fake.reactionCalls()) != 1 {
		t.Fatalf("reactions.add calls = %d, want 1", len(fake.reactionCalls()))
	}
}

// ---------- disposition consumer: no-match reply ----------

func TestLifecycleAdapter_Disposition_TasklessNoHandlerOrOwner_PostsNotConfiguredReply(t *testing.T) {
	for _, disp := range []string{events.DispositionTasklessNoHandler, events.DispositionTasklessNoOwner} {
		t.Run(disp, func(t *testing.T) {
			h, stores, fake, orgID, owner, _ := newLifecycleTestRig(t)
			seedLifecycleWorkspace(t, stores, orgID, owner, "T1", "A1", "xoxb-test")
			eventID, meta := seedSlackMentionEvent(t, h, orgID, "T1", "A1", "C1", "1700000000.000200", "1700000000.000100")

			adapter := newTestLifecycleAdapter(stores, staticURL(""))
			adapter.dispatch(context.Background(), dispositionEvent(orgID, eventID, domain.EventSlackMention, disp), map[string]*runEntry{})

			posts := fake.postCalls()
			if len(posts) != 1 {
				t.Fatalf("chat.postMessage calls = %d, want 1: %+v", len(posts), posts)
			}
			if posts[0].Channel != meta.Channel || posts[0].ThreadTS != meta.ThreadTS {
				t.Errorf("post = %+v, want channel=%q thread_ts=%q (the thread root)", posts[0], meta.Channel, meta.ThreadTS)
			}
			if !strings.Contains(posts[0].Text, "no team is set up") {
				t.Errorf("text = %q, want the not-configured copy", posts[0].Text)
			}
			if len(fake.reactionCalls()) != 0 {
				t.Error("must not react for a taskless disposition")
			}
		})
	}
}

// TestLifecycleAdapter_Disposition_TasklessNoHandler_RootMessage_RepliesInOwnThread
// covers a root-message mention (no ThreadTS) — the reply's thread root must
// fall back to the mention's own ts, starting a new thread there.
func TestLifecycleAdapter_Disposition_TasklessNoHandler_RootMessage_RepliesInOwnThread(t *testing.T) {
	h, stores, fake, orgID, owner, _ := newLifecycleTestRig(t)
	seedLifecycleWorkspace(t, stores, orgID, owner, "T1", "A1", "xoxb-test")
	eventID, meta := seedSlackMentionEvent(t, h, orgID, "T1", "A1", "C1", "1700000000.000300", "")

	adapter := newTestLifecycleAdapter(stores, staticURL(""))
	adapter.dispatch(context.Background(), dispositionEvent(orgID, eventID, domain.EventSlackMention, events.DispositionTasklessNoHandler), map[string]*runEntry{})

	posts := fake.postCalls()
	if len(posts) != 1 || posts[0].ThreadTS != meta.TS {
		t.Fatalf("post = %+v, want exactly one reply threaded at the mention's own ts %q", posts, meta.TS)
	}
}

// TestLifecycleAdapter_Disposition_MissingWorkspace_NoCallNoPanic covers the
// "workspace row + token must resolve" gate: an unconnected (workspace_id,
// api_app_id) never calls Slack.
func TestLifecycleAdapter_Disposition_MissingWorkspace_NoCallNoPanic(t *testing.T) {
	h, stores, fake, orgID, _, _ := newLifecycleTestRig(t)
	// Deliberately no seedLifecycleWorkspace call.
	eventID, _ := seedSlackMentionEvent(t, h, orgID, "T-missing", "A-missing", "C1", "1700000000.000400", "")

	adapter := newTestLifecycleAdapter(stores, staticURL(""))
	adapter.dispatch(context.Background(), dispositionEvent(orgID, eventID, domain.EventSlackMention, events.DispositionTaskCreated), map[string]*runEntry{})

	if len(fake.reactionCalls()) != 0 || len(fake.postCalls()) != 0 {
		t.Errorf("expected no Slack calls with no connected workspace, got reactions=%v posts=%v", fake.reactionCalls(), fake.postCalls())
	}
}

// TestLifecycleAdapter_Disposition_InertOutcomes_NoOp covers dormancy/error
// outcomes that must never touch Slack — frozen, taskless_unroutable, error.
func TestLifecycleAdapter_Disposition_InertOutcomes_NoOp(t *testing.T) {
	for _, disp := range []string{events.DispositionFrozen, events.DispositionTasklessUnroutable, events.DispositionError} {
		t.Run(disp, func(t *testing.T) {
			h, stores, fake, orgID, owner, _ := newLifecycleTestRig(t)
			seedLifecycleWorkspace(t, stores, orgID, owner, "T1", "A1", "xoxb-test")
			eventID, _ := seedSlackMentionEvent(t, h, orgID, "T1", "A1", "C1", "1700000000.000500", "")

			adapter := newTestLifecycleAdapter(stores, staticURL(""))
			adapter.dispatch(context.Background(), dispositionEvent(orgID, eventID, domain.EventSlackMention, disp), map[string]*runEntry{})

			if len(fake.reactionCalls()) != 0 || len(fake.postCalls()) != 0 {
				t.Errorf("%s must be inert, got reactions=%v posts=%v", disp, fake.reactionCalls(), fake.postCalls())
			}
		})
	}
}

// TestLifecycleAdapter_Disposition_NonSlackEventType_Ignored pins the
// EventType != slack:mention filter — a disposition for a GitHub/Jira event
// must never reach the mention-specific handling at all (which would
// otherwise misinterpret its EventID as a Slack mention's).
func TestLifecycleAdapter_Disposition_NonSlackEventType_Ignored(t *testing.T) {
	_, stores, fake, orgID, _, _ := newLifecycleTestRig(t)
	adapter := newTestLifecycleAdapter(stores, staticURL(""))
	adapter.dispatch(context.Background(), dispositionEvent(orgID, "some-github-event-id", domain.EventGitHubPRCICheckFailed, events.DispositionTaskCreated), map[string]*runEntry{})

	if len(fake.reactionCalls()) != 0 || len(fake.postCalls()) != 0 {
		t.Error("a non-slack:mention disposition must never call Slack")
	}
}

// ---------- run-status consumer: setStatus lifecycle ----------

func TestLifecycleAdapter_RunStatus_RunningThenActivity_SetsDescriptionDerivedText(t *testing.T) {
	withFastLifecycleTimings(t)
	h, stores, fake, orgID, owner, teamID := newLifecycleTestRig(t)
	seedLifecycleWorkspace(t, stores, orgID, owner, "T1", "A1", "xoxb-test")
	fx := seedSlackMentionRun(t, h, orgID, owner, teamID, "T1", "A1", "C1", "1700000000.000100", "")

	adapter := newTestLifecycleAdapter(stores, staticURL(""))
	runs := map[string]*runEntry{}
	t.Cleanup(func() { stopAllLifecycleWorkers(t, runs) })
	ctx := context.Background()

	adapter.dispatch(ctx, runStatusEvent(orgID, fx.RunID, "running"), runs)
	waitForCondition(t, 2*time.Second, func() bool { return len(fake.statusCalls()) >= 1 })
	first := fake.statusCalls()[0]
	if first.Channel != fx.Meta.Channel || first.Status != slackLifecycleInitialStatusText {
		t.Errorf("initial status call = %+v, want channel=%q status=%q", first, fx.Meta.Channel, slackLifecycleInitialStatusText)
	}

	adapter.dispatch(ctx, runActivityEvent(orgID, fx.RunID, []events.RunActivityTool{{Name: "Bash", Description: "Running go test ./..."}}), runs)
	waitForCondition(t, 2*time.Second, func() bool {
		calls := fake.statusCalls()
		return len(calls) > 0 && calls[len(calls)-1].Status == "is running: Running go test ./..."
	})
}

// TestLifecycleAdapter_RunStatus_ActivityBurst_DebounceCollapsesToOneCall
// fires a rapid burst of activity sentinels and pins that they collapse to
// exactly one additional setStatus call, carrying the LAST event's text.
func TestLifecycleAdapter_RunStatus_ActivityBurst_DebounceCollapsesToOneCall(t *testing.T) {
	withFastLifecycleTimings(t)
	// The refresh ticker is independent of the debounce window; push it well
	// past this test's short duration so it can't sneak in an extra call.
	origRefresh := slackLifecycleStatusRefreshInterval
	slackLifecycleStatusRefreshInterval = 10 * time.Second
	t.Cleanup(func() { slackLifecycleStatusRefreshInterval = origRefresh })

	h, stores, fake, orgID, owner, teamID := newLifecycleTestRig(t)
	seedLifecycleWorkspace(t, stores, orgID, owner, "T1", "A1", "xoxb-test")
	fx := seedSlackMentionRun(t, h, orgID, owner, teamID, "T1", "A1", "C1", "1700000000.000100", "")

	adapter := newTestLifecycleAdapter(stores, staticURL(""))
	runs := map[string]*runEntry{}
	t.Cleanup(func() { stopAllLifecycleWorkers(t, runs) })
	ctx := context.Background()

	adapter.dispatch(ctx, runStatusEvent(orgID, fx.RunID, "running"), runs)
	waitForCondition(t, 2*time.Second, func() bool { return len(fake.statusCalls()) >= 1 })
	before := len(fake.statusCalls())

	for i := 0; i < 5; i++ {
		adapter.dispatch(ctx, runActivityEvent(orgID, fx.RunID, []events.RunActivityTool{{Name: fmt.Sprintf("tool_%d", i)}}), runs)
	}

	waitForCondition(t, 2*time.Second, func() bool { return len(fake.statusCalls()) > before })
	// Give any (incorrect) extra calls a chance to land before asserting the count.
	time.Sleep(slackLifecycleActivityDebounce * 3)

	newCalls := fake.statusCalls()[before:]
	if len(newCalls) != 1 {
		t.Fatalf("burst of 5 activity events produced %d new setStatus calls, want exactly 1: %+v", len(newCalls), newCalls)
	}
	if want := "is running: tool 4"; newCalls[0].Status != want {
		t.Errorf("collapsed call text = %q, want %q (latest wins)", newCalls[0].Status, want)
	}
}

// TestLifecycleAdapter_RunStatus_TickerRefresh_Fires pins the dead-man-switch
// ticker: with no further activity, setStatus keeps getting re-sent on the
// (lowered, injectable) refresh interval.
func TestLifecycleAdapter_RunStatus_TickerRefresh_Fires(t *testing.T) {
	withFastLifecycleTimings(t)
	h, stores, fake, orgID, owner, teamID := newLifecycleTestRig(t)
	seedLifecycleWorkspace(t, stores, orgID, owner, "T1", "A1", "xoxb-test")
	fx := seedSlackMentionRun(t, h, orgID, owner, teamID, "T1", "A1", "C1", "1700000000.000100", "")

	adapter := newTestLifecycleAdapter(stores, staticURL(""))
	runs := map[string]*runEntry{}
	t.Cleanup(func() { stopAllLifecycleWorkers(t, runs) })

	adapter.dispatch(context.Background(), runStatusEvent(orgID, fx.RunID, "running"), runs)
	waitForCondition(t, 3*time.Second, func() bool { return len(fake.statusCalls()) >= 3 })

	for _, c := range fake.statusCalls() {
		if c.Status != slackLifecycleInitialStatusText {
			t.Errorf("ticker refresh call = %+v, want the unchanged initial text (no activity happened)", c)
		}
	}
}

// TestLifecycleAdapter_RunStatus_Failed_ClearsAndPostsFailureReplyWithURL
// covers the terminal "failed" arm: the indicator clears (a final
// setStatus("")) and a thread reply lands with the run URL fragment when
// PublicURL() is non-empty.
func TestLifecycleAdapter_RunStatus_Failed_ClearsAndPostsFailureReplyWithURL(t *testing.T) {
	withFastLifecycleTimings(t)
	h, stores, fake, orgID, owner, teamID := newLifecycleTestRig(t)
	seedLifecycleWorkspace(t, stores, orgID, owner, "T1", "A1", "xoxb-test")
	fx := seedSlackMentionRun(t, h, orgID, owner, teamID, "T1", "A1", "C1", "1700000000.000100", "")

	adapter := newTestLifecycleAdapter(stores, staticURL("https://tf.example.com"))
	runs := map[string]*runEntry{}
	t.Cleanup(func() { stopAllLifecycleWorkers(t, runs) })
	ctx := context.Background()

	adapter.dispatch(ctx, runStatusEvent(orgID, fx.RunID, "running"), runs)
	waitForCondition(t, 2*time.Second, func() bool { return len(fake.statusCalls()) >= 1 })

	adapter.dispatch(ctx, runStatusEvent(orgID, fx.RunID, "failed"), runs)
	waitForCondition(t, 2*time.Second, func() bool {
		calls := fake.statusCalls()
		return len(calls) > 0 && calls[len(calls)-1].Status == ""
	})
	waitForCondition(t, 2*time.Second, func() bool { return len(fake.postCalls()) >= 1 })

	posts := fake.postCalls()
	last := posts[len(posts)-1]
	wantURL := fmt.Sprintf("https://tf.example.com/orgs/%s/runs/%s", orgID, fx.RunID)
	if !strings.Contains(last.Text, "Something went wrong") || !strings.Contains(last.Text, wantURL) {
		t.Errorf("failure reply = %q, want the failure copy + %q", last.Text, wantURL)
	}
	if entry := runs[fx.RunID]; entry.worker != nil {
		t.Error("worker should have been cleared from the run entry on terminal status")
	}
}

// TestLifecycleAdapter_RunStatus_Failed_NoPublicURL_NoURLFragment covers the
// "omit the link entirely" rule when api.PublicURL() is "".
func TestLifecycleAdapter_RunStatus_Failed_NoPublicURL_NoURLFragment(t *testing.T) {
	withFastLifecycleTimings(t)
	h, stores, fake, orgID, owner, teamID := newLifecycleTestRig(t)
	seedLifecycleWorkspace(t, stores, orgID, owner, "T1", "A1", "xoxb-test")
	fx := seedSlackMentionRun(t, h, orgID, owner, teamID, "T1", "A1", "C1", "1700000000.000100", "")

	adapter := newTestLifecycleAdapter(stores, staticURL(""))
	runs := map[string]*runEntry{}
	t.Cleanup(func() { stopAllLifecycleWorkers(t, runs) })
	ctx := context.Background()

	// Never went "running" — a run that fails during setup still owes the
	// failure note, via the worker-less direct path in handleRunStatus.
	adapter.dispatch(ctx, runStatusEvent(orgID, fx.RunID, "failed"), runs)
	waitForCondition(t, 2*time.Second, func() bool { return len(fake.postCalls()) >= 1 })

	last := fake.postCalls()[len(fake.postCalls())-1]
	if !strings.Contains(last.Text, "Something went wrong") || strings.Contains(last.Text, "Details:") {
		t.Errorf("failure reply = %q, want the failure copy with no Details: fragment", last.Text)
	}
	if len(fake.statusCalls()) != 0 {
		t.Errorf("no worker ever started — expected zero setStatus calls, got %+v", fake.statusCalls())
	}
}

// TestLifecycleAdapter_RunStatus_Parked_ClearsWithoutFailureReply covers the
// "open" (parked/awaiting-input) terminal: cleared silently, no message.
func TestLifecycleAdapter_RunStatus_Parked_ClearsWithoutFailureReply(t *testing.T) {
	withFastLifecycleTimings(t)
	h, stores, fake, orgID, owner, teamID := newLifecycleTestRig(t)
	seedLifecycleWorkspace(t, stores, orgID, owner, "T1", "A1", "xoxb-test")
	fx := seedSlackMentionRun(t, h, orgID, owner, teamID, "T1", "A1", "C1", "1700000000.000100", "")

	adapter := newTestLifecycleAdapter(stores, staticURL(""))
	runs := map[string]*runEntry{}
	t.Cleanup(func() { stopAllLifecycleWorkers(t, runs) })
	ctx := context.Background()

	adapter.dispatch(ctx, runStatusEvent(orgID, fx.RunID, "running"), runs)
	waitForCondition(t, 2*time.Second, func() bool { return len(fake.statusCalls()) >= 1 })

	adapter.dispatch(ctx, runStatusEvent(orgID, fx.RunID, "open"), runs)
	waitForCondition(t, 2*time.Second, func() bool {
		calls := fake.statusCalls()
		return len(calls) > 0 && calls[len(calls)-1].Status == ""
	})

	// Give a wrongly-posted failure reply a chance to land before asserting none did.
	time.Sleep(50 * time.Millisecond)
	if len(fake.postCalls()) != 0 {
		t.Errorf("parked/open must never post the failure reply, got %+v", fake.postCalls())
	}
}

// TestLifecycleAdapter_RunStatus_TerminalJoin_PreventsResumeInterleaving is
// the direct regression test for the park→resume ordering fix: worker.stop
// now blocks until the retiring worker's trailing setStatus("") has
// actually resolved, so a fast-following "running" for the SAME run can't
// start a new worker whose initial status then gets clobbered by the old
// worker's late-arriving clear.
//
// The fake's setStatusClearDelay makes the OLD worker's clearing call slow
// (simulating it being mid-flight on a real Slack round trip when "open"
// dispatches) — long enough that, under the pre-fix non-blocking stop, the
// resumed run's new "is working on it…" call would complete and get
// recorded FIRST, with the stale clear landing after it and wiping the
// indicator right back to blank. Recording happens after each call resolves
// (see the fake's setStatus handler), so the LAST entry in statusCalls()
// always reflects the indicator's actual final state — asserting it here is
// exactly what would have failed against the old non-blocking stop.
func TestLifecycleAdapter_RunStatus_TerminalJoin_PreventsResumeInterleaving(t *testing.T) {
	withFastLifecycleTimings(t)
	h, stores, fake, orgID, owner, teamID := newLifecycleTestRig(t)
	seedLifecycleWorkspace(t, stores, orgID, owner, "T1", "A1", "xoxb-test")
	fx := seedSlackMentionRun(t, h, orgID, owner, teamID, "T1", "A1", "C1", "1700000000.000100", "")

	adapter := newTestLifecycleAdapter(stores, staticURL(""))
	runs := map[string]*runEntry{}
	t.Cleanup(func() { stopAllLifecycleWorkers(t, runs) })
	ctx := context.Background()

	adapter.dispatch(ctx, runStatusEvent(orgID, fx.RunID, "running"), runs)
	waitForCondition(t, 2*time.Second, func() bool { return len(fake.statusCalls()) >= 1 })

	fake.setStatusClearDelay(150 * time.Millisecond)
	adapter.dispatch(ctx, runStatusEvent(orgID, fx.RunID, "open"), runs) // parked — resumable
	// No wait needed: dispatch() itself must not return until the old
	// worker's delayed clear has already resolved (that's the fix).
	fake.setStatusClearDelay(0)

	adapter.dispatch(ctx, runStatusEvent(orgID, fx.RunID, "running"), runs) // resumes immediately
	waitForCondition(t, 2*time.Second, func() bool {
		calls := fake.statusCalls()
		return len(calls) > 0 && calls[len(calls)-1].Status == slackLifecycleInitialStatusText
	})

	last := fake.statusCalls()[len(fake.statusCalls())-1]
	if last.Status != slackLifecycleInitialStatusText {
		t.Errorf("final indicator state = %q, want %q (the resumed run's new worker, not the old worker's stale clear)", last.Status, slackLifecycleInitialStatusText)
	}
}

// ---------- run-status consumer: non-Slack cache ----------

// TestLifecycleAdapter_RunStatus_NonSlackRun_CachedAfterFirstLookup pins the
// per-runID cache: after the first system:run:status resolves a run as
// non-Slack, the underlying data is mutated to look Slack-shaped (entity
// source flipped to 'slack', task's event_type flipped to slack:mention) —
// if resolution were re-run on a later sentinel for the SAME runID, it would
// now (incorrectly) start a worker and call Slack. It must not: the cached
// negative verdict is never re-queried.
func TestLifecycleAdapter_RunStatus_NonSlackRun_CachedAfterFirstLookup(t *testing.T) {
	h, stores, fake, orgID, owner, teamID := newLifecycleTestRig(t)
	taskID, runID := seedGitHubTaskAndRun(t, h, orgID, owner, teamID)

	adapter := newTestLifecycleAdapter(stores, staticURL(""))
	runs := map[string]*runEntry{}
	ctx := context.Background()

	adapter.dispatch(ctx, runStatusEvent(orgID, runID, "running"), runs)
	entry, ok := runs[runID]
	if !ok {
		t.Fatal("expected a cache entry after the first system:run:status")
	}
	if entry.isSlack {
		t.Fatal("expected isSlack=false for a GitHub-sourced task")
	}
	if len(fake.statusCalls()) != 0 {
		t.Fatalf("must not call Slack for a non-Slack run, got %+v", fake.statusCalls())
	}

	// Mutate the underlying rows so a FRESH lookup would now resolve Slack.
	var entityID string
	if err := h.AdminDB.QueryRow(`SELECT entity_id FROM tasks WHERE id = $1`, taskID).Scan(&entityID); err != nil {
		t.Fatalf("look up task's entity: %v", err)
	}
	if _, err := h.AdminDB.Exec(`UPDATE entities SET source = 'slack' WHERE id = $1`, entityID); err != nil {
		t.Fatalf("mutate entity source: %v", err)
	}
	if _, err := h.AdminDB.Exec(`UPDATE tasks SET event_type = $1 WHERE id = $2`, domain.EventSlackMention, taskID); err != nil {
		t.Fatalf("mutate task event_type: %v", err)
	}

	adapter.dispatch(ctx, runStatusEvent(orgID, runID, "running"), runs)
	adapter.dispatch(ctx, runActivityEvent(orgID, runID, []events.RunActivityTool{{Name: "Bash"}}), runs)

	time.Sleep(50 * time.Millisecond)
	if len(fake.statusCalls()) != 0 {
		t.Errorf("cached isSlack=false must not be re-resolved, got Slack calls: %+v", fake.statusCalls())
	}
	if runs[runID].worker != nil {
		t.Error("no worker should ever start for a cached non-Slack run")
	}
}

// ---------- run-activity consumer: unknown run ----------

// TestLifecycleAdapter_RunActivity_UnknownRun_Ignored covers activity
// arriving for a runID this adapter has never seen a system:run:status for
// (the verdict-priming event) — it must be dropped, not trigger its own
// resolution.
func TestLifecycleAdapter_RunActivity_UnknownRun_Ignored(t *testing.T) {
	_, stores, fake, orgID, _, _ := newLifecycleTestRig(t)
	adapter := newTestLifecycleAdapter(stores, staticURL(""))
	runs := map[string]*runEntry{}

	adapter.dispatch(context.Background(), runActivityEvent(orgID, "unseen-run-id", []events.RunActivityTool{{Name: "Bash"}}), runs)

	if _, ok := runs["unseen-run-id"]; ok {
		t.Error("run-activity must not prime the cache — only run-status does")
	}
	if len(fake.statusCalls()) != 0 {
		t.Errorf("expected no Slack calls, got %+v", fake.statusCalls())
	}
}
