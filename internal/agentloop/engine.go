package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
)

// Transcript is the loop's view of the messages table. The three methods are
// exactly the native-loop store primitives; nothing else about a
// conversation is readable from inside the loop, which is what makes
// assembly provably a pure function of rows.
type Transcript interface {
	// ListForAssembly returns every row in the assembly window, ordered by
	// COALESCE(seq, id), undelivered rows included and flagged.
	ListForAssembly(ctx context.Context, orgID, conversationID string) ([]domain.Message, error)
	// MarkDelivered flushes pending rows, stamping subtype when non-empty.
	MarkDelivered(ctx context.Context, orgID, conversationID string, ids []int, subtype string) error
	// Insert appends a row and returns its assigned id. The implementation
	// stamps claim attribution server-side and broadcasts the row.
	Insert(ctx context.Context, orgID string, msg *domain.Message) (int, error)
	// Compact commits one compaction atomically: insert replyRow when
	// non-nil (forced inactive — the forced-shape path's reconstructed
	// artifact), insert resultRow, flip inactiveIDs out of the window, and
	// re-seq undelivered rows to fractional positions after the result row,
	// preserving their relative order. Assigned IDs are written back onto
	// the row pointers.
	Compact(ctx context.Context, orgID, conversationID string, replyRow, resultRow *domain.Message, inactiveIDs []int) error
	// SettleCompactionRequest records a discarded warm compaction attempt on
	// the request row: the failed call's usage and cost (its reply is never
	// inserted, but its dollars are real and the ledger is messages alone),
	// and the failure reason for observability.
	SettleCompactionRequest(ctx context.Context, orgID, conversationID string, requestID, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int, costUSD *float64, reason string) error
}

// Provider is the streaming completion surface — inference.Client in
// production, a scripted fake in tests.
type Provider interface {
	Stream(ctx context.Context, req inference.Request) (*inference.Completion, error)
}

// Credentials resolves the provider account for one call. Resolution is
// per-call, not per-engagement: a Bedrock STS triple can expire mid-run, and
// re-resolving is cheap next to the call it precedes.
type Credentials interface {
	// ForCall returns the provider to route to and the client to route
	// through. The returned release is called once the call completes;
	// it may be nil.
	ForCall(ctx context.Context) (provider schemas.ModelProvider, client Provider, release func(), err error)
}

// Guard is one pre-call admission check. A non-empty notice means stop
// before making the call: the engine writes the notice into the transcript,
// parks the conversation `open`, and releases the claim. Guards never fail a
// conversation — a guard trip is a pause, not an error.
type Guard interface {
	// Check reports why the engagement must stop, or "" to proceed. An
	// error is logged and treated as "proceed": a guard that cannot read
	// its own inputs must not wedge every run.
	Check(ctx context.Context, turn int) (notice string, err error)
}

// Hooks are the loop's internal seams, mirroring the loop-config layer of
// the harness this engine replaces. Plain function fields, deliberately not
// a registry: the user-facing extension taxonomy is a later ticket that
// builds on these, and a registry now would fix an interface before there
// is a consumer to shape it.
type Hooks struct {
	// BeforeToolCall gates a call. A non-empty deny message becomes a
	// synthetic is_error result in-band — the model sees the denial and the
	// loop continues, so a gate is a conversation, not a kill.
	BeforeToolCall func(ctx context.Context, call domain.ToolCall) (deny string)

	// AfterToolCall rewrites a result before it is persisted. It receives
	// the outcome as dispatched and returns the outcome to record.
	AfterToolCall func(ctx context.Context, call domain.ToolCall, out ToolOutcome) ToolOutcome

	// ShouldStopAfterTurn runs when the model would conclude (an assistant
	// message with no tool calls). A non-empty nudge is inserted as pending
	// input and the loop continues instead of concluding; "" lets the
	// conclusion stand. This is where the artifact-contract nudge lives.
	//
	// Whether a nudge repeats is the hook's business, not the engine's. The
	// engine deliberately keeps no "already nudged" flag: that would be
	// process state governing behavior, and the transcript already records
	// what was asked and what has happened since. A hook that never returns
	// "" is bounded by the turn backstop and the spend guard like any other
	// work.
	ShouldStopAfterTurn func(ctx context.Context, turn int, finalText string) (nudge string)
}

// Params is one engagement: everything the loop needs that isn't a dependency.
type Params struct {
	OrgID          string
	ConversationID string

	// Model is the provider model id. SystemPrompt is the fully assembled
	// envelope (see envelope.go) — the loop never composes prompt text
	// itself beyond the drain/repair notices.
	Model        string
	SystemPrompt string

	// HasBlueprint registers the flow-control tool. True for a delegation,
	// where the conversation executes a blueprint against a task; false for
	// a conversation with a human in it, which has no blueprint to stop and
	// nobody absent to leave a reason for. It must agree with the composed
	// system prompt, whose completion contract describes that tool: a model
	// must never hold a tool its instructions omit, nor be told about one it
	// was not given.
	HasBlueprint bool

	// MaxIterations bounds provider calls since the conversation's last
	// human input. Zero uses DefaultMaxIterations.
	MaxIterations int

	// MaxTokens and Effort ride through to the provider call unchanged.
	MaxTokens int
	Effort    string

	// MissionAnchored marks a conversation whose opening turn is a
	// control-plane-minted mission (a task/blueprint-backed delegation).
	// Compaction pins that opening — the summary can never mutate the task,
	// and the cacheable prefix survives. Taskless surfaces (the curator, a
	// free-form manual run) pass false: their first message is just the
	// oldest message, and the original request is re-injected mechanically
	// into the result row instead.
	MissionAnchored bool

	// CompactionThreshold is the window fraction that trips proactive
	// compaction. Zero uses DefaultCompactionThreshold.
	CompactionThreshold float64

	// ColdCompactionModel is the forced-shape summarize call's model. Empty
	// uses DefaultColdCompactionModel. The credentials layer must whitelist
	// it alongside the conversation's own model.
	ColdCompactionModel string

	// UserID attributes rows the loop writes on a user's behalf (the
	// repair notice, drained input it re-stamps). Empty for event runs.
	UserID string
}

// ResultKind is how an engagement ended.
type ResultKind int

const (
	// ResultConcluded — the model finished: an assistant message with
	// no tool calls, or a flow-control tool. Outcome carries which.
	ResultConcluded ResultKind = iota
	// ResultParked — a guard tripped. The conversation is `open` with
	// a notice row; the next claim continues it.
	ResultParked
	// ResultFailed — the engagement could not continue (retry
	// exhaustion, a fatal tool-host error). The conversation fails.
	ResultFailed
	// ResultCancelled — the context was cancelled.
	ResultCancelled
)

// Result is the engagement's terminal report, returned to the in-process
// caller that drove the engagement (the delegate layer's recordNativeResult).
// It deliberately lives here and not in domain: the shared vocabulary it
// carries (RunOutcome, RunFailureKind) is already domain's, and the rest is
// the shape of one function's return value.
type Result struct {
	Kind          ResultKind
	Outcome       domain.RunOutcome
	OutcomeReason string
	ResultSummary string
	FailureKind   domain.RunFailureKind
	NumTurns      int
	DurationMs    int
	// ParkNotice is the user-visible reason a guard parked the engagement.
	ParkNotice string
	// Err carries the underlying cause on ResultFailed.
	Err error
}

// DefaultMaxIterations bounds provider calls since the last human input. It
// is a backstop against a cheap-call loop, not a work budget: spend is the
// real brake, so this is set generously enough that a legitimately long task
// never trips it. The count is derived from the transcript, not kept as
// engagement state — a crash or re-claim cannot reset it, and only a human
// message renews it, so a cheap model cannot buy unbounded calls by cycling
// claims.
const DefaultMaxIterations = 400

// wrapUpNotice is injected one call before the turn budget parks the run,
// so the final call produces an account of the work instead of stopping
// mid-loop. Deliberately free of numbers: the text is constant across
// configurations, which is what lets a prior wrap-up be recognized by its
// subtype alone.
const wrapUpNotice = "<system-note>\n" +
	"The next reply is the last model call before this run pauses to wait for user input. " +
	"Do not start new work and do not call tools. Reply with a wrap-up of the run so far: " +
	"what was accomplished, the exact state of the workspace (branch, commits, uncommitted or untracked changes), " +
	"what remains to be done, and the next steps a resumed run should take.\n" +
	"</system-note>"

// limitParkNotice is the user-facing record of a turn-budget park, written
// once per park (deduped against a re-claim re-parking without new input).
func limitParkNotice(maxIter int) string {
	return fmt.Sprintf("This run reached its limit of %d model calls since the last user message and has been paused. "+
		"Nothing is lost — send a message to pick it back up.", maxIter)
}

// turnBudget is the transcript-derived state of the call bound: how many
// assistant turns have happened since the last human input, and whether the
// wrap-up notice has already been requested within this budget window.
type turnBudget struct {
	turns           int
	wrapUpRequested bool
}

// deriveBudget walks the assembly window. Human input resets the budget —
// a user message is renewed license to work — and everything the system
// authored (injections, notices) deliberately does not: an additive event
// landing on a parked conversation gets the park answered again, not a
// fresh block of calls nobody asked for.
func deriveBudget(rows []domain.Message) turnBudget {
	var b turnBudget
	for _, r := range rows {
		// Case order matters: a wrap-up row is a user row that is never
		// human input (isHumanInput excludes it by construction), so the
		// wrapUpRequested case only reaches rows the isHumanInput case above
		// it already declined to reset the budget on.
		switch {
		case r.Role == "assistant":
			b.turns++
		case r.Role != "user":
		case IsHumanInput(r):
			b = turnBudget{}
		case r.Subtype == domain.MessageSubtypeInjectionWrapUp:
			b.wrapUpRequested = true
		}
	}
	return b
}

// Engine drives native conversations. One Engine is shared across
// engagements; per-engagement state lives entirely in Run's frame and in
// the messages table.
type Engine struct {
	Transcript  Transcript
	Credentials Credentials
	Tools       ToolHost
	Guards      []Guard
	Hooks       Hooks

	// Retry tunes the same-provider-same-model retry. The zero value uses
	// the package defaults.
	Retry RetryPolicy

	// Log receives operational messages. Optional.
	Log Logger
}

// Logger is the minimal logging surface the engine needs, satisfied by
// *slog.Logger.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

// Run drives one claimed engagement to a terminal disposition.
//
// The order inside the loop is load-bearing and matches the engine's
// contract: compaction trip (before the drain, so queued input survives a
// compaction as live input) → drain (the only door input enters through) →
// budget (wrap-up one call before the bound) → guards (before every call,
// not every turn) → assemble → credentials → stream → persist → stop-reason
// handling → tool dispatch → would-stop.
func (e *Engine) Run(ctx context.Context, params Params) Result {
	started := time.Now()
	maxIter := params.MaxIterations
	if maxIter <= 0 {
		maxIter = DefaultMaxIterations
	}

	// Setup: make the transcript legal and honest before anything reads it.
	// Unconditional and idempotent — there is no resume branch, so the
	// crash-recovery path is the path that runs every time and cannot rot.
	if err := e.repairTranscript(ctx, params); err != nil {
		return e.failed(started, 0, fmt.Errorf("repair transcript: %w", err))
	}

	// Cold-resume compaction, after repair and before the first drain:
	// repair → compact → drain → call, so the summarize call reads exactly
	// what the previous engagement left and queued input stays queued
	// through it.
	if err := e.compactOnResume(ctx, params); err != nil {
		if ctx.Err() != nil {
			return e.cancelled(started, 0)
		}
		e.insertNotice(ctx, params, "Compacting this conversation on resume failed: "+err.Error())
		return e.failed(started, 0, fmt.Errorf("compact on resume: %w", err))
	}

	// bareDrain is true for the engagement's first drain and for any drain
	// that follows a no-tool-call assistant message. Every other drain
	// happens between turns, while the model is mid-work, and is stamped as
	// a steer so assembly can wrap it in the keep-working envelope.
	bareDrain := true
	turn := 0
	lastText := ""

	// reactiveCompacted licenses at most one overflow-triggered compaction
	// per provider call: a second overflow immediately after compacting
	// means compaction cannot make this request fit, and retrying is a
	// loop. Loop-local on purpose — it scopes a single call's retry, not
	// cross-claim state, so the derived-state discipline does not apply.
	reactiveCompacted := false

	for {
		if err := ctx.Err(); err != nil {
			return e.cancelled(started, turn)
		}

		// 1. Compaction trip, BEFORE the drain — the same order the resume
		// path uses (compact → drain → call), and for the same reason: a
		// message queued while the window filled must stay queued through
		// the compaction so it lands after the summary as live input. A
		// drain first would deliver it into a span no call ever read, and
		// the flip would swallow it into the summary.
		preRows, err := e.Transcript.ListForAssembly(ctx, params.OrgID, params.ConversationID)
		if err != nil {
			return e.failed(started, turn, fmt.Errorf("list rows for compaction trip: %w", err))
		}
		if e.compactionDue(params, preRows) {
			if err := e.compactWarm(ctx, params); err != nil {
				if ctx.Err() != nil {
					return e.cancelled(started, turn)
				}
				e.insertNotice(ctx, params, "Compacting this conversation failed: "+err.Error())
				return e.failed(started, turn, fmt.Errorf("compact conversation: %w", err))
			}
			continue
		}

		// 2. Drain. Every input — the delegation prompt itself, a user
		// follow-up, a staged injection, a repair notice — enters here, so
		// there is no special first-call case.
		if err := e.drain(ctx, params, bareDrain); err != nil {
			return e.failed(started, turn, fmt.Errorf("drain pending input: %w", err))
		}

		// 2b. Rows — one post-flush read serves the budget derivation and the
		// assembly below; no write lands between them on any path that
		// reaches the call.
		rows, err := e.Transcript.ListForAssembly(ctx, params.OrgID, params.ConversationID)
		if err != nil {
			return e.failed(started, turn, fmt.Errorf("list rows for assembly: %w", err))
		}

		// 3. The turn budget, derived from rows. One call before the bound,
		// ask for a wrap-up so the transcript ends with an account of the
		// work rather than a mid-loop stop; the row's presence since the
		// last human input is what makes the ask once-per-budget, durable
		// across claims.
		//
		// A compaction restarts this derivation as a side effect: the
		// flipped span leaves the assembly read, so its assistant turns no
		// longer count. Deliberate — filling 80% of a model window is
		// substantial real work, spend is the actual brake, and deriving
		// through inactive rows would re-read the very history compaction
		// paid to shed.
		budget := deriveBudget(rows)
		if budget.turns == maxIter-1 && !budget.wrapUpRequested {
			if err := e.insertPending(ctx, params, wrapUpNotice, domain.MessageSubtypeInjectionWrapUp); err != nil {
				return e.failed(started, turn, fmt.Errorf("insert wrap-up notice: %w", err))
			}
			continue
		}

		// 4. Guards, before every call. The turn backstop reads the derived
		// budget; configured guards see the engagement's own call count.
		if notice := e.checkGuards(ctx, params, budget.turns, turn, maxIter); notice != "" {
			if !hasNoticeSince(rows, notice) {
				e.insertNotice(ctx, params, notice)
			}
			return Result{
				Kind:       ResultParked,
				ParkNotice: notice,
				NumTurns:   turn,
				DurationMs: msSince(started),
			}
		}

		// 5. Credentials, per call.
		provider, client, release, err := e.Credentials.ForCall(ctx)
		if err != nil {
			return e.failed(started, turn, fmt.Errorf("resolve provider credentials: %w", err))
		}

		// 6. Stream.
		callStarted := time.Now()
		completion, err := e.streamWithRetry(ctx, client, e.buildRequest(params, provider, rows))
		if release != nil {
			release()
		}
		if err != nil {
			if ctx.Err() != nil {
				return e.cancelled(started, turn)
			}
			// Reactive arm: a context-overflow rejection is a compaction
			// trigger, not a run failure — the proactive trip estimates and
			// the estimate will sometimes be wrong. Compact (warm while the
			// prior call's cache lives, forced-shape otherwise) and retry
			// this call exactly once.
			if errors.Is(err, inference.ErrContextOverflow) && !reactiveCompacted {
				reactiveCompacted = true
				if cerr := e.compactAfterOverflow(ctx, params); cerr == nil {
					continue
				} else if ctx.Err() != nil {
					return e.cancelled(started, turn)
				} else {
					err = fmt.Errorf("compact after context overflow: %w (overflow: %v)", cerr, err)
				}
			}
			// A crash or an exhausted retry between stream start and persist
			// loses this message entirely, which is safe: its tool calls
			// never ran. Record the error as a row so the failure has a
			// visible cause in the transcript, then fail.
			e.insertNotice(ctx, params, "The model call failed and could not be retried: "+err.Error())
			return e.failed(started, turn, err)
		}
		turn++
		reactiveCompacted = false

		// 7. Persist — one row, priced, display columns populated. Under a
		// `length` stop the cut usually lands mid-arguments, leaving the
		// final tool call's JSON unparseable; stub those arguments empty —
		// under that stop reason only — so the row persists and step 7 can
		// answer the batch. Everywhere else malformed arguments stay a loud
		// persist failure: with no truncation to blame they are a provider
		// bug, not something to paper over.
		if isLengthStop(completion.FinishReason) {
			stubTruncatedToolArgs(completion)
		}
		assistantRow, err := e.persistAssistant(ctx, params, completion, msSince(callStarted))
		if err != nil {
			return e.failed(started, turn, fmt.Errorf("persist assistant message: %w", err))
		}
		lastText = assistantRow.Content
		calls := assistantRow.ToolCalls

		// 8. Stop-reason handling. A `length` stop with tool calls present
		// is a truncated batch. The cut call's arguments were stubbed above,
		// and even a call whose JSON parsed may be silently missing the tail
		// of what the model meant to write — so no call in the message is
		// safe to run. Answer every one with an instructive error instead of
		// executing any of them.
		if isLengthStop(completion.FinishReason) && len(calls) > 0 {
			for _, call := range calls {
				if err := e.insertToolResult(ctx, params, call, truncatedBatchNotice, true); err != nil {
					return e.failed(started, turn, err)
				}
			}
			bareDrain = false
			continue
		}

		// 9. Dispatch. Flow-control calls resolve loop-side; everything
		// else goes into the jail, serially, in call order.
		if len(calls) > 0 {
			outcome, terminated, err := e.dispatchBatch(ctx, params, calls)
			if err != nil {
				// A cancellation observed mid-batch is a cancellation, not a
				// failure: the user asked to stop, and the calls that did not
				// run simply did not run.
				if ctx.Err() != nil {
					return e.cancelled(started, turn)
				}
				return e.failed(started, turn, err)
			}
			if terminated {
				// stop_blueprint's summary argument is the account of the
				// work; the prose in the same assistant message backs it up
				// should a terminal outcome ever arrive without one.
				if outcome.ResultSummary == "" {
					outcome.ResultSummary = lastText
				}
				outcome.NumTurns = turn
				outcome.DurationMs = msSince(started)
				return outcome
			}
			bareDrain = false
			continue
		}

		// 10. Would-stop. A no-tool-call message concludes the run — but
		// only after two rechecks, both of which can legitimately keep it
		// going.
		bareDrain = true

		// Late-signal recheck: input that landed while this turn was
		// streaming means the model has not seen the newest instruction, so
		// concluding now would drop it.
		pending, err := e.hasPending(ctx, params)
		if err != nil {
			return e.failed(started, turn, fmt.Errorf("recheck pending input: %w", err))
		}
		if pending {
			continue
		}

		// A wrap-up turn stopping is not a conclusion. The budget asked for
		// this reply; treating it as the step being done would advance the
		// blueprint on work the bound cut short. Park instead — the summary
		// just persisted is the account the transcript ends on, and a user
		// message resumes with a fresh budget. The hook is deliberately not
		// consulted: nudging more work out of an exhausted budget would
		// contradict the wrap-up ask one turn earlier.
		if budget.wrapUpRequested {
			notice := limitParkNotice(maxIter)
			if !hasNoticeSince(rows, notice) {
				e.insertNotice(ctx, params, notice)
			}
			return Result{
				Kind:          ResultParked,
				ParkNotice:    notice,
				ResultSummary: lastText,
				NumTurns:      turn,
				DurationMs:    msSince(started),
			}
		}

		// The would-stop hook (the artifact contract, today). Its answer is
		// taken as given — see Hooks.ShouldStopAfterTurn for why the engine
		// keeps no state about whether it has fired.
		if e.Hooks.ShouldStopAfterTurn != nil {
			if nudge := e.Hooks.ShouldStopAfterTurn(ctx, turn, lastText); nudge != "" {
				if err := e.insertPending(ctx, params, nudge, domain.MessageSubtypeInjectionNudge); err != nil {
					return e.failed(started, turn, fmt.Errorf("insert turn-end nudge: %w", err))
				}
				continue
			}
		}

		// Implicit completion. The final assistant text is the summary —
		// there is no JSON envelope to parse on this path.
		//
		// The outcome is `continue`, not `finish`, on every step: stopping
		// means "my part is done", and only decideBlueprintStep knows whether
		// a part being done ends the task. On a final or single step it
		// resolves `continue` to a structural finish, so the common case is
		// unchanged; on a non-final step it hands off. Ending the whole task
		// early is the deliberate act, and it goes through stop_blueprint.
		return Result{
			Kind:          ResultConcluded,
			Outcome:       domain.RunOutcomeContinue,
			ResultSummary: lastText,
			NumTurns:      turn,
			DurationMs:    msSince(started),
		}
	}
}

// buildRequest is the single constructor for a conversational provider
// call. The warm compaction arm calls it too, and that shared construction
// IS the request-invariance contract: the summarize call differs from the
// call the loop would otherwise make by its appended request row and by
// nothing else — same system prompt, same tools in the same order, same
// (absent) tool_choice, same model, same effort, same token cap. Changing
// any of these forfeits the cached prefix; only the forced-shape call, which
// has already forfeited it, builds a request any other way.
func (e *Engine) buildRequest(params Params, provider schemas.ModelProvider, rows []domain.Message) inference.Request {
	return inference.Request{
		Provider:     provider,
		Model:        params.Model,
		SystemPrompt: params.SystemPrompt,
		Rows:         rows,
		Tools:        e.toolSchemas(params),
		Effort:       params.Effort,
		MaxTokens:    params.MaxTokens,
	}
}

// toolSchemas assembles the call's tool list: the seven in-jail tools, plus
// flow control when there is a blueprint to control. Built per call from
// immutable inputs so no mutation can leak between engagements. Within a
// blueprint the list does not vary with the step's position — only the
// system prompt does.
func (e *Engine) toolSchemas(params Params) []schemas.ChatTool {
	sandboxed := SandboxTools()
	flow := flowControlTools(params.HasBlueprint)
	out := make([]schemas.ChatTool, 0, len(sandboxed)+len(flow))
	out = append(out, sandboxed...)
	out = append(out, flow...)
	return out
}

// checkGuards runs the turn backstop and every configured guard, returning
// the first notice that says stop. The backstop is checked here rather than
// as a Guard so its bound travels with the params; it reads the
// transcript-derived budget, while configured guards see the engagement's
// own call count.
func (e *Engine) checkGuards(ctx context.Context, params Params, budgetTurns, turn, maxIter int) string {
	if budgetTurns >= maxIter {
		return limitParkNotice(maxIter)
	}
	for _, g := range e.Guards {
		notice, err := g.Check(ctx, turn)
		if err != nil {
			// Fail open, deliberately and consistently with the admission
			// gate this mirrors: a guard that can't read its own inputs must
			// not wedge every run in the fleet.
			e.warn("agent loop guard check failed; proceeding", "conversation", params.ConversationID, "error", err)
			continue
		}
		if notice != "" {
			return notice
		}
	}
	return ""
}

// isLengthStop recognizes the provider's truncation stop reason across the
// two spellings the neutral layer surfaces.
func isLengthStop(reason string) bool {
	return reason == "length" || reason == "max_tokens"
}

// stubTruncatedToolArgs blanks any tool-call arguments in the completion that
// do not parse as JSON. Licensed only under a length stop, where the provider
// cut the stream mid-arguments and the concatenated fragments cannot form a
// whole document; the strict parse stays in force on every other path. The
// stubbed call still persists and is answered with truncatedBatchNotice, so
// nothing runs on the partial input.
func stubTruncatedToolArgs(completion *inference.Completion) {
	assistant := completion.Message.ChatAssistantMessage
	if assistant == nil {
		return
	}
	for i := range assistant.ToolCalls {
		args := assistant.ToolCalls[i].Function.Arguments
		if args == "" || json.Valid([]byte(args)) {
			continue
		}
		assistant.ToolCalls[i].Function.Arguments = "{}"
	}
}

// truncatedBatchNotice is the instructive result every call in a
// length-truncated assistant message receives. It names the cause and the
// remedy, because the model cannot otherwise tell a truncated argument
// object from one it wrote badly.
const truncatedBatchNotice = "This tool call was not executed: the model's response hit the output length limit, " +
	"so the tool arguments in that message may be silently truncated and are not safe to run. " +
	"Re-issue the call you intended, with smaller arguments or fewer calls in one message."

func (e *Engine) failed(started time.Time, turn int, err error) Result {
	return Result{
		Kind:        ResultFailed,
		FailureKind: domain.RunFailureAgentError,
		NumTurns:    turn,
		DurationMs:  msSince(started),
		Err:         err,
	}
}

func (e *Engine) cancelled(started time.Time, turn int) Result {
	return Result{
		Kind:       ResultCancelled,
		NumTurns:   turn,
		DurationMs: msSince(started),
		Err:        context.Canceled,
	}
}

func (e *Engine) warn(msg string, args ...any) {
	if e.Log != nil {
		e.Log.Warn(msg, args...)
	}
}

func (e *Engine) info(msg string, args ...any) {
	if e.Log != nil {
		e.Log.Info(msg, args...)
	}
}

func msSince(t time.Time) int {
	return int(time.Since(t) / time.Millisecond)
}

// ErrNoCredentials is returned by a Credentials implementation that has
// nothing to resolve — surfaced rather than papered over, since a native
// call cannot fall back to anything.
var ErrNoCredentials = errors.New("agentloop: no LLM credentials available for this conversation")
