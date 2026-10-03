package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// nextAttemptIn reads the conversation's next_attempt_at as an offset from
// database now, and false when none is set.
func (f *launchFixture) nextAttemptIn(t *testing.T) (time.Duration, bool) {
	t.Helper()
	return upstreamFixture{stallFixture: stallFixture{database: f.database, conversationID: f.conv.ID}}.nextAttemptIn(t)
}

// endTheWait backdates next_attempt_at so the claim scan sees the wait over.
func (f *launchFixture) endTheWait(t *testing.T) {
	t.Helper()
	if _, err := f.database.Exec(`UPDATE conversations SET next_attempt_at = strftime('%Y-%m-%d %H:%M:%f','now','-1.000 seconds') WHERE id = ?`, f.conv.ID); err != nil {
		t.Fatalf("end the wait: %v", err)
	}
}

func (f *launchFixture) reclaim(t *testing.T) *domain.Conversation {
	t.Helper()
	next, err := f.stores.ConversationQueue.ClaimNextConversation(context.Background(), "lf-exec", 1, db.ClaimPlacement{}, db.DefaultClaimLease)
	if err != nil {
		t.Fatalf("ClaimNextConversation: %v", err)
	}
	return next
}

func (f *launchFixture) sdkPark() liveParkContext {
	return liveParkContext{
		orgID:          runmode.LocalDefaultOrgID,
		conversationID: f.conv.ID,
		claimID:        f.conv.ClaimID,
		runtime:        domain.ConversationRuntimeSDK,
	}
}

// overloaded is the result the SDK reports when the provider answered its
// last request 529 after the SDK's own retries.
func overloaded() *agentproc.Result {
	return &agentproc.Result{
		IsError:        true,
		Subtype:        "success",
		APIErrorStatus: 529,
		CostUSD:        0.02,
		Result:         `API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
	}
}

func TestSDKProviderUnavailable_ClassifiesByStatus(t *testing.T) {
	for status, want := range map[int]bool{
		529: true, 503: true, 500: true, 429: true, 408: true, 409: true,
		401: false, 403: false, 400: false, 404: false, 0: false,
	} {
		r := &agentproc.Result{IsError: true, APIErrorStatus: status}
		if got := sdkProviderUnavailable(r); got != want {
			t.Errorf("sdkProviderUnavailable(IsError, %d) = %v, want %v", status, got, want)
		}
	}
	if sdkProviderUnavailable(&agentproc.Result{APIErrorStatus: 529}) {
		t.Error("a result that is not an error reads as a provider outage")
	}
	if sdkProviderUnavailable(nil) {
		t.Error("no result reads as a provider outage")
	}
}

// TestSDKUpstream_An529HandsBackAndTheNextClaimSaysWhy: an SDK result the
// provider ended with 529 is handed back 'requeued_upstream' on the schedule
// rather than failed, and the claim after the wait carries the outcome that
// makes the resumed session's first message the upstream note.
func TestSDKUpstream_An529HandsBackAndTheNextClaimSaysWhy(t *testing.T) {
	counted := handBackCounter(t)
	f := newLaunchFixture(t, "sdk-529")
	f.open(t)

	disp, ok := f.s.leaveSDKOnUpstream(context.Background(), f.sdkPark(), overloaded(), "sess-529", 0, runmode.LocalDefaultUserID)
	if !ok || !disp.handedBack || disp.fenced {
		t.Fatalf("leaveSDKOnUpstream = (%+v, %v), want handed back and unfenced", disp, ok)
	}
	if st := f.storedStatus(t); st != "" {
		t.Errorf("stored status = %q, want none — the conversation is mid-flight, not failed", st)
	}
	if got := f.claimOutcomes(t); len(got) != 1 || got[0] != db.HandBackUpstream {
		t.Errorf("claim outcomes = %v, want [%s]", got, db.HandBackUpstream)
	}
	if in, ok := f.nextAttemptIn(t); !ok || in > 30*time.Second || in < 20*time.Second {
		t.Errorf("next_attempt_at = now + %v (set %v), want about now + 30s", in, ok)
	}
	var summary string
	if err := f.database.QueryRow(`SELECT COALESCE(result_summary, '') FROM conversations WHERE id = ?`, f.conv.ID).Scan(&summary); err != nil {
		t.Fatalf("read result_summary: %v", err)
	}
	if summary != "model provider unavailable (HTTP 529)" {
		t.Errorf("result_summary = %q, want the status and not the provider's body", summary)
	}
	if st := f.blueprintStatus(t); st != string(domain.BlueprintRunStatusRunning) {
		t.Errorf("blueprint_run status = %q, want running", st)
	}
	if got := counted(db.HandBackUpstream, runmode.LocalDefaultOrgID); got != 1 {
		t.Errorf("conversations.handed_back{requeued_upstream} = %d, want 1", got)
	}

	if next := f.reclaim(t); next != nil {
		t.Fatalf("claimed %s during the wait; want nothing claimable", next.ID)
	}
	f.endTheWait(t)
	next := f.reclaim(t)
	if next == nil || next.ID != f.conv.ID {
		t.Fatalf("claim after the wait = %+v, want conversation %s", next, f.conv.ID)
	}
	if next.LastHandBackOutcome != db.HandBackUpstream || next.UpstreamHandBacks != 1 || next.SetupFailures != 0 {
		t.Errorf("successor = (last %q, upstream %d, setup %d), want (%s, 1, 0)",
			next.LastHandBackOutcome, next.UpstreamHandBacks, next.SetupFailures, db.HandBackUpstream)
	}

	sink := newConversationSink(f.s, runmode.LocalDefaultOrgID, f.conv.ID, next.ClaimID, "manual", runmode.LocalDefaultUserID)
	resumed := agentproc.RunOptions{SessionID: "sess-529"}
	if err := f.s.composeLaunchTurn(context.Background(), &resumed, sink, runmode.LocalDefaultOrgID, f.conv.ID,
		runmode.LocalDefaultUserID, next.LastHandBackOutcome, nil, ""); err != nil {
		t.Fatalf("composeLaunchTurn: %v", err)
	}
	if resumed.Message != domain.UpstreamContinuationNote {
		t.Errorf("the resumed session's first message = %q, want the upstream note", resumed.Message)
	}
	var sent int
	for _, m := range f.transcript(t) {
		if m.Content == domain.SessionContinuationNote {
			t.Error("the transcript records the crash note; the process did not end unexpectedly")
		}
		if m.Content == domain.UpstreamContinuationNote {
			sent++
		}
	}
	if sent != 1 {
		t.Errorf("upstream notes on the transcript = %d, want 1", sent)
	}
}

// TestSDKUpstream_ARefusalOrAnUnreportedStatusStillFails: a 401 is the
// provider refusing the credential, and a result with no status is the agent's
// own error. Neither is handed back, and both fail the conversation as before.
func TestSDKUpstream_ARefusalOrAnUnreportedStatusStillFails(t *testing.T) {
	for _, status := range []int{401, 0} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			f := newLaunchFixture(t, fmt.Sprintf("sdk-fail-%d", status))
			result := &agentproc.Result{IsError: true, APIErrorStatus: status, Result: "API Error: authentication_error"}

			if disp, ok := f.s.leaveSDKOnUpstream(context.Background(), f.sdkPark(), result, "sess-x", 0, runmode.LocalDefaultUserID); ok {
				t.Fatalf("leaveSDKOnUpstream took a %d: %+v", status, disp)
			}
			if got := f.claimOutcomes(t); len(got) != 1 || got[0] != "" {
				t.Fatalf("claim outcomes = %v, want the claim untouched by the declined hand-back", got)
			}
			task, err := f.stores.Tasks.GetSystem(context.Background(), runmode.LocalDefaultOrgID, f.conv.TaskID)
			if err != nil || task == nil {
				t.Fatalf("load task: (%v, %v)", task, err)
			}
			f.s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, f.conv.ID, f.br.ID, f.conv.ClaimID, *task,
				result, t.TempDir(), nil, "sess-x", "manual", runmode.LocalDefaultUserID)
			if st := f.storedStatus(t); st != "failed" {
				t.Errorf("stored status = %q, want failed", st)
			}
			if _, ok := f.nextAttemptIn(t); ok {
				t.Error("a failed conversation carries next_attempt_at")
			}
		})
	}
}

// gitFailure is a git command that failed with output, wrapped the way the
// setup path wraps it on its way to handlePreAgentFailure.
func gitFailure(output string) error {
	return fmt.Errorf("failed to create worktree: %w", &worktree.GitError{
		Args:   []string{"clone", "--bare", "--filter=blob:none", "https://github.com/o/r", "/state/repos/o/r"},
		Output: output,
		Err:    errors.New("exit status 128"),
	})
}

func TestUpstreamSetupFailure_ReadsTheCause(t *testing.T) {
	deadline := fmt.Errorf("failed to create worktree: %w", &worktree.GitError{
		Args: []string{"clone"}, Output: "Cloning into bare repository '/state/repos/o/r'...\n", Err: context.DeadlineExceeded,
	})
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"git: unresolvable host":    {gitFailure("fatal: unable to access 'https://github.com/o/r/': Could not resolve host: github.com\n"), true},
		"git: proxy 502":            {gitFailure("fatal: unable to access 'http://127.0.0.1:41000/o/r/': The requested URL returned error: 502\n"), true},
		"git: request timeout":      {gitFailure("fatal: unable to access 'https://github.com/o/r/': The requested URL returned error: 408\n"), true},
		"git: repository not found": {gitFailure("remote: Repository not found.\nfatal: repository 'https://github.com/o/r/' not found\n"), false},
		"git: refused credential":   {gitFailure("fatal: unable to access 'https://github.com/o/r/': The requested URL returned error: 403\n"), false},
		"git: its deadline":         {deadline, false},
		"github: transport": {fmt.Errorf("failed to fetch PR: %w", &url.Error{
			Op: "Get", URL: "https://api.github.com/repos/o/r/pulls/7", Err: errors.New("dial tcp: lookup api.github.com: no such host"),
		}), true},
		"github: 502":          {fmt.Errorf("failed to fetch PR: %w", ghclient.NewHTTPError(502, "<html>bad gateway</html>", "github: 502")), true},
		"github: rate limited": {fmt.Errorf("failed to fetch PR: %w", &ghclient.ErrRateLimited{ResumeAt: time.Now().Add(time.Minute)}), true},
		"github: 401":          {fmt.Errorf("failed to fetch PR: %w", ghclient.NewHTTPError(401, `{"message":"Bad credentials"}`, "github: 401")), false},
		"github: 404":          {fmt.Errorf("failed to fetch PR: %w", ghclient.NewHTTPError(404, `{"message":"Not Found"}`, "github: 404")), false},
		"unclassified text":    {errors.New("ensure workspace before resume failed: snapshot fetch: connection reset"), false},
		"jail":                 {errors.New("connect to tool host: runsc: exit status 128"), false},
	} {
		if got := upstreamSetupFailure(tc.err); got != tc.want {
			t.Errorf("%s: upstreamSetupFailure = %v, want %v", name, got, tc.want)
		}
	}
}

func TestUpstreamSetupReason_NamesWhatWasUnreachable(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"git":          {gitFailure("fatal: unable to access 'http://127.0.0.1:41000/o/r/': The requested URL returned error: 502\n"), "the repository's git host"},
		"github: 502":  {fmt.Errorf("failed to fetch PR: %w", ghclient.NewHTTPError(502, "<html>bad gateway</html>", "github: 502")), "a service it needs"},
		"github: dial": {fmt.Errorf("failed to fetch PR: %w", &url.Error{Op: "Get", URL: "https://api.github.com/repos/o/r/pulls/7", Err: errors.New("connection refused")}), "a service it needs"},
	} {
		got := upstreamSetupReason(tc.err)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: upstreamSetupReason = %q, want it to name %q", name, got, tc.want)
		}
		assertQuotesNoError(t, name, got)
	}
}

// assertQuotesNoError fails when text a person reads carries the error an
// upstream setup failure ended on, rather than naming what was unreachable.
func assertQuotesNoError(t *testing.T, what, text string) {
	t.Helper()
	for _, leak := range []string{"127.0.0.1", "remote:", "fatal:", "exit status", "failed to fetch PR", "bad gateway", "api.github.com"} {
		if strings.Contains(text, leak) {
			t.Errorf("%s %q quotes the error (%q); it should name what was unreachable", what, text, leak)
		}
	}
}

// TestUpstreamSetup_AnUnreachableHostHandsBackOnTheSchedule: a clone that
// failed because GitHub could not be reached spends the upstream budget, on
// its schedule, and leaves the setup budget alone.
func TestUpstreamSetup_AnUnreachableHostHandsBackOnTheSchedule(t *testing.T) {
	counted := handBackCounter(t)
	f := newLaunchFixture(t, "setup-dns")

	survived := f.s.handlePreAgentFailure(runmode.LocalDefaultOrgID, f.br, f.conv,
		gitFailure("Cloning into bare repository '/state/repos/o/r'...\nfatal: unable to access 'https://github.com/o/r/': Could not resolve host: github.com\n"))
	if !survived {
		t.Fatal("handlePreAgentFailure reported the step over")
	}
	if st := f.storedStatus(t); st != "" {
		t.Errorf("stored status = %q, want none — mid-flight", st)
	}
	if got := f.claimOutcomes(t); len(got) != 1 || got[0] != db.HandBackUpstream {
		t.Errorf("claim outcomes = %v, want [%s]", got, db.HandBackUpstream)
	}
	if in, ok := f.nextAttemptIn(t); !ok || in > 30*time.Second || in < 20*time.Second {
		t.Errorf("next_attempt_at = now + %v (set %v), want about now + 30s", in, ok)
	}
	if st := f.blueprintStatus(t); st != string(domain.BlueprintRunStatusRunning) {
		t.Errorf("blueprint_run status = %q, want running", st)
	}
	if got := counted(db.HandBackUpstream, runmode.LocalDefaultOrgID); got != 1 {
		t.Errorf("conversations.handed_back{requeued_upstream} = %d, want 1", got)
	}

	if next := f.reclaim(t); next != nil {
		t.Fatalf("claimed %s during the wait; want nothing claimable", next.ID)
	}
	f.endTheWait(t)
	next := f.reclaim(t)
	if next == nil || next.ID != f.conv.ID {
		t.Fatalf("claim after the wait = %+v, want conversation %s", next, f.conv.ID)
	}
	if next.SetupFailures != 0 || next.UpstreamHandBacks != 1 {
		t.Errorf("successor's budgets = (setup %d, upstream %d), want (0, 1) — the setup budget is unchanged", next.SetupFailures, next.UpstreamHandBacks)
	}
}

// TestUpstreamSetup_ARefusalIsStillASetupFailure: a repository that is not
// there says the same thing on every attempt, so it keeps today's 'requeued'
// hand-back, claimable at once and counted against the setup budget.
func TestUpstreamSetup_ARefusalIsStillASetupFailure(t *testing.T) {
	f := newLaunchFixture(t, "setup-notfound")

	f.s.handlePreAgentFailure(runmode.LocalDefaultOrgID, f.br, f.conv,
		gitFailure("remote: Repository not found.\nfatal: repository 'https://github.com/o/r/' not found\n"))
	if got := f.claimOutcomes(t); len(got) != 1 || got[0] != string(db.RequeueSetupFailure) {
		t.Errorf("claim outcomes = %v, want [requeued]", got)
	}
	if in, ok := f.nextAttemptIn(t); ok {
		t.Errorf("next_attempt_at = now + %v, want none — a setup failure retries at once", in)
	}
	next := f.reclaim(t)
	if next == nil || next.ID != f.conv.ID || next.SetupFailures != 1 || next.UpstreamHandBacks != 0 {
		t.Errorf("re-claim = %+v, want the conversation with (setup 1, upstream 0)", next)
	}
}

// TestUpstreamSetup_ASpentBudgetParks: the upstream failure that would be the
// budget's last hand-back parks the conversation upstream_unavailable through
// the launch_failed park's write, on a first engagement too. The blueprint
// keeps running, and a message retries it.
func TestUpstreamSetup_ASpentBudgetParks(t *testing.T) {
	f := newLaunchFixture(t, "setup-spent")
	if _, err := f.database.Exec(`UPDATE conversations SET runtime = 'native' WHERE id = ?`, f.conv.ID); err != nil {
		t.Fatalf("mark native: %v", err)
	}
	spent := f.conv
	spent.Runtime = domain.ConversationRuntimeNative
	spent.UpstreamHandBacks = maxUpstreamHandBacks - 1

	survived := f.s.handlePreAgentFailure(runmode.LocalDefaultOrgID, f.br, spent,
		gitFailure("remote: Internal Server Error\nfatal: unable to access 'http://127.0.0.1:41000/o/r/': Failed to connect to 127.0.0.1 port 41000 after 0 ms: Connection refused\n"))
	if !survived {
		t.Fatal("handlePreAgentFailure reported the step over")
	}
	var status, parkReason string
	if err := f.database.QueryRow(
		`SELECT COALESCE(status, ''), COALESCE(park_reason, '') FROM conversations WHERE id = ?`, f.conv.ID,
	).Scan(&status, &parkReason); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if status != domain.StatusOpen || parkReason != string(domain.ParkReasonUpstreamUnavailable) {
		t.Errorf("conversation = (%q, %q), want (open, upstream_unavailable)", status, parkReason)
	}
	if st := f.blueprintStatus(t); st != string(domain.BlueprintRunStatusRunning) {
		t.Errorf("blueprint_run status = %q, want running", st)
	}
	if got := f.claimOutcomes(t); len(got) != 1 || got[0] == db.HandBackUpstream {
		t.Errorf("claim outcomes = %v, want the park's, not another hand-back", got)
	}
	var note string
	for _, m := range f.transcript(t) {
		if m.Subtype == domain.MessageSubtypeStopNote {
			note = m.Content
		}
	}
	for _, want := range []string{"the repository's git host", "about four hours", "Send a message to try again"} {
		if !strings.Contains(note, want) {
			t.Errorf("stop note %q does not mention %q", note, want)
		}
	}
	assertQuotesNoError(t, "stop note", note)
	if next := f.reclaim(t); next != nil {
		t.Errorf("claimed %s after the park; want nothing until someone sends a message", next.ID)
	}
}

// TestUpstreamSetup_ASpentBudgetOnAnSDKStepWithNoSession_FailsAsBefore: an SDK
// conversation with no session cannot be woken by a message, so a park would
// strand it. It takes the setup budget's exhausted disposition, which fails a
// step that never ran.
func TestUpstreamSetup_ASpentBudgetOnAnSDKStepWithNoSession_FailsAsBefore(t *testing.T) {
	f := newLaunchFixture(t, "setup-spent-sdk")
	spent := f.conv
	spent.UpstreamHandBacks = maxUpstreamHandBacks - 1

	if survived := f.s.handlePreAgentFailure(runmode.LocalDefaultOrgID, f.br, spent,
		gitFailure("fatal: unable to access 'http://127.0.0.1:41000/o/r/': Could not resolve host: github.com\n")); survived {
		t.Fatal("the step survived; with no session to resume nothing could wake a park")
	}
	if st := f.blueprintStatus(t); st != string(domain.BlueprintRunStatusFailed) {
		t.Errorf("blueprint_run status = %q, want failed", st)
	}
	var abortReason string
	if err := f.database.QueryRow(`SELECT COALESCE(abort_reason, '') FROM blueprint_runs WHERE id = ?`, f.br.ID).Scan(&abortReason); err != nil {
		t.Fatalf("read abort reason: %v", err)
	}
	if !strings.Contains(abortReason, "the repository's git host") {
		t.Errorf("abort reason = %q, want it to name the repository's git host", abortReason)
	}
	assertQuotesNoError(t, "abort reason", abortReason)
}

// TestUpstreamSetup_ASpentBudgetOnAnSDKConversationWithWorkButNoSession_Parks:
// the same SDK conversation with a transcript parks launch_failed, as the
// setup budget's exhausted disposition does. Its note names what was
// unreachable and offers no message, which the follow-up path would refuse
// with no session to resume.
func TestUpstreamSetup_ASpentBudgetOnAnSDKConversationWithWorkButNoSession_Parks(t *testing.T) {
	f := newLaunchFixture(t, "setup-spent-sdk-work")
	f.speak(t, "assistant", "on it")
	spent := f.conv
	spent.UpstreamHandBacks = maxUpstreamHandBacks - 1

	if survived := f.s.handlePreAgentFailure(runmode.LocalDefaultOrgID, f.br, spent,
		gitFailure("remote: Internal Server Error\nfatal: unable to access 'http://127.0.0.1:41000/o/r/': The requested URL returned error: 502\n")); !survived {
		t.Fatal("handlePreAgentFailure reported the step over; a conversation with work parks")
	}
	var status, parkReason string
	if err := f.database.QueryRow(
		`SELECT COALESCE(status, ''), COALESCE(park_reason, '') FROM conversations WHERE id = ?`, f.conv.ID,
	).Scan(&status, &parkReason); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if status != domain.StatusOpen || parkReason != string(domain.ParkReasonLaunchFailed) {
		t.Errorf("conversation = (%q, %q), want (open, launch_failed)", status, parkReason)
	}
	var note string
	for _, m := range f.transcript(t) {
		if m.Subtype == domain.MessageSubtypeStopNote {
			note = m.Content
		}
	}
	if !strings.Contains(note, "the repository's git host") || !strings.Contains(note, "about four hours") {
		t.Errorf("stop note = %q, want it to name the repository's git host and the four hours", note)
	}
	if strings.Contains(note, "Send a message") {
		t.Errorf("stop note %q offers a message the follow-up path refuses", note)
	}
	assertQuotesNoError(t, "stop note", note)
}

// TestDispatch_GitHubUnreachableDuringSetupRetriesLaterAndProceedsOnceItIsBack
// is the local-mode outage through the real dispatcher: GitHub answers the
// pull-request fetch with a proxy's 502 page, and the engagement leaves the
// conversation queued with a retry time instead of spending its setup budget
// or failing the step. Once GitHub answers again, the claim after the wait
// fetches the pull request and goes on to the clone. The clone points at a
// repository that is not there, so the second engagement ends as an ordinary
// setup failure, which is what shows it got past the outage.
func TestDispatch_GitHubUnreachableDuringSetupRetriesLaterAndProceedsOnceItIsBack(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	ghclient.SetTransientBackoffForTest(t, time.Millisecond)
	fx := newLaunchFixtureWithWorktree(t, "1045", "")

	var down atomic.Bool
	down.Store(true)
	missing := "file://" + filepath.Join(t.TempDir(), "no-such-repo.git")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "<html><body><h1>502 Bad Gateway</h1></body></html>")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/pulls/1045") {
			_, _ = io.WriteString(w, "[]")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 1045, "state": "open", "title": "t",
			"head": map[string]any{"ref": "feature", "sha": "abc", "repo": map[string]any{"clone_url": missing}},
			"base": map[string]any{"ref": "main", "sha": "def", "repo": map[string]any{"clone_url": missing}},
		})
	}))
	t.Cleanup(server.Close)
	fx.s.SetRunCredentialResolvers(bringUpResolver{client: ghclient.NewClient(server.URL, "test-token")}, nil, nil)

	first := fx.conv
	first.OrgID = runmode.LocalDefaultOrgID
	fx.s.dispatchClaimedConversation(context.Background(), &first, time.Now())

	if got := fx.claimOutcomes(t); len(got) != 1 || got[0] != db.HandBackUpstream {
		t.Fatalf("claim outcomes = %v, want [%s]", got, db.HandBackUpstream)
	}
	conv, err := fx.stores.Conversations.GetSystem(context.Background(), runmode.LocalDefaultOrgID, fx.conv.ID)
	if err != nil || conv == nil {
		t.Fatalf("GetSystem: (%v, %v)", conv, err)
	}
	if conv.Status != domain.StatusQueued || conv.NextAttemptAt == nil || !conv.NextAttemptAt.After(time.Now()) {
		t.Errorf("conversation = (status %q, next attempt %v), want queued with a retry time ahead — what the run view renders as \"Retrying at\"", conv.Status, conv.NextAttemptAt)
	}
	if strings.Contains(conv.ResultSummary, "<html") {
		t.Errorf("result_summary carries the proxy's page: %q", conv.ResultSummary)
	}
	if st := fx.blueprintStatus(t); st != string(domain.BlueprintRunStatusRunning) {
		t.Errorf("blueprint_run status = %q, want running", st)
	}

	down.Store(false)
	fx.endTheWait(t)
	second := fx.reclaim(t)
	if second == nil || second.ID != fx.conv.ID {
		t.Fatalf("claim after the wait = %+v, want conversation %s", second, fx.conv.ID)
	}
	if second.UpstreamHandBacks != 1 || second.SetupFailures != 0 {
		t.Errorf("successor's budgets = (upstream %d, setup %d), want (1, 0)", second.UpstreamHandBacks, second.SetupFailures)
	}
	second.OrgID = runmode.LocalDefaultOrgID
	fx.s.dispatchClaimedConversation(context.Background(), second, time.Now())

	if got := fx.claimOutcomes(t); len(got) != 2 || got[1] != string(db.RequeueSetupFailure) {
		t.Errorf("claim outcomes = %v, want [%s requeued] — past the fetch, failing at the clone", got, db.HandBackUpstream)
	}
	if _, ok := fx.nextAttemptIn(t); ok {
		t.Error("a setup failure after the outage left a wait")
	}
}
