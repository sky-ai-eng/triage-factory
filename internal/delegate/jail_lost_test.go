package delegate

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/wsevent/toast"
)

// deadToolHost is a tool host whose jail dies during the first call it takes,
// the request reaching it and no answer coming back, and that cannot be
// restarted.
type deadToolHost struct{}

func (deadToolHost) Call(string, map[string]any) (agentloop.ToolOutcome, error) {
	return agentloop.ToolOutcome{}, io.EOF
}

func (deadToolHost) Close() error { return nil }

// toasts returns the bodies of the toasts captured, in broadcast order.
func (c *capturedEvents) toasts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.events {
		if p, ok := e.Data.(toast.Payload); ok && e.Type == "toast" {
			out = append(out, p.Body)
		}
	}
	return out
}

// TestJailLost_TheConversationGoesOnInANewCell drives a jail that dies under
// a command, and that the loop cannot restart in place, through the real
// engine and the ending: the conversation is handed back, not failed, a
// successor claims it at once on the jail budget, and the successor's first
// model request carries the dead call answered, so the model reads what
// happened to it.
func TestJailLost_TheConversationGoesOnInANewCell(t *testing.T) {
	f := newStallFixture(t, "r-jail-lost", activityTimings{idle: time.Hour})

	provider := &scriptedGateProvider{turns: []gateTurn{{calls: []domain.ToolCall{bashCall("c1", "make test")}}}}
	result := f.runNative(t, provider, deadToolHost{})
	if result.Kind != agentloop.ResultToolHostLost {
		t.Fatalf("result = %v (err %v), want tool host lost", result.Kind, result.Err)
	}

	disp := f.s.recordNativeResult(f.claimCtx, runmode.LocalDefaultOrgID, f.conversationID, f.task,
		runConfig{orgID: runmode.LocalDefaultOrgID, claimID: f.claimID},
		"", "", "manual", runmode.LocalDefaultUserID, time.Now(), result, nil)
	if !disp.handedBack || disp.fenced {
		t.Fatalf("disposition = %+v, want handed back and unfenced", disp)
	}

	var status, parkReason, summary string
	var waits bool
	if err := f.database.QueryRow(
		`SELECT COALESCE(status, ''), COALESCE(park_reason, ''), COALESCE(result_summary, ''), next_attempt_at IS NOT NULL
		 FROM conversations WHERE id = ?`, f.conversationID,
	).Scan(&status, &parkReason, &summary, &waits); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if status != "" || parkReason != "" {
		t.Errorf("conversation = (%q, %q), want mid-flight with no park — a lost jail is neither a failure nor a stop", status, parkReason)
	}
	if waits {
		t.Error("the hand-back set next_attempt_at; a new jail can start at once")
	}
	if !strings.Contains(summary, "the sandbox stopped unexpectedly") {
		t.Errorf("result_summary = %q, want what ended the jail", summary)
	}
	if got := claimOutcome(t, f, f.claimID); got != db.HandBackJailLost {
		t.Errorf("claim outcome = %q, want %s", got, db.HandBackJailLost)
	}

	next, err := f.s.conversationQueue.ClaimNextConversation(context.Background(), "exec-successor", 1, db.ClaimPlacement{}, time.Minute)
	if err != nil || next == nil || next.ID != f.conversationID {
		t.Fatalf("successor claim = (%+v, %v), want conversation %s at once", next, err, f.conversationID)
	}
	if next.JailLosses != 1 || next.LostEngagements != 0 || next.SetupFailures != 0 || next.UpstreamHandBacks != 0 {
		t.Errorf("successor's budgets = (jail %d, lost %d, setup %d, upstream %d), want (1, 0, 0, 0)",
			next.JailLosses, next.LostEngagements, next.SetupFailures, next.UpstreamHandBacks)
	}

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

	var answer string
	for _, r := range rows {
		if r.Role == "tool" && r.ToolCallID == "c1" {
			answer = r.Content
		}
	}
	if !strings.Contains(answer, "The tool host stopped while this call ran") || !strings.Contains(answer, "picked up again in a new sandbox") {
		raw, _ := json.Marshal(rows)
		t.Errorf("the dead call's answer in the successor's request = %q, want the lost-call notice: %s", answer, raw)
	}
}

// TestJailLost_TheLastLossTheBudgetAllowsParksForAPerson: the loss that
// spends the jail budget does not start another cell. The conversation parks
// sandbox_lost with a note naming what ended the last jail, its blueprint
// keeps running, nothing claims it until someone writes, and the toast names
// the limit a person can raise when the memory ceiling ended the last jail.
func TestJailLost_TheLastLossTheBudgetAllowsParksForAPerson(t *testing.T) {
	f := newStallFixture(t, "r-jail-lost-spent", activityTimings{idle: time.Hour})
	collect := sandboxMemoryMetrics(t)
	hub, captured := capturingHub(t)
	f.s.wsHub = hub

	reason := "the sandbox reached its 4096 MB memory limit and the kernel ended it"
	disp := f.s.recordNativeResult(f.claimCtx, runmode.LocalDefaultOrgID, f.conversationID, f.task,
		runConfig{orgID: runmode.LocalDefaultOrgID, claimID: f.claimID, jailLosses: maxJailLosses - 1},
		"", "", "manual", runmode.LocalDefaultUserID, time.Now(),
		agentloop.Result{Kind: agentloop.ResultToolHostLost, Err: &agentloop.ToolHostLostError{Reason: reason, MemoryLimit: true, Err: io.EOF}}, nil)
	if disp.handedBack || disp.fenced {
		t.Fatalf("disposition = %+v, want a park", disp)
	}

	var status, parkReason, blueprintStatus string
	if err := f.database.QueryRow(
		`SELECT COALESCE(c.status, ''), COALESCE(c.park_reason, ''), b.status
		 FROM conversations c JOIN blueprint_runs b ON b.id = c.blueprint_run_id WHERE c.id = ?`, f.conversationID,
	).Scan(&status, &parkReason, &blueprintStatus); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if status != domain.StatusOpen || parkReason != string(domain.ParkReasonSandboxLost) {
		t.Errorf("conversation = (%q, %q), want (open, sandbox_lost)", status, parkReason)
	}
	if blueprintStatus != string(domain.BlueprintRunStatusRunning) {
		t.Errorf("blueprint_run status = %q, want running", blueprintStatus)
	}

	var notes []string
	for _, m := range allRows(t, f.s, f.conversationID) {
		if m.Subtype == domain.MessageSubtypeStopNote {
			notes = append(notes, m.Content)
		}
	}
	if want := jailLostNote(maxJailLosses, reason); len(notes) != 1 || notes[0] != want {
		t.Errorf("stop notes = %q, want one: %q", notes, want)
	}
	if got := captured.toasts(); len(got) != 1 || !strings.Contains(got[0], "lost 3 times in a row") || !strings.Contains(got[0], "TF_CLAIM_MEMORY_LIMIT_MB") {
		t.Errorf("toasts = %q, want one saying how many sandboxes were lost and naming TF_CLAIM_MEMORY_LIMIT_MB", got)
	}
	if got := counterAt(t, collect(), "conversations.sandbox_lost_parks",
		attribute.String("memory_limit", "true"), attribute.String("org.id", runmode.LocalDefaultOrgID)); got != 1 {
		t.Errorf("sandbox-lost parks at the memory limit = %d, want 1", got)
	}

	if next, err := f.s.conversationQueue.ClaimNextConversation(context.Background(), "exec-successor", 1, db.ClaimPlacement{}, time.Minute); err != nil || next != nil {
		t.Errorf("claim after the park = (%+v, %v), want nothing until someone sends a message", next, err)
	}
}

// A park that lands nothing is not counted as one. The fence held, so the
// engagement is not fenced, but the row had settled under it and the park
// flipped nothing: the conversation is not paused, and an operator alert
// saying it is would send someone after a run that is not waiting.
func TestJailLost_AParkThatLandsNothingIsNotCounted(t *testing.T) {
	f := newStallFixture(t, "r-jail-lost-settled", activityTimings{idle: time.Hour})
	collect := sandboxMemoryMetrics(t)
	if _, err := f.database.Exec(`UPDATE conversations SET status = ? WHERE id = ?`, domain.StatusFailed, f.conversationID); err != nil {
		t.Fatalf("settle the conversation: %v", err)
	}

	disp := f.s.recordNativeResult(f.claimCtx, runmode.LocalDefaultOrgID, f.conversationID, f.task,
		runConfig{orgID: runmode.LocalDefaultOrgID, claimID: f.claimID, jailLosses: maxJailLosses - 1},
		"", "", "manual", runmode.LocalDefaultUserID, time.Now(),
		agentloop.Result{Kind: agentloop.ResultToolHostLost, Err: &agentloop.ToolHostLostError{
			Reason: "the sandbox reached its 4096 MB memory limit and the kernel ended it", MemoryLimit: true, Err: io.EOF,
		}}, nil)
	if disp.fenced {
		t.Fatalf("disposition = %+v, want unfenced: the claim was still this engagement's", disp)
	}

	var status string
	if err := f.database.QueryRow(`SELECT COALESCE(status, '') FROM conversations WHERE id = ?`, f.conversationID).Scan(&status); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if status != domain.StatusFailed {
		t.Fatalf("status = %q, want the settled row left as it was", status)
	}
	if got := counterAt(t, collect(), "conversations.sandbox_lost_parks",
		attribute.String("memory_limit", "true"), attribute.String("org.id", runmode.LocalDefaultOrgID)); got != 0 {
		t.Errorf("sandbox-lost parks = %d, want 0 for a park that flipped nothing", got)
	}
}
