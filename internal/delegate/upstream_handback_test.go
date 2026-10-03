package delegate

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

// unavailableProvider answers every call the way a provider in an outage
// does: a 503, whatever was asked.
type unavailableProvider struct{ calls atomic.Int32 }

func (p *unavailableProvider) Stream(context.Context, inference.Request) (*inference.Completion, error) {
	p.calls.Add(1)
	return nil, errors.New("503 service unavailable")
}

// handBackCounter backs the hand-back counter with a manual reader for the
// test's duration and returns a reader of its value at one (outcome, org).
func handBackCounter(t *testing.T) func(outcome, orgID string) int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := handBacks.Swap(newHandBackStats(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	t.Cleanup(func() { handBacks.Store(prev) })
	return func(outcome, orgID string) int64 {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		want := attribute.NewSet(attribute.String("outcome", outcome), attribute.String("org.id", orgID))
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "conversations.handed_back" {
					continue
				}
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("conversations.handed_back is %T, want an int64 sum", m.Data)
				}
				for _, dp := range sum.DataPoints {
					if dp.Attributes.Equals(&want) {
						return dp.Value
					}
				}
			}
		}
		return 0
	}
}

// upstreamFixture is a native engagement whose provider is unavailable, run
// through the real engine to its end, with a workspace on disk and a blob
// store for the ending to snapshot it into.
type upstreamFixture struct {
	stallFixture
	result    agentloop.Result
	provider  *unavailableProvider
	wtPath    string
	namespace string
}

func newUpstreamFixture(t *testing.T, conversationID string) upstreamFixture {
	t.Helper()
	f := newStallFixture(t, conversationID, activityTimings{idle: time.Hour})
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	f.s.SetStorage(blobs)
	wtPath := t.TempDir()
	writeFile(t, filepath.Join(wtPath, "_tfac", "notes.txt"), "work before the outage")

	provider := &unavailableProvider{}
	result := f.runNative(t, provider, &recordingToolHost{})
	if result.Kind != agentloop.ResultUpstreamUnavailable {
		t.Fatalf("engine result = %v (err %v), want ResultUpstreamUnavailable", result.Kind, result.Err)
	}
	return upstreamFixture{stallFixture: f, result: result, provider: provider, wtPath: wtPath, namespace: workspaceKey(f.task.ID)}
}

func (f upstreamFixture) record(t *testing.T, upstreamHandBacks int) engagementDisposition {
	t.Helper()
	return f.s.recordNativeResult(f.claimCtx, runmode.LocalDefaultOrgID, f.conversationID, f.task,
		runConfig{orgID: runmode.LocalDefaultOrgID, claimID: f.claimID, upstreamHandBacks: upstreamHandBacks},
		f.namespace, f.wtPath, "manual", runmode.LocalDefaultUserID, time.Now(), f.result, nil)
}

// nextAttemptIn reads next_attempt_at as an offset from database now, on the
// database's own clock and in the layout the claim scan compares as text.
func (f upstreamFixture) nextAttemptIn(t *testing.T) (time.Duration, bool) {
	t.Helper()
	var at, now *string
	if err := f.database.QueryRow(
		`SELECT CAST(next_attempt_at AS TEXT), strftime('%Y-%m-%d %H:%M:%f','now') FROM conversations WHERE id = ?`, f.conversationID,
	).Scan(&at, &now); err != nil {
		t.Fatalf("read next_attempt_at: %v", err)
	}
	if at == nil {
		return 0, false
	}
	const layout = "2006-01-02 15:04:05.000"
	atT, err := time.Parse(layout, *at)
	if err != nil {
		t.Fatalf("next_attempt_at %q is not in the layout the claim scan compares: %v", *at, err)
	}
	nowT, err := time.Parse(layout, *now)
	if err != nil {
		t.Fatalf("parse database now %q: %v", *now, err)
	}
	return atT.Sub(nowT), true
}

func (f upstreamFixture) blueprintStatus(t *testing.T) string {
	t.Helper()
	var status string
	if err := f.database.QueryRow(
		`SELECT br.status FROM blueprint_runs br JOIN conversations c ON c.blueprint_run_id = br.id WHERE c.id = ?`, f.conversationID,
	).Scan(&status); err != nil {
		t.Fatalf("read blueprint status: %v", err)
	}
	return status
}

// TestUpstream_NativeEngagementHandsBackToRetryLater: a native engagement
// whose provider answers 503 through every in-turn retry gives its claim back
// 'requeued_upstream' with the first wait on the schedule, snapshots its
// workspace on the way out, and writes nothing a person or the model would
// read as a failure. The conversation is not claimable until the wait is over,
// and the claim after it counts the hand-back.
func TestUpstream_NativeEngagementHandsBackToRetryLater(t *testing.T) {
	counted := handBackCounter(t)
	f := newUpstreamFixture(t, "r-upstream-handback")
	if got := f.provider.calls.Load(); got != 5 {
		t.Errorf("provider attempts = %d, want the loop's 5 before the engagement gives up", got)
	}

	disp := f.record(t, 0)
	if !disp.handedBack || disp.fenced {
		t.Fatalf("disposition = %+v, want handed back and unfenced", disp)
	}
	if got := storedStatus(t, f.database, f.conversationID); got != "" {
		t.Errorf("stored status = %q, want none — the conversation is mid-flight, not failed or parked", got)
	}
	if got := claimOutcome(t, f.stallFixture, f.claimID); got != db.HandBackUpstream {
		t.Errorf("claim outcome = %q, want %s", got, db.HandBackUpstream)
	}
	if in, ok := f.nextAttemptIn(t); !ok || in > 30*time.Second || in < 20*time.Second {
		t.Errorf("next_attempt_at = now + %v (set %v), want about now + 30s", in, ok)
	}
	var summary string
	if err := f.database.QueryRow(`SELECT COALESCE(result_summary, '') FROM conversations WHERE id = ?`, f.conversationID).Scan(&summary); err != nil {
		t.Fatalf("read result_summary: %v", err)
	}
	if !strings.Contains(summary, "503") {
		t.Errorf("result_summary = %q, want the provider's last failure", summary)
	}
	for _, m := range allRows(t, f.s, f.conversationID) {
		if strings.Contains(m.Content, "could not be retried") || m.Subtype == domain.MessageSubtypeStopNote {
			t.Errorf("the transcript gained %q (subtype %q); an unavailable provider writes nothing to it", m.Content, m.Subtype)
		}
	}
	if got := f.blueprintStatus(t); got != "running" {
		t.Errorf("blueprint status = %q, want running", got)
	}

	rc, err := f.s.Storage().Get(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, f.namespace))
	if err != nil {
		t.Fatalf("the hand-back wrote no workspace snapshot: %v", err)
	}
	_ = rc.Close()
	state, err := f.s.snapshotStateFor(context.Background(), runmode.LocalDefaultOrgID, f.namespace)
	if err != nil || state == nil || state.State != domain.WorkspaceSnapshotWritten || state.WriterClaimID != f.claimID {
		t.Errorf("snapshot record = (%+v, %v), want written by the handed-back claim", state, err)
	}

	if got := counted(db.HandBackUpstream, runmode.LocalDefaultOrgID); got != 1 {
		t.Errorf("conversations.handed_back{requeued_upstream} = %d, want 1", got)
	}

	// Not claimable while the wait is ahead; once it is over, the claim
	// carries the hand-back it continues from, and clears the wait.
	q := f.s.conversationQueue
	if next, err := q.ClaimNextConversation(context.Background(), "exec-successor", 1, db.ClaimPlacement{}, time.Minute); err != nil || next != nil {
		t.Fatalf("claim during the wait = (%+v, %v), want nothing claimable", next, err)
	}
	if _, err := f.database.Exec(`UPDATE conversations SET next_attempt_at = strftime('%Y-%m-%d %H:%M:%f','now','-1.000 seconds') WHERE id = ?`, f.conversationID); err != nil {
		t.Fatalf("end the wait: %v", err)
	}
	next, err := q.ClaimNextConversation(context.Background(), "exec-successor", 1, db.ClaimPlacement{}, time.Minute)
	if err != nil || next == nil || next.ID != f.conversationID {
		t.Fatalf("claim after the wait = (%+v, %v), want conversation %s", next, err, f.conversationID)
	}
	if next.UpstreamHandBacks != 1 || next.SetupFailures != 0 || next.LostEngagements != 0 {
		t.Errorf("successor's budgets = (upstream %d, setup %d, lost %d), want (1, 0, 0)", next.UpstreamHandBacks, next.SetupFailures, next.LostEngagements)
	}
	if _, ok := f.nextAttemptIn(t); ok {
		t.Error("the claim left next_attempt_at set")
	}

	// The provider is back. The successor's engine, on its own claim, asks the
	// model to continue the transcript the first engagement left, with the
	// mission in it and no failure in it.
	successorCtx, cancelSuccessor := context.WithCancel(context.Background())
	defer cancelSuccessor()
	capture := capturingProvider{rows: make(chan []domain.Message, 1)}
	done := make(chan agentloop.Result, 1)
	go func() {
		engine := &agentloop.Engine{
			Transcript:  newNativeTranscript(f.s, runmode.LocalDefaultOrgID, f.conversationID, next.ClaimID),
			Credentials: staticGateCredentials{provider: capture},
			Tools:       &recordingToolHost{},
			Retry:       agentloop.RetryPolicy{Sleep: func(context.Context, time.Duration) error { return nil }},
		}
		done <- engine.Run(successorCtx, agentloop.Params{
			OrgID: runmode.LocalDefaultOrgID, ConversationID: f.conversationID,
			Model: "claude-sonnet-4-5", SystemPrompt: "system", HasBlueprint: true,
		})
	}()
	var rows []domain.Message
	select {
	case rows = <-capture.rows:
	case <-time.After(10 * time.Second):
		t.Fatal("the successor never asked the model anything")
	}
	cancelSuccessor()
	<-done
	var sawMission bool
	for _, r := range rows {
		if r.Role == "user" && strings.Contains(r.Content, "do the work") {
			sawMission = true
		}
		if strings.Contains(r.Content, "could not be retried") || strings.Contains(r.Content, "503") {
			t.Errorf("the successor's request carries the outage: %q", r.Content)
		}
	}
	if !sawMission {
		t.Errorf("the successor's request does not continue the conversation: %+v", rows)
	}
}

// TestUpstream_TheWaitFollowsTheSchedule: the wait is picked by how many
// upstream hand-backs the episode already holds, and the last entry repeats.
func TestUpstream_TheWaitFollowsTheSchedule(t *testing.T) {
	f := newUpstreamFixture(t, "r-upstream-schedule")
	if disp := f.record(t, 7); !disp.handedBack {
		t.Fatalf("disposition = %+v, want handed back", disp)
	}
	if in, ok := f.nextAttemptIn(t); !ok || in > 10*time.Minute || in < 10*time.Minute-10*time.Second {
		t.Errorf("next_attempt_at = now + %v (set %v), want about now + 10m", in, ok)
	}
}

// TestUpstream_ASpentBudgetParksForAPerson: the engagement that would be the
// budget's last hand-back parks the conversation instead, as
// upstream_unavailable with a note saying what to do. It is never failed, and
// its blueprint keeps running.
func TestUpstream_ASpentBudgetParksForAPerson(t *testing.T) {
	counted := handBackCounter(t)
	f := newUpstreamFixture(t, "r-upstream-exhausted")

	disp := f.record(t, maxUpstreamHandBacks-1)
	if disp.handedBack || disp.fenced {
		t.Fatalf("disposition = %+v, want a park", disp)
	}
	var status, parkReason string
	if err := f.database.QueryRow(
		`SELECT COALESCE(status, ''), COALESCE(park_reason, '') FROM conversations WHERE id = ?`, f.conversationID,
	).Scan(&status, &parkReason); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if status != domain.StatusOpen || parkReason != string(domain.ParkReasonUpstreamUnavailable) {
		t.Errorf("conversation = (%q, %q), want (open, upstream_unavailable)", status, parkReason)
	}
	if got := claimOutcome(t, f.stallFixture, f.claimID); got == db.HandBackUpstream || got == "" {
		t.Errorf("claim outcome = %q, want the park's, not another hand-back", got)
	}
	if _, ok := f.nextAttemptIn(t); ok {
		t.Error("a parked conversation carries next_attempt_at")
	}
	var notes int
	for _, m := range allRows(t, f.s, f.conversationID) {
		if m.Subtype == domain.MessageSubtypeStopNote && m.Content == upstreamExhaustedNote {
			notes++
		}
		if strings.Contains(m.Content, "could not be retried") {
			t.Errorf("the transcript gained the failure notice %q; the run did not fail", m.Content)
		}
	}
	if notes != 1 {
		t.Errorf("stop notes saying why = %d, want 1", notes)
	}
	if got := f.blueprintStatus(t); got != "running" {
		t.Errorf("blueprint status = %q, want running", got)
	}
	if got := counted(db.HandBackUpstream, runmode.LocalDefaultOrgID); got != 0 {
		t.Errorf("conversations.handed_back{requeued_upstream} = %d, want 0 — a park is not a hand-back", got)
	}
	if next, err := f.s.conversationQueue.ClaimNextConversation(context.Background(), "exec-successor", 1, db.ClaimPlacement{}, time.Minute); err != nil || next != nil {
		t.Errorf("claim after the park = (%+v, %v), want nothing until someone sends a message", next, err)
	}
}

// TestUpstream_AMessageRetriesAtOnce: a person writing to a conversation that
// is waiting out its provider means "try now". The follow-up clears the wait
// in the transaction that records the message.
func TestUpstream_AMessageRetriesAtOnce(t *testing.T) {
	f := newUpstreamFixture(t, "r-upstream-message")
	if disp := f.record(t, 0); !disp.handedBack {
		t.Fatalf("disposition = %+v, want handed back", disp)
	}
	conv, err := f.s.conversations.GetSystem(context.Background(), runmode.LocalDefaultOrgID, f.conversationID)
	if err != nil || conv == nil || conv.Status != domain.StatusQueued || conv.NextAttemptAt == nil {
		t.Fatalf("conversation before the message = (%+v, %v), want queued with a wait", conv, err)
	}

	if err := f.s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, f.conversationID, runmode.LocalDefaultUserID, "the provider is back, try again"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if _, ok := f.nextAttemptIn(t); ok {
		t.Error("the message left the wait in place")
	}
	if got := pendingRows(t, f.s, f.conversationID); len(got) != 1 || got[0].Content != "the provider is back, try again" {
		t.Errorf("pending input = %+v, want the message queued for the next claim", got)
	}
	next, err := f.s.conversationQueue.ClaimNextConversation(context.Background(), "exec-successor", 1, db.ClaimPlacement{}, time.Minute)
	if err != nil || next == nil || next.ID != f.conversationID {
		t.Errorf("claim after the message = (%+v, %v), want conversation %s at once", next, err, f.conversationID)
	}
}
