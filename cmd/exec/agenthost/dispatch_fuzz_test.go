package agenthost

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fuzzRelayConn stands in for the orchestrator on the other end of a sidecar's
// supervision channel. It gives the fuzz fixture a production-shaped runtime
// (relay-backed, no db.Stores, no DB connection) whose effects resolve
// instantly instead of standing up a database inside the fuzz loop.
//
// The two repo-gate reads answer permissively so the gh verbs get PAST the gate
// and actually build a client — the point is to reach the fan-out, and a gate
// that denied everything would fuzz one error path forty times. Every other op
// fails, which is a shape the verbs must handle anyway (an orchestrator that
// went away mid-run).
type fuzzRelayConn struct{}

func (fuzzRelayConn) call(_ context.Context, namespace, op string, _, out any) error {
	switch op {
	case opTeamTracksRepo:
		return fuzzRelayResult(out, teamTracksRepoResult{Tracks: true})
	case opListConversationWorktrees:
		return fuzzRelayResult(out, conversationWorktreesResult{Worktrees: []domain.ConversationWorktree{
			{ConversationID: "run-fuzz", RepoID: "o/r", Ref: "refs/heads/topic", Path: "/work/o-r"},
		}})
	}
	return fmt.Errorf("fuzz fixture: relay %s/%s unavailable", namespace, op)
}

func (fuzzRelayConn) notify(string, string, any) {}

// fuzzRelayResult delivers a canned op result through the same marshal/unmarshal
// hop the real relay uses, so the runtime decodes it exactly as it would a wire
// response.
func fuzzRelayResult(out any, v any) error {
	if out == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// fuzzDispatchBudget bounds one request's service time. The dispatch's own
// deadline is callTimeout (30s) for all but two methods, and this fixture makes
// no call that can legitimately reach it — every DB effect fails at the relay
// and every REST call lands on a local test server that answers immediately —
// so anything still running at this point is a hang, which for a fuzz target is
// as much a finding as a panic.
const fuzzDispatchBudget = 45 * time.Second

// FuzzHandleConn fuzzes the surface that can actually terminate the sidecar:
// the exec RPC's frame decode and verb dispatch. Those frames are composed by
// the jailed agent, which makes them the most directly attacker-influenced
// input in the process holding the run's credentials, and dispatch fans out to
// roughly forty handlers that unmarshal and act on agent-supplied arguments.
//
// It drives handleConn rather than serveConn on purpose — that is, deliberately
// outside the panic guard. The guard's job in production is to keep a panic
// from killing the process; its job here is to stay out of the way, so a panic
// reaches the fuzzing harness and is recorded as a finding instead of a log
// line.
//
// Two outcomes, the same contract internal/ghwrite's GraphQL fuzzer asserts: no
// input panics, and no input hangs. Correctness of any individual verb is not
// what this asserts — the table tests do that.
func FuzzHandleConn(f *testing.F) {
	// Every REST call this fixture can make lands here. A small, immediate 404
	// keeps the gh/jira verbs on a real client path (build request, send,
	// classify the failure) without a network or a fixture per verb.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"not found"}`))
	}))
	f.Cleanup(upstream.Close)

	// Multi mode, because that is the only mode this Server shape exists in: the
	// relocated daemon runs in the per-run credential sidecar, behind the repo
	// gate, resolving gh clients through the run's REST proxies.
	runmode.SetForTest(f, runmode.ModeMulti)

	info := ConversationInfo{OrgID: "org-1", UserID: "user-1", ConversationID: "run-fuzz", TeamID: "team-1"}
	srv := NewServerWithRuntime(newRelayRuntime(fuzzRelayConn{}, info, nil), &ProxyCredentials{
		GitHubAPIURL:   upstream.URL,
		GitHubAPIToken: "placeholder",
		JiraAPIURL:     upstream.URL,
		JiraAPIToken:   "placeholder",
	})

	// Seeds cover the shapes an agent's tool call actually produces — one per
	// argument family, so the mutator starts from something that reaches past
	// the unmarshal into each verb rather than from noise.
	seeds := []struct {
		method string
		args   string
	}{
		{methodLookupConversation, `{}`},
		{methodGetTask, `{"task_id":"task-1"}`},
		{methodGetRepo, `{"repo_id":"repo-1"}`},
		{methodInsertConversationWorktree, `{"row":{"repo_id":"repo-1","ref":"refs/heads/topic","path":"/work/x"}}`},
		{methodCreateWorkspaceCheckout, `{"owner":"o","repo":"r","ref":"refs/heads/main","pr":7}`},
		{methodGithubGetPR, `{"owner":"o","repo":"r","number":7,"verbose":true}`},
		{methodGithubAddPendingReviewComment, `{"owner":"o","repo":"r","review_id":"rv-1","path":"a.go","body":"b","line":-1,"start_line":-9}`},
		{methodGithubDownloadArtifact, `{"owner":"o","repo":"r","path":"actions/runs/1/logs","max_bytes":9223372036854775807}`},
		{methodJiraSearchIssues, `{"jql":"project = X","fields":["summary"],"max_results":-1}`},
		{methodJiraUpdateIssue, `{"key":"SKY-1","fields":{"summary":"s"}}`},
		{methodMemoryLoad, `{"source":"github","source_id":"o/r#1","limit":-5}`},
		{methodCallExtension, `{"namespace":"n","method":"m","args":{"a":1}}`},
		{methodUpsertArtifact, `{"artifact":{"provider":"github","kind":"comment","target":"o/r#1"}}`},
		{"", ``},
		{"NotAMethod", `[]`},
	}
	for _, s := range seeds {
		f.Add(s.method, []byte(s.args))
	}

	f.Fuzz(func(t *testing.T, method string, args []byte) {
		// The args a verb sees are always syntactically valid JSON — they arrive
		// inside the request frame, so a malformed body is rejected by the frame
		// decode and never reaches dispatch. Bytes that aren't valid JSON ride as
		// a JSON string instead of being skipped: that is a shape a client can
		// genuinely send, and it keeps every input reaching the fan-out.
		raw := json.RawMessage(args)
		if !json.Valid(args) {
			quoted, err := json.Marshal(string(args))
			if err != nil {
				t.Skip("input is not representable as a JSON string")
			}
			raw = quoted
		}

		srvSide, cliSide := net.Pipe()
		served := make(chan struct{})
		go func() {
			defer close(served)
			defer func() { _ = srvSide.Close() }()
			var seen string
			srv.handleConn(srvSide, &seen)
		}()

		// The pipe is unbuffered, so the client half has to drain the response
		// for the daemon's write to complete; the deadline keeps a wedged peer
		// from becoming this test's hang rather than the daemon's.
		_ = cliSide.SetDeadline(time.Now().Add(fuzzDispatchBudget))
		if err := writeFrame(cliSide, request{Version: ProtocolVersion, Method: method, Args: raw}); err == nil {
			// Either a response or a read failure is fine — the assertion is about
			// the daemon surviving, not about what it answered.
			var resp response
			_ = readFrame(cliSide, &resp)
		}
		_ = cliSide.Close()

		select {
		case <-served:
		case <-time.After(fuzzDispatchBudget):
			t.Fatalf("dispatch of %q did not return within %s — a hang wedges the run's exec socket as surely as a crash", method, fuzzDispatchBudget)
		}
	})
}
