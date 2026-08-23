package agentproc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// The control protocol bridging Go and wrapper.mjs in streaming-input
// mode. Both sides must match exactly.
//
// stdin (Go → wrapper), newline-delimited JSON, one object per line:
//
//	{"kind":"user_message","text":"..."}
//	{"kind":"interrupt"}
//	{"kind":"set_mode","mode":"default|acceptEdits|plan|bypassPermissions|dontAsk|auto"}
//	{"kind":"permission_response","tool_call_id":"<id>","behavior":"allow"|"deny","message":"...","updated_input":{...}}
//	{"kind":"end"}
//
// On an allow, updated_input optionally overrides the tool input; when
// omitted the wrapper echoes the original input back to the SDK (its CLI
// requires updatedInput on every allow result, so "absent" cannot mean
// "send nothing").
//
// stdout (wrapper → Go): the existing SDK envelopes (system/assistant/
// user/result) the StreamState parser already consumes, PLUS control
// lines this reader intercepts:
//
//	{"type":"control","subtype":"ready"}
//	{"type":"control","subtype":"permission_request","tool_call_id":"<id>","tool_name":"...","input":{...},"title":"...","display_name":"...","description":"..."}
//	{"type":"control","subtype":"interrupted"}

// PermissionRequest is one canUseTool round-trip surfaced from the
// wrapper. The handler decides allow/deny; the reader writes the
// matching permission_response back to the wrapper.
//
// ToolCallID is the SDK's toolUseID for the gated call — the same id that
// appears in the assistant message's tool_calls and on the tool result that
// follows, and the same identity the native loop's gate seam names as
// domain.ToolCall.ID. Title/DisplayName/Description are the prompt copy the
// SDK already rendered ("Claude wants to read foo.txt" / "Read file" / a
// subtitle); all three are optional and empty when the SDK omits them, so a
// consumer must still be able to reconstruct from ToolName + Input.
type PermissionRequest struct {
	ToolCallID  string
	ToolName    string
	Input       map[string]any
	Title       string
	DisplayName string
	Description string
}

// PermissionDecision is the handler's answer. Behavior is "allow" or
// "deny"; Message is the (optional) deny reason; UpdatedInput, when set
// on an allow, replaces the tool input the agent runs with — nil means
// the agent runs with its original input unchanged.
type PermissionDecision struct {
	Behavior     string
	Message      string
	UpdatedInput map[string]any
}

// PermissionHandler is called from the reader goroutine for every
// permission_request. It must not block indefinitely — the agent's turn
// is parked on the answer. A nil handler denies every request.
type PermissionHandler func(PermissionRequest) PermissionDecision

const (
	// closeDrainTimeout bounds the graceful end→drain phase before we
	// escalate to SIGKILL.
	closeDrainTimeout = 5 * time.Second
	// closeKillTimeout bounds the post-SIGKILL wait. The reader goroutine
	// closes done, but it can be wedged in a slow Sink or PermissionHandler
	// (both run on it) where SIGKILL'ing the subprocess won't free it — so
	// Close returns rather than blocking the caller forever.
	closeKillTimeout = 5 * time.Second
)

// LiveRun is a single long-lived streaming-input agent process you can
// send messages to, interrupt, switch permission modes on, and answer
// tool-permission prompts for. Created by RunInteractive; unlike the
// one-shot Run it does not block until the subprocess exits — the reader
// loop runs in a goroutine and the caller steers via the methods below.
//
// Concurrency: the sink is driven from the single reader goroutine
// (preserving the Sink contract). The control-writer methods (Send,
// Interrupt, SetMode, Close) and the reader's permission responses share
// one mutex-guarded stdin writer, so they're safe to call from any
// goroutine.
type LiveRun struct {
	stdin   io.WriteCloser
	writeMu sync.Mutex
	cancel  context.CancelFunc

	// cleanup is the direct-spawn path's teardown hook — a no-op, since the
	// direct path owns nothing beyond the process itself (which cancel's
	// SIGKILL already reclaims). Run exactly once, at the very end of
	// readLoop — after cmd.Wait and the final cancel, never concurrently
	// with the stream read (StdoutPipe forbids Wait before the reader
	// drains; readLoop already orders this). cleanupOnce guards the
	// single-shot guarantee against any double-close.
	cleanup     func()
	cleanupOnce sync.Once

	done      chan struct{} // closed when the reader loop exits
	ready     chan struct{} // closed when the wrapper emits control/ready
	readyOnce sync.Once

	// stream is the reader loop's parser, constructed here rather than
	// inside the loop so Send can mark when a user message goes out. A run
	// parked between turns would otherwise bill the whole wait to the first
	// assistant message of the next one.
	stream *StreamState

	// Close phase bounds; zero falls back to the package consts. Fields
	// (not just consts) so tests can drive the timeout paths quickly.
	drainTimeout time.Duration
	killTimeout  time.Duration

	mu        sync.Mutex
	termErr   error
	result    *Result
	sessionID string
	stderr    string
}

// InteractiveSupported reports whether this host can run a LiveRun.
// Unconditionally true; retained as the seam the delegate layer forks on
// (runLiveAndDrive vs the one-shot runOneShot fallback) so that fallback stays
// reachable if a future host ever needs it.
func InteractiveSupported() bool {
	return true
}

// RunInteractive spawns the agent in streaming-input mode and returns a
// LiveRun the caller steers. The reader loop runs in a background
// goroutine; the call returns as soon as the subprocess is started.
//
// If opts.Message is non-empty it is sent as the first user_message once
// the wrapper signals ready. perms answers tool-permission prompts; a
// nil perms denies every prompt. A nil sink discards stream events.
//
// Local mode only, and enforced: refuses with errSDKLoopInMultiMode before
// spawning anything if runmode is multi (the SDK loop only ever runs local —
// multi mode's delegations are runtime='native', driven through
// agentproc.LaunchToolHost's jail instead) rather than running the SDK
// unsandboxed on the host — see refuseMultiModeSDKLoop. Otherwise always the
// direct subprocess; the streaming-input flags are set on opts before the
// command is built, so BuildArgs carries them.
func RunInteractive(ctx context.Context, opts RunOptions, sink Sink, perms PermissionHandler) (*LiveRun, error) {
	if err := refuseMultiModeSDKLoop(); err != nil {
		return nil, err
	}
	if sink == nil {
		sink = NoopSink{}
	}
	opts.Interactive = true
	// Opt the wrapper into canUseTool ONLY when the caller supplied a
	// handler. A nil handler means "autonomous run": the wrapper omits
	// canUseTool, so the --allowedTools list is the sole gate and
	// off-allowlist tools auto-deny — byte-identical to the headless
	// one-shot path, no per-tool prompts. A non-nil handler emits the
	// flag so the wrapper routes the off-allowlist remainder to perms.
	opts.PermissionPrompts = perms != nil

	// Derived ctx so Close() (and the terminal-error path) can SIGKILL
	// the process group via cmd.Cancel without touching the caller's ctx.
	runCtx, cancel := context.WithCancel(ctx)

	wrapperPath, err := ensureSDKTraced(runCtx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("agent runtime: %w", err)
	}

	nodeArgs := append([]string{wrapperPath}, BuildArgs(opts)...)
	directCmd, derr := newDirectCommand(runCtx, opts, nodeArgs)
	if derr != nil {
		cancel()
		return nil, derr
	}
	proc, perr := newExecProc(directCmd)
	if perr != nil {
		cancel()
		return nil, perr
	}

	// The stdio pipes + stderr capture are set up inside execProc; Stdin/
	// Stdout are valid only after Start.
	if err := proc.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start agent runtime: %w", err)
	}

	l := &LiveRun{
		stdin:   proc.Stdin(),
		cancel:  cancel,
		cleanup: func() {},
		done:    make(chan struct{}),
		ready:   make(chan struct{}),
		stream:  NewStreamState(),
	}

	// opts travels whole (rather than the two fields the loop used to take)
	// because the teardown stamp at the end of the loop needs the run's claim
	// + recorder as well.
	go l.readLoop(runCtx, opts, proc, sink, perms)

	// Send the initial prompt once the wrapper is ready. Done in its own
	// goroutine so RunInteractive returns immediately; Send blocks on the
	// ready signal internally.
	if opts.Message != "" {
		go func() {
			if err := l.Send(ctx, opts.Message); err != nil {
				agentprocLog.Error("initial message send failed", "error", err)
			}
		}()
	}

	return l, nil
}

// Send delivers a user message to the agent. It blocks until the wrapper
// has signaled readiness (or the run finishes / ctx cancels first).
func (l *LiveRun) Send(ctx context.Context, text string) error {
	select {
	case <-l.ready:
	case <-l.done:
		return fmt.Errorf("live run finished before send: %w", l.Err())
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := l.writeControl(map[string]any{"kind": "user_message", "text": text}); err != nil {
		return err
	}
	l.stream.MarkRequest()
	return nil
}

// Interrupt stops the agent's current turn. The wrapper acknowledges
// with a control/interrupted line and the in-flight turn ends with an
// error_during_execution result; the process stays alive for further
// messages.
func (l *LiveRun) Interrupt(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return l.writeControl(map[string]any{"kind": "interrupt"})
}

// SetMode switches the agent's permission mode mid-run (default,
// acceptEdits, plan, bypassPermissions, dontAsk, auto).
func (l *LiveRun) SetMode(ctx context.Context, mode string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return l.writeControl(map[string]any{"kind": "set_mode", "mode": mode})
}

// Close ends the run gracefully: it tells the wrapper to end its input
// iterable so the query drains, waits up to closeDrainTimeout for the
// process to exit, then SIGKILLs the process group on timeout. Returns
// the run's terminal error, if any.
func (l *LiveRun) Close() error {
	// Best-effort graceful end; ignore the write error (the process may
	// already be gone, in which case the drain wait returns immediately).
	_ = l.writeControl(map[string]any{"kind": "end"})

	drain := l.drainTimeout
	if drain <= 0 {
		drain = closeDrainTimeout
	}
	kill := l.killTimeout
	if kill <= 0 {
		kill = closeKillTimeout
	}

	select {
	case <-l.done:
		return l.Err()
	case <-time.After(drain):
	}

	// Graceful drain timed out — SIGKILL the process group, then wait with
	// a second bound. If the reader goroutine is blocked in a Sink or
	// PermissionHandler, SIGKILL won't unblock it, so we return a timeout
	// error instead of hanging the caller forever.
	l.cancel()
	select {
	case <-l.done:
		return l.Err()
	case <-time.After(kill):
		return fmt.Errorf("live run close timed out after SIGKILL; reader goroutine may be blocked in a sink or permission handler")
	}
}

// Done is closed when the run has fully terminated and Err/Result/
// SessionID are final.
func (l *LiveRun) Done() <-chan struct{} { return l.done }

// Err returns the run's terminal error, or nil. Only meaningful after
// Done is closed.
func (l *LiveRun) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.termErr
}

// Result returns the folded terminal Result (cost/duration/turns summed
// across turns), or nil if no result event was seen. Only meaningful
// after Done is closed.
func (l *LiveRun) Result() *Result {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.result
}

// SessionID returns the captured Claude Code session id, or empty if the
// stream never emitted system/init.
func (l *LiveRun) SessionID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sessionID
}

// Stderr returns the subprocess's captured stderr. Only meaningful after
// Done is closed.
func (l *LiveRun) Stderr() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stderr
}

func (l *LiveRun) markReady() {
	l.readyOnce.Do(func() { close(l.ready) })
}

// writeControl marshals one control object and writes it as a single
// NDJSON line to the wrapper's stdin. Serialized by writeMu so the
// reader goroutine's permission responses and the caller's steering
// writes never interleave.
func (l *LiveRun) writeControl(v map[string]any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')

	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	select {
	case <-l.done:
		return fmt.Errorf("live run already finished: %w", l.Err())
	default:
	}
	_, err = l.stdin.Write(b)
	return err
}

// readLoop is the single goroutine that owns the sink and the
// subprocess lifecycle: it consumes the stream until EOF/ctx, waits on
// the process, and records the terminal state.
func (l *LiveRun) readLoop(runCtx context.Context, opts RunOptions, proc runProc, sink Sink, perms PermissionHandler) {
	defer close(l.done)

	stream := l.stream
	result, streamErr := l.consumeStreamInteractive(proc.Stdout(), sink, stream, perms, opts.OnResult, opts.TraceID)

	// If the stream reader bailed before any terminal result, the
	// subprocess may still be running with data to write — kill the
	// process (via ctx cancel) so Wait doesn't block on a stuck pipe.
	if streamErr != nil && result == nil {
		l.cancel()
	}

	waitErr := proc.Wait()

	// Wait has taken the cgroup read, so the engagement's actuals are final
	// here — for every disposition alike, including a cancelled run (whose
	// ctx is already dead; the stamp detaches) and an idle hibernation.
	recordSandboxActuals(runCtx, opts, proc)

	l.mu.Lock()
	l.result = result
	l.sessionID = stream.SessionID()
	l.stderr = proc.Stderr()
	switch {
	case streamErr != nil && result == nil:
		l.termErr = fmt.Errorf("stream: %w", streamErr)
	case waitErr != nil && result == nil:
		if runCtx.Err() != nil {
			l.termErr = runCtx.Err()
		} else if proc.OOMKilled() {
			// OOMKilled is read from the run's Wait result (in-process it is
			// captured before the cgroup is torn down; brokered the broker
			// reports it with the exit), so the attribution is stable here.
			l.termErr = fmt.Errorf("agent runtime killed: %w (%d MB; tune TF_CLAIM_MEMORY_LIMIT_MB): %v",
				ErrClaimMemoryLimit, ClaimMemoryLimitMB(), waitErr)
		} else {
			l.termErr = fmt.Errorf("agent runtime exited with error: %w", waitErr)
		}
	}
	l.mu.Unlock()

	// Release the derived context now the process is gone. Idempotent: on the
	// stream-error path above this already fired (to SIGKILL the group), so
	// here it's a no-op; on the normal path this is the release.
	l.cancel()

	// Tear down the sandbox bring-up (sandbox + proxies + agenthost daemon +
	// scratch dir) now the subprocess has exited and cmd.Wait has returned —
	// after the stream fully drained, never concurrently with the read
	// (StdoutPipe forbids Wait before the reader finishes; the ordering above
	// guarantees it). Once-guarded and a no-op for direct/local runs, so the
	// local path stays byte-identical. Idle hibernation reaches here via
	// Close()'s graceful end → the wrapper exits → cmd.Wait returns, so the
	// subnet slot frees automatically. The pathological Close kill-timeout
	// path (reader wedged in a slow sink/handler) never reaches this line;
	// the startup sandbox.ReapOrphans backstop covers that exactly as it
	// covers a crash.
	l.runCleanup()
}

// runCleanup runs the sandbox teardown exactly once. Safe to call on a
// LiveRun constructed without a cleanup (the direct path and the unit-test
// fixtures): a nil cleanup is a no-op.
func (l *LiveRun) runCleanup() {
	if l.cleanup == nil {
		return
	}
	l.cleanupOnce.Do(l.cleanup)
}

// consumeStreamInteractive scans the wrapper's NDJSON output, driving
// the sink with SDK envelopes and intercepting control lines. Unlike the
// one-shot consumeStream it does NOT return on the first result: it loops
// until EOF / read error, folding every per-turn Result with MergeResult
// so the returned Result reflects the whole conversation.
func (l *LiveRun) consumeStreamInteractive(stdout io.Reader, sink Sink, stream *StreamState, perms PermissionHandler, onResult func(*Result), traceID string) (*Result, error) {
	reader := bufio.NewReader(stdout)

	sessionDelivered := false
	interruptPending := false
	var merged *Result

	for {
		line, readErr := readLine(reader, maxStreamLineBytes)
		if len(line) > 0 {
			if ctl, ok := parseControlLine(line); ok {
				switch ctl.Subtype {
				case "ready":
					l.markReady()
				case "interrupted":
					// The next result closes out the interrupted turn;
					// label it so callers can tell an interrupt apart
					// from a natural end even if the SDK omits the
					// error_during_execution subtype.
					interruptPending = true
				case "permission_request":
					// The handler parks this goroutine for as long as the
					// human takes, so the marks are held still across it —
					// the wait belongs to the approval, not to the tool that
					// runs once it clears.
					gateAt := stream.Now()
					l.handlePermission(ctl, perms)
					stream.DiscountGate(gateAt)
				}
				// Control lines are not sink content.
			} else {
				messages, result := stream.ParseLine(line, traceID)

				if !sessionDelivered {
					if sid := stream.SessionID(); sid != "" {
						// Publish the session id eagerly so callers can
						// read it (resume key, takeover validation) while
						// the run is still live, not just at exit.
						l.mu.Lock()
						l.sessionID = sid
						l.mu.Unlock()
						if err := sink.OnSession(sid); err != nil {
							agentprocLog.Warn("sink on-session failed", "error", err)
						}
						sessionDelivered = true
					}
				}

				for _, msg := range messages {
					if err := sink.OnMessage(msg); err != nil {
						agentprocLog.Warn("sink on-message failed", "error", err)
						continue
					}
				}

				if result != nil {
					if interruptPending {
						// This result closes the turn our interrupt() ended.
						// parseResult already marks Interrupted from the SDK's
						// native terminal_reason; this corroborates it from the
						// wrapper's control/interrupted ack — same goroutine as
						// the result, so the pairing can't desync — covering any
						// path that omits terminal_reason.
						result.Interrupted = true
						if result.Subtype == "" {
							result.Subtype = "error_during_execution"
						}
						interruptPending = false
					}
					if merged == nil {
						merged = result
					} else {
						merged = MergeResult(merged, result)
					}
					// Per-turn terminal signal: a live caller (the delegate
					// driver) reacts to this turn's result without waiting
					// for the whole query to drain. Fired with the per-turn
					// result, not the running merge.
					if onResult != nil {
						onResult(result)
					}
				}
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				return merged, nil
			}
			return merged, readErr
		}
	}
}

// handlePermission answers one permission_request by invoking the
// handler and writing the resulting permission_response back to the
// wrapper. A nil handler denies. Runs on the reader goroutine; the
// handler must not block indefinitely.
func (l *LiveRun) handlePermission(ctl controlLine, perms PermissionHandler) {
	var decision PermissionDecision
	if perms != nil {
		decision = perms(PermissionRequest{
			ToolCallID:  ctl.ToolCallID,
			ToolName:    ctl.ToolName,
			Input:       ctl.Input,
			Title:       ctl.Title,
			DisplayName: ctl.DisplayName,
			Description: ctl.Description,
		})
	} else {
		decision = PermissionDecision{Behavior: "deny", Message: "no permission handler configured"}
	}
	if decision.Behavior == "" {
		decision.Behavior = "deny"
	}

	resp := map[string]any{
		"kind":         "permission_response",
		"tool_call_id": ctl.ToolCallID,
		"behavior":     decision.Behavior,
	}
	if decision.Message != "" {
		resp["message"] = decision.Message
	}
	if decision.UpdatedInput != nil {
		resp["updated_input"] = decision.UpdatedInput
	}
	if err := l.writeControl(resp); err != nil {
		agentprocLog.Error("permission response write failed", "error", err)
	}
}

// controlLine is the decoded shape of a `type:"control"` stdout line.
type controlLine struct {
	Subtype     string
	ToolCallID  string
	ToolName    string
	Input       map[string]any
	Title       string
	DisplayName string
	Description string
}

// parseControlLine reports whether line is a control envelope and, if
// so, returns its decoded fields. Non-control (or malformed) lines
// return ok=false so the caller falls back to the SDK-envelope parser.
func parseControlLine(line []byte) (controlLine, bool) {
	var raw struct {
		Type        string         `json:"type"`
		Subtype     string         `json:"subtype"`
		ToolCallID  string         `json:"tool_call_id"`
		ToolName    string         `json:"tool_name"`
		Input       map[string]any `json:"input"`
		Title       string         `json:"title"`
		DisplayName string         `json:"display_name"`
		Description string         `json:"description"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return controlLine{}, false
	}
	if raw.Type != "control" {
		return controlLine{}, false
	}
	return controlLine{
		Subtype:     raw.Subtype,
		ToolCallID:  raw.ToolCallID,
		ToolName:    raw.ToolName,
		Input:       raw.Input,
		Title:       raw.Title,
		DisplayName: raw.DisplayName,
		Description: raw.Description,
	}, true
}

// syncBuffer is a minimal concurrency-safe bytes buffer for capturing
// subprocess stderr that exec writes from its own goroutine while the
// LiveRun owner may read it via Stderr().
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func newSyncBuffer() *syncBuffer { return &syncBuffer{} }

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
