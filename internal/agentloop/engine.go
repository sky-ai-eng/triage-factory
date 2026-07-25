package agentloop

import (
	"context"
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
//
// There is deliberately NO context-transform hook. Assembly is a pure
// function of rows; anything that wants to change what the model sees must
// become a column, and window shaping happens at cold moments only.
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

	// PrepareNextTurn is RESERVED and never called. Compaction attaches
	// here (P3): it is the one point at which a batched, cold-moment window
	// pass may run without violating KV-cache discipline. Declared now so
	// the seam's shape is settled before there is pressure to add one
	// somewhere worse.
	PrepareNextTurn func(ctx context.Context) error
}

// Spec is one engagement: everything the loop needs that isn't a dependency.
type Spec struct {
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
	// nobody absent to leave a reason for. It must agree with
	// EnvelopeParts.HasBlueprint, which appends the text describing the tool.
	HasBlueprint bool

	// MaxIterations bounds the engagement's provider calls. Zero uses
	// DefaultMaxIterations.
	MaxIterations int

	// MaxTokens and Effort ride through to the provider call unchanged.
	MaxTokens int
	Effort    string

	// UserID attributes rows the loop writes on a user's behalf (the
	// repair notice, drained input it re-stamps). Empty for event runs.
	UserID string
}

// Disposition is how an engagement ended.
type Disposition int

const (
	// DispositionConcluded — the model finished: an assistant message with
	// no tool calls, or a flow-control tool. Outcome carries which.
	DispositionConcluded Disposition = iota
	// DispositionParked — a guard tripped. The conversation is `open` with
	// a notice row; the next claim continues it.
	DispositionParked
	// DispositionFailed — the engagement could not continue (retry
	// exhaustion, a fatal tool-host error). The conversation fails.
	DispositionFailed
	// DispositionCancelled — the context was cancelled.
	DispositionCancelled
)

// Result is the engagement's terminal report, shaped so the caller can hand
// it straight to the existing completion bookkeeping.
type Result struct {
	Disposition   Disposition
	Outcome       domain.RunOutcome
	OutcomeReason string
	ResultSummary string
	FailureKind   domain.RunFailureKind
	NumTurns      int
	DurationMs    int
	// ParkNotice is the user-visible reason a guard parked the engagement.
	ParkNotice string
	// Err carries the underlying cause on DispositionFailed.
	Err error
}

// DefaultMaxIterations bounds provider calls per engagement. It is a
// backstop against a cheap-call loop, not a work budget: spend is the real
// brake, so this is set generously enough that a legitimately long task
// never trips it.
const DefaultMaxIterations = 400

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
// contract: drain (the only door input enters through) → guards (before
// every call, not every turn) → assemble → credentials → stream → persist →
// stop-reason handling → tool dispatch → would-stop.
func (e *Engine) Run(ctx context.Context, spec Spec) Result {
	started := time.Now()
	maxIter := spec.MaxIterations
	if maxIter <= 0 {
		maxIter = DefaultMaxIterations
	}

	// Setup: make the transcript legal and honest before anything reads it.
	// Unconditional and idempotent — there is no resume branch, so the
	// crash-recovery path is the path that runs every time and cannot rot.
	if err := e.repairTranscript(ctx, spec); err != nil {
		return e.failed(started, 0, fmt.Errorf("repair transcript: %w", err))
	}

	// bareDrain is true for the engagement's first drain and for any drain
	// that follows a no-tool-call assistant message. Every other drain
	// happens between turns, while the model is mid-work, and is stamped as
	// a steer so assembly can wrap it in the keep-working envelope.
	bareDrain := true
	turn := 0
	lastText := ""

	for {
		if err := ctx.Err(); err != nil {
			return e.cancelled(started, turn)
		}

		// 1. Drain. Every input — the delegation prompt itself, a user
		// follow-up, a staged injection, a repair notice — enters here, so
		// there is no special first-call case.
		if err := e.drain(ctx, spec, bareDrain); err != nil {
			return e.failed(started, turn, fmt.Errorf("drain pending input: %w", err))
		}

		// 2. Guards, before every call.
		if notice := e.checkGuards(ctx, spec, turn, maxIter); notice != "" {
			e.insertNotice(ctx, spec, notice)
			return Result{
				Disposition: DispositionParked,
				ParkNotice:  notice,
				NumTurns:    turn,
				DurationMs:  msSince(started),
			}
		}

		// 3. Assemble — rows only.
		rows, err := e.Transcript.ListForAssembly(ctx, spec.OrgID, spec.ConversationID)
		if err != nil {
			return e.failed(started, turn, fmt.Errorf("list rows for assembly: %w", err))
		}
		tools, err := e.toolSchemas(spec)
		if err != nil {
			return e.failed(started, turn, err)
		}

		// 4. Credentials, per call.
		provider, client, release, err := e.Credentials.ForCall(ctx)
		if err != nil {
			return e.failed(started, turn, fmt.Errorf("resolve provider credentials: %w", err))
		}

		// 5. Stream.
		completion, err := e.streamWithRetry(ctx, client, inference.Request{
			Provider:     provider,
			Model:        spec.Model,
			SystemPrompt: spec.SystemPrompt,
			Rows:         rows,
			Tools:        tools,
			Effort:       spec.Effort,
			MaxTokens:    spec.MaxTokens,
		})
		if release != nil {
			release()
		}
		if err != nil {
			if ctx.Err() != nil {
				return e.cancelled(started, turn)
			}
			// A crash or an exhausted retry between stream start and persist
			// loses this message entirely, which is safe: its tool calls
			// never ran. Record the error as a row so the failure has a
			// visible cause in the transcript, then fail.
			e.insertNotice(ctx, spec, "The model call failed and could not be retried: "+err.Error())
			return e.failed(started, turn, err)
		}
		turn++

		// 6. Persist — one row, priced, display columns populated.
		assistantRow, err := e.persistAssistant(ctx, spec, completion)
		if err != nil {
			return e.failed(started, turn, fmt.Errorf("persist assistant message: %w", err))
		}
		lastText = assistantRow.Content
		calls := assistantRow.ToolCalls

		// 7. Stop-reason handling. A `length` stop with tool calls present
		// is a truncated batch: streamed arguments are JSON-salvage-finalized
		// on the way in, so a call can validate while silently missing
		// arguments. Answer every call in that message with an instructive
		// error instead of executing any of them.
		if isLengthStop(completion.FinishReason) && len(calls) > 0 {
			for _, call := range calls {
				if err := e.insertToolResult(ctx, spec, call, truncatedBatchNotice, true); err != nil {
					return e.failed(started, turn, err)
				}
			}
			bareDrain = false
			continue
		}

		// 8/9. Dispatch. Flow-control calls resolve loop-side; everything
		// else goes into the jail, serially, in call order.
		if len(calls) > 0 {
			outcome, terminated, err := e.dispatchBatch(ctx, spec, calls)
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
				// stop_blueprint carries no summary of its own — the prose in the
				// same assistant message is the account of the work, exactly
				// as it is when the model concludes by stopping.
				outcome.ResultSummary = lastText
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
		pending, err := e.hasPending(ctx, spec)
		if err != nil {
			return e.failed(started, turn, fmt.Errorf("recheck pending input: %w", err))
		}
		if pending {
			continue
		}

		// The would-stop hook (the artifact contract, today). Its answer is
		// taken as given — see Hooks.ShouldStopAfterTurn for why the engine
		// keeps no state about whether it has fired.
		if e.Hooks.ShouldStopAfterTurn != nil {
			if nudge := e.Hooks.ShouldStopAfterTurn(ctx, turn, lastText); nudge != "" {
				if err := e.insertPending(ctx, spec, nudge, ""); err != nil {
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
			Disposition:   DispositionConcluded,
			Outcome:       domain.RunOutcomeContinue,
			ResultSummary: lastText,
			NumTurns:      turn,
			DurationMs:    msSince(started),
		}
	}
}

// toolSchemas assembles the call's tool list: the seven in-jail tools, plus
// flow control when there is a blueprint to control. Built per call from
// immutable inputs so no mutation can leak between engagements. Within a
// blueprint the list does not vary with the step's position — only the
// system prompt does.
func (e *Engine) toolSchemas(spec Spec) ([]schemas.ChatTool, error) {
	sandboxed, err := SandboxTools()
	if err != nil {
		return nil, err
	}
	flow := flowControlTools(spec.HasBlueprint)
	out := make([]schemas.ChatTool, 0, len(sandboxed)+len(flow))
	out = append(out, sandboxed...)
	out = append(out, flow...)
	return out, nil
}

// checkGuards runs the turn backstop and every configured guard, returning
// the first notice that says stop. The backstop is checked here rather than
// as a Guard so its bound travels with the spec.
func (e *Engine) checkGuards(ctx context.Context, spec Spec, turn, maxIter int) string {
	if turn >= maxIter {
		return fmt.Sprintf("This run reached its limit of %d model calls in a single engagement and has been paused. "+
			"Nothing is lost — send a message to pick it back up.", maxIter)
	}
	for _, g := range e.Guards {
		notice, err := g.Check(ctx, turn)
		if err != nil {
			// Fail open, deliberately and consistently with the admission
			// gate this mirrors: a guard that can't read its own inputs must
			// not wedge every run in the fleet.
			e.warn("agent loop guard check failed; proceeding", "conversation", spec.ConversationID, "error", err)
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

// truncatedBatchNotice is the instructive result every call in a
// length-truncated assistant message receives. It names the cause and the
// remedy, because the model cannot otherwise tell a truncated argument
// object from one it wrote badly.
const truncatedBatchNotice = "This tool call was not executed: the model's response hit the output length limit, " +
	"so the tool arguments in that message may be silently truncated and are not safe to run. " +
	"Re-issue the call you intended, with smaller arguments or fewer calls in one message."

func (e *Engine) failed(started time.Time, turn int, err error) Result {
	return Result{
		Disposition: DispositionFailed,
		FailureKind: domain.RunFailureAgentError,
		NumTurns:    turn,
		DurationMs:  msSince(started),
		Err:         err,
	}
}

func (e *Engine) cancelled(started time.Time, turn int) Result {
	return Result{
		Disposition: DispositionCancelled,
		NumTurns:    turn,
		DurationMs:  msSince(started),
		Err:         context.Canceled,
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
