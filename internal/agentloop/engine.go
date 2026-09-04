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
	// "" is bounded by the spend guard like any other work.
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

	// BashMemBudgetMB bounds the resident memory of any single `bash`
	// command the jail runs for this engagement. A command over it is killed
	// and answered with an error naming the limit; the jail's own memory
	// ceiling stays underneath as the backstop, so this is about attributing
	// a breach to the command that caused it rather than to whoever
	// allocates next.
	//
	// Zero disables it: no configure frame is sent, and the harness arms no
	// watchdog. Policy, so it comes from the driver as a constant — there is
	// no env knob and no settings surface, and it is never a tool argument
	// the model could raise for itself.
	BashMemBudgetMB int

	// Effort rides through to the provider call unchanged.
	Effort string

	// MaxTokens pins the completion cap for every call this engagement
	// makes. Zero — what every caller passes — resolves the per-provider
	// budget policy (inference.MaxOutputTokens) against the provider each
	// call routes to, which is the only place that knows both halves of it:
	// Anthropic bills what it generates and a generous cap is free, while
	// Bedrock reserves the cap against the account's quota at admission and
	// an oversized one throttles requests that would have fit.
	MaxTokens int

	// MissionAnchored marks a conversation whose opening turn is a
	// control-plane-minted mission (a task/blueprint-backed delegation).
	// Compaction pins that opening — the summary can never mutate the task,
	// and the cacheable prefix survives. Taskless surfaces (a free-form
	// manual run) pass false: their first message is just the
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

	// Workspace is how the run tree this engagement entered came to be. The
	// claim path resolves it; the loop cannot, since a warm tree and a
	// reconstruction of one are the same directory from in here.
	//
	// It gates the one thing the loop says about the workspace. Unset means
	// the caller does not restore workspaces and nothing is claimed about
	// this one — the same silence a warm tree gets, because a restore that
	// cannot be established must not be asserted.
	Workspace domain.WorkspaceProvenance

	// ExecutorChanged reports that the engagement before this one ran on a
	// different executor. Read alongside Workspace: it only ever adds a
	// sentence to a notice a restore already earned.
	ExecutorChanged bool
}

// ResultKind is how an engagement ended.
type ResultKind int

const (
	// ResultConcluded — the model finished: an assistant message with
	// no tool calls, or a flow-control tool. Outcome carries which.
	ResultConcluded ResultKind = iota
	// ResultParked — the engagement stopped short of concluding: a guard
	// tripped before a call, or the provider stopped for a reason the loop
	// will not act on and only a person can resolve. The conversation is
	// `open` with a notice row; the next claim continues it.
	ResultParked
	// ResultFailed — the engagement could not continue (retry
	// exhaustion, a fatal tool-host error) with its context still live.
	// The conversation fails.
	ResultFailed
	// ResultCancelled — the context was cancelled. Every exit under a
	// killed context reports this, however the kill was observed: a write
	// that came back "context canceled" is a cancellation, not a failure
	// that happens to mention one.
	ResultCancelled
)

// Result is the engagement's terminal report, returned to the in-process
// caller that drove the engagement (the delegate layer's recordNativeResult).
// It deliberately lives here and not in domain: the shared vocabulary it
// carries (ConversationOutcome, ConversationFailureKind) is already domain's, and the rest is
// the shape of one function's return value.
type Result struct {
	Kind          ResultKind
	Outcome       domain.ConversationOutcome
	OutcomeReason string
	ResultSummary string
	FailureKind   domain.ConversationFailureKind
	NumTurns      int
	DurationMs    int
	// ParkNotice is the user-visible reason a guard parked the engagement.
	ParkNotice string
	// Err carries the underlying cause on ResultFailed.
	Err error
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
// guards (before every call, not every turn) → assemble → credentials →
// stream → persist → stop-reason handling → tool dispatch → would-stop.
func (e *Engine) Run(ctx context.Context, params Params) Result {
	started := time.Now()

	// Policy into the jail, before anything can dispatch a tool. One frame,
	// once per engagement — the host holds it for the life of the connection.
	e.configureToolHost(params)

	// Setup: make the transcript legal and honest before anything reads it.
	// Unconditional and idempotent — there is no resume branch, so the
	// crash-recovery path is the path that runs every time and cannot rot.
	if err := e.repairTranscript(ctx, params); err != nil {
		return e.failed(ctx, started, 0, fmt.Errorf("repair transcript: %w", err))
	}

	// Cold-resume compaction, after repair and before the first drain:
	// repair → compact → drain → call, so the summarize call reads exactly
	// what the previous engagement left and queued input stays queued
	// through it.
	if err := e.compactOnResume(ctx, params); err != nil {
		if ctx.Err() != nil {
			return e.cancelled(ctx, started, 0)
		}
		e.insertNotice(ctx, params, "Compacting this conversation on resume failed: "+err.Error())
		return e.failed(ctx, started, 0, fmt.Errorf("compact on resume: %w", err))
	}

	// bareDrain is true for the engagement's first drain and for any drain
	// that follows a no-tool-call assistant message. Every other drain
	// happens between turns, while the model is mid-work, and is stamped as
	// a steer so assembly can wrap it in the keep-working envelope.
	bareDrain := true
	turn := 0
	lastText := ""

	// emptyLengthStops counts CONSECUTIVE turns the provider cut at the
	// output limit with nothing to show for them, and escalatedMaxTokens is
	// the one-shot larger cap the next call gets after one. Both are
	// loop-local for the same reason reactiveCompacted is: they scope a
	// single call's retry, not cross-claim state. A re-claim starting the
	// count over is the right answer — it retries once more with the notice
	// already in the transcript, which beats inheriting a verdict about calls
	// a different engagement made.
	emptyLengthStops := 0
	escalatedMaxTokens := 0

	// windowWallStops counts CONSECUTIVE turns the provider cut at the
	// context window wall, licensing one compaction-and-retry between them.
	// Loop-local for the same reason as the two above; a re-claim starting
	// the count over reads a transcript the first wall already compacted,
	// which is a different experiment from the one that just failed.
	windowWallStops := 0

	// reactiveCompacted licenses at most one overflow-triggered compaction
	// per provider call: a second overflow immediately after compacting
	// means compaction cannot make this request fit, and retrying is a
	// loop. Loop-local on purpose — it scopes a single call's retry, not
	// cross-claim state, so the derived-state discipline does not apply.
	reactiveCompacted := false

	for {
		if err := ctx.Err(); err != nil {
			return e.cancelled(ctx, started, turn)
		}

		// 1. Compaction trip, BEFORE the drain — the same order the resume
		// path uses (compact → drain → call), and for the same reason: a
		// message queued while the window filled must stay queued through
		// the compaction so it lands after the summary as live input. A
		// drain first would deliver it into a span no call ever read, and
		// the flip would swallow it into the summary.
		preRows, err := e.Transcript.ListForAssembly(ctx, params.OrgID, params.ConversationID)
		if err != nil {
			return e.failed(ctx, started, turn, fmt.Errorf("list rows for compaction trip: %w", err))
		}
		if e.compactionDue(params, preRows) {
			if err := e.compactWarm(ctx, params); err != nil {
				if ctx.Err() != nil {
					return e.cancelled(ctx, started, turn)
				}
				e.insertNotice(ctx, params, "Compacting this conversation failed: "+err.Error())
				return e.failed(ctx, started, turn, fmt.Errorf("compact conversation: %w", err))
			}
			continue
		}

		// 2. Drain. Every input — the delegation prompt itself, a user
		// follow-up, a staged injection, a repair notice — enters here, so
		// there is no special first-call case.
		if err := e.drain(ctx, params, bareDrain); err != nil {
			return e.failed(ctx, started, turn, fmt.Errorf("drain pending input: %w", err))
		}

		// 2b. Rows — one post-flush read serves the park-notice dedupe and
		// the assembly below; no write lands between them on any path that
		// reaches the call.
		rows, err := e.Transcript.ListForAssembly(ctx, params.OrgID, params.ConversationID)
		if err != nil {
			return e.failed(ctx, started, turn, fmt.Errorf("list rows for assembly: %w", err))
		}

		// 3. Guards, before every call. Each sees the engagement's own call
		// count.
		if notice := e.checkGuards(ctx, params, turn); notice != "" {
			if !HasNoticeSince(rows, notice) {
				e.insertNotice(ctx, params, notice)
			}
			return Result{
				Kind:       ResultParked,
				ParkNotice: notice,
				NumTurns:   turn,
				DurationMs: msSince(started),
			}
		}

		// 4. Credentials, per call.
		provider, client, release, err := e.Credentials.ForCall(ctx)
		if err != nil {
			return e.failed(ctx, started, turn, fmt.Errorf("resolve provider credentials: %w", err))
		}

		// 5. Stream. The cap is this engagement's resolved per-provider
		// budget, unless the previous turn hit the limit having produced
		// nothing — then it is the one-shot escalation, consumed here so it
		// applies to exactly the call that follows the stop.
		maxTokens := callMaxTokens(params, provider)
		if escalatedMaxTokens > 0 {
			maxTokens = escalatedMaxTokens
			escalatedMaxTokens = 0
		}
		callStarted := time.Now()
		completion, err := e.streamWithRetry(ctx, client, e.buildRequest(params, provider, rows, maxTokens))
		if release != nil {
			release()
		}
		if err != nil {
			if ctx.Err() != nil {
				return e.cancelled(ctx, started, turn)
			}
			// Reactive arm: a context-overflow rejection is a compaction
			// trigger, not a run failure — the proactive trip estimates and
			// the estimate will sometimes be wrong. Compact (warm while the
			// prior call's cache lives, forced-shape otherwise) and retry
			// this call exactly once.
			if errors.Is(err, inference.ErrContextOverflow) && !reactiveCompacted {
				reactiveCompacted = true
				if cerr := e.compactWindowFull(ctx, params); cerr == nil {
					continue
				} else if ctx.Err() != nil {
					return e.cancelled(ctx, started, turn)
				} else {
					err = fmt.Errorf("compact after context overflow: %w (overflow: %v)", cerr, err)
				}
			}
			// A crash or an exhausted retry between stream start and persist
			// loses this message entirely, which is safe: its tool calls
			// never ran. Record the error as a row so the failure has a
			// visible cause in the transcript, then fail.
			e.insertNotice(ctx, params, "The model call failed and could not be retried: "+err.Error())
			return e.failed(ctx, started, turn, err)
		}
		turn++
		reactiveCompacted = false

		// 6. Persist — one row, priced, display columns populated. A message
		// the provider ended rather than the model finishing usually lands
		// mid-arguments, leaving the final tool call's JSON unparseable; stub
		// those arguments empty — under those stop reasons only — so the row
		// persists and step 7 can answer the batch. Everywhere else malformed
		// arguments stay a loud persist failure: with nothing to blame for
		// the damage they are a provider bug, not something to paper over.
		class := classifyStop(completion.FinishReason)
		if completion.FinishReason == "" {
			// Read as an ordinary stop, per classifyStop. The warn is the only
			// trace it leaves, since an absent reason is deliberately not
			// stored — "unknown" and "not reported" stay the same thing.
			e.warn("provider reported no finish reason; reading the turn as an ordinary stop",
				"conversation", params.ConversationID, "model", params.Model)
		}
		if class.interrupted() {
			stubTruncatedToolArgs(completion)
		}
		assistantRow, err := e.persistAssistant(ctx, params, completion, msSince(callStarted))
		if err != nil {
			return e.failed(ctx, started, turn, fmt.Errorf("persist assistant message: %w", err))
		}
		lastText = assistantRow.Content
		calls := assistantRow.ToolCalls

		// 7. Stop-reason handling, as an allowlist: only an end-of-turn stop
		// reaches the would-stop path at step 9, and every other reason
		// takes a named arm right here. See stopreason.go for why the default
		// has to be "park", never "conclude".
		//
		// Both consecutive-stop counters bound a provider repeating itself,
		// so any other outcome in between clears them — one healthy turn
		// means the loop is making progress again.
		if class != stopLength {
			emptyLengthStops = 0
		}
		if class != stopWindowWall {
			windowWallStops = 0
		}

		switch class {
		case stopLength:
			// A length stop with tool calls present is a truncated batch. The
			// cut call's arguments were stubbed above, and even a call whose
			// JSON parsed may be silently missing the tail of what the model
			// meant to write — so no call in the message is safe to run.
			// Answer every one with an instructive error instead of executing
			// any of them.
			if len(calls) > 0 {
				emptyLengthStops = 0
				if err := e.answerUndispatchedCalls(ctx, params, assistantRow.ID, calls, truncatedBatchNotice); err != nil {
					return e.failed(ctx, started, turn, err)
				}
				bareDrain = false
				continue
			}

			// The same stop with an EMPTY batch: the cut landed before the
			// model produced any text or any tool call, which on a thinking
			// model means the whole cap went to reasoning. There is no batch
			// to answer and — the bug this arm exists for — nothing to
			// conclude from either. Concluding here would read a turn that
			// produced literally nothing as the step being finished, and
			// advance a blueprint on it.
			emptyLengthStops++
			if emptyLengthStops >= maxEmptyLengthStops {
				// Twice in a row means the cap is not the obstacle — the
				// escalated call had strictly more room and still produced
				// nothing. Fail with the limit named, rather than buying a
				// third identical turn.
				notice := outputLimitFailureNotice(maxTokens)
				e.insertNotice(ctx, params, notice)
				return e.failed(ctx, started, turn, fmt.Errorf(
					"two consecutive model turns hit the %d-token output limit without producing text or a tool call", maxTokens))
			}
			escalatedMaxTokens = escalateMaxTokens(maxTokens, params.Model)
			e.warn("model turn hit the output token limit before producing anything; retrying with a larger cap",
				"conversation", params.ConversationID, "model", params.Model,
				"max_tokens", maxTokens, "retry_max_tokens", escalatedMaxTokens)
			if err := e.insertDelivered(ctx, params, outputLimitNotice, domain.MessageSubtypeInjectionOutputLimit); err != nil {
				return e.failed(ctx, started, turn, fmt.Errorf("insert output-limit notice: %w", err))
			}
			// Mid-work, not concluding: the model was on its way to an action
			// when the cut landed, so input arriving now is a steer.
			bareDrain = false
			continue

		case stopWindowWall:
			// The window is genuinely full — the same event the reactive arm
			// answers when it arrives as a rejection, arriving instead as a
			// 200 with a half-written message. So it gets the same remedy:
			// rewind to a valid prefix and compact. Escalating the cap, the
			// length arm's answer, would make the wall arrive sooner.
			//
			// No call in a wall-cut message is ever dispatched. Answering
			// them before compacting is what keeps the transcript legal for
			// the summarize call about to read it — an unanswered tool_use is
			// rejected on the wire, and this arm continues in-process, so no
			// repair pass runs in between. The answers flip inactive with the
			// rest of the span a moment later.
			if err := e.answerUndispatchedCalls(ctx, params, assistantRow.ID, calls, wallCutBatchNotice); err != nil {
				return e.failed(ctx, started, turn, err)
			}
			windowWallStops++
			if windowWallStops >= maxWindowWallStops {
				e.insertNotice(ctx, params, windowWallFailureNotice)
				return e.failed(ctx, started, turn, errors.New(
					"the model context window was exhausted mid-reply again on the call immediately after compacting"))
			}
			e.warn("model call stopped at the context window wall; compacting",
				"conversation", params.ConversationID, "model", params.Model, "max_tokens", maxTokens)
			if err := e.compactWindowFull(ctx, params); err != nil {
				if ctx.Err() != nil {
					return e.cancelled(ctx, started, turn)
				}
				e.insertNotice(ctx, params, "Compacting this conversation failed: "+err.Error())
				return e.failed(ctx, started, turn, fmt.Errorf("compact after context window wall: %w", err))
			}
			bareDrain = false
			continue

		case stopDecline:
			// A person decides what happens next. Retrying a refusal
			// reproduces it, and the loop has nothing to change about the ask.
			//
			// A decline can land after a tool call has begun streaming — a
			// guardrail scanning output intervenes when it sees something,
			// not before the model starts. Those calls are answered like any
			// other the loop declines to run: whatever the person does with
			// this park, it must not begin by having the model told a call it
			// never made was interrupted.
			if err := e.answerUndispatchedCalls(ctx, params, assistantRow.ID, calls, interruptedCallNotice); err != nil {
				return e.failed(ctx, started, turn, err)
			}
			return e.parkOnStop(ctx, params, started, turn, rows, declineParkNotice(completion.FinishReason))

		case stopUnknown:
			e.warn("provider reported an unrecognized finish reason; parking rather than concluding",
				"conversation", params.ConversationID, "model", params.Model,
				"finish_reason", completion.FinishReason)
			if err := e.answerUndispatchedCalls(ctx, params, assistantRow.ID, calls, interruptedCallNotice); err != nil {
				return e.failed(ctx, started, turn, err)
			}
			return e.parkOnStop(ctx, params, started, turn, rows, unrecognizedStopParkNotice(completion.FinishReason))

		case stopToolCalls:
			// The provider said it stopped to call a tool. If it called none,
			// the message contradicts its own stop reason — dispatch has
			// nothing to run and there is no completed turn to conclude from.
			if len(calls) == 0 {
				return e.parkOnStop(ctx, params, started, turn, rows, emptyToolBatchParkNotice)
			}

		case stopEndTurn:
			// The one class that may conclude — at step 9, and only after
			// dispatch, since a message carrying tool calls has work to
			// answer whatever the provider called the stop.
		}

		// 8. Dispatch. Flow-control calls resolve loop-side; everything
		// else goes into the jail, serially, in call order.
		if len(calls) > 0 {
			outcome, terminated, err := e.dispatchBatch(ctx, params, assistantRow.ID, calls)
			if err != nil {
				return e.failed(ctx, started, turn, err)
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

		// 9. Would-stop. A no-tool-call message concludes the run — but
		// only after two rechecks, both of which can legitimately keep it
		// going.
		bareDrain = true

		// Late-signal recheck: input that landed while this turn was
		// streaming means the model has not seen the newest instruction, so
		// concluding now would drop it.
		pending, err := e.hasPending(ctx, params)
		if err != nil {
			return e.failed(ctx, started, turn, fmt.Errorf("recheck pending input: %w", err))
		}
		if pending {
			continue
		}

		// The would-stop hook (the artifact contract, today). Its answer is
		// taken as given — see Hooks.ShouldStopAfterTurn for why the engine
		// keeps no state about whether it has fired.
		if e.Hooks.ShouldStopAfterTurn != nil {
			if nudge := e.Hooks.ShouldStopAfterTurn(ctx, turn, lastText); nudge != "" {
				if err := e.insertPending(ctx, params, nudge, domain.MessageSubtypeInjectionNudge); err != nil {
					return e.failed(ctx, started, turn, fmt.Errorf("insert turn-end nudge: %w", err))
				}
				continue
			}
		}

		// Implicit completion. The final assistant text is the summary —
		// there is no JSON envelope to parse on this path.
		//
		// The outcome is `continue`, not `finish`, on every step: stopping
		// means "my part is done", and only blueprintDecisionForStepConversation knows
		// whether a part being done ends the task. On a final or single step it
		// resolves `continue` to a structural finish, so the common case is
		// unchanged; on a non-final step it hands off. Ending the whole task
		// early is the deliberate act, and it goes through stop_blueprint.
		return Result{
			Kind:          ResultConcluded,
			Outcome:       domain.ConversationOutcomeContinue,
			ResultSummary: lastText,
			NumTurns:      turn,
			DurationMs:    msSince(started),
		}
	}
}

// configureToolHost hands the tool host this engagement's execution policy —
// today the per-command bash memory budget, and nothing else.
//
// Skipped entirely when there is no policy to send, which is what makes an
// unconfigured engagement byte-identical to the world before the frame
// existed: no frame, no watchdog, no syscall.
//
// Never fatal, in any of the three ways it can fail. A host older than the
// verb answers the ordinary non-fatal unknown_tool; a host that rejects the
// args answers a tool error; a dead connection answers nothing. All three
// mean "this run has no per-command budget", and none of them is worth
// refusing to run the engagement over — the jail ceiling is still underneath,
// and a connection that is genuinely gone fails the first real tool call a
// moment later with a diagnosis this frame could not improve on.
func (e *Engine) configureToolHost(params Params) {
	if params.BashMemBudgetMB <= 0 {
		return
	}
	const notice = "configuring the tool host failed; this engagement runs without a per-command memory budget"
	out, err := e.Tools.Call(toolHostConfigureTool, map[string]any{
		bashMemBudgetArg: params.BashMemBudgetMB,
	})
	switch {
	case err != nil:
		e.warn(notice, "conversation", params.ConversationID, "error", err)
	case out.Protocol != nil:
		e.warn(notice, "conversation", params.ConversationID, "kind", out.Protocol.Kind)
	case out.ToolError != "":
		e.warn(notice, "conversation", params.ConversationID, "error", out.ToolError)
	}
}

// buildRequest is the single constructor for a conversational provider
// call. The warm compaction arm calls it too, and that shared construction
// IS the request-invariance contract: the summarize call differs from the
// call the loop would otherwise make by its appended request row and by
// nothing else — same system prompt, same tools in the same order, same
// (absent) tool_choice, same model, same effort. Changing any of those
// forfeits the cached prefix; only the forced-shape call, which has already
// forfeited it, builds a request any other way.
//
// maxTokens is the one parameter passed in rather than read off params, and
// the one the two callers may legitimately disagree on. The cached prefix is
// the system prompt, the tools and the message prefix — the completion cap is
// not part of it, so the loop's one-shot escalation after a truncated turn
// costs nothing, while tool_choice (which does invalidate) stays absent on
// both.
func (e *Engine) buildRequest(params Params, provider schemas.ModelProvider, rows []domain.Message, maxTokens int) inference.Request {
	return inference.Request{
		Provider:     provider,
		Model:        params.Model,
		SystemPrompt: params.SystemPrompt,
		Rows:         rows,
		Tools:        e.toolSchemas(params),
		Effort:       params.Effort,
		MaxTokens:    maxTokens,
	}
}

// callMaxTokens is the completion cap for one provider call: the
// engagement's explicit override when it has one, else the per-provider
// budget policy resolved against the provider this call actually routes to.
//
// It resolves per call rather than once per engagement because the provider
// does: credentials are resolved per call (an STS triple expires), and the
// budget an Anthropic-direct call may ask for is not the budget a Bedrock
// call may — Bedrock reserves the cap against the account's quota at
// admission, Anthropic bills only what it generates.
func callMaxTokens(params Params, provider schemas.ModelProvider) int {
	if params.MaxTokens > 0 {
		return params.MaxTokens
	}
	return inference.MaxOutputTokens(provider, params.Model)
}

// escalateMaxTokens doubles a cap that just cut a turn short, clamped to what
// the model can actually emit. A model the datasheet doesn't carry has no
// ceiling to clamp against, so its cap stays where the budget policy put it:
// asking for more than a provider accepts turns a truncated turn into a
// rejected request, which is a worse answer than one more try at the same
// size.
func escalateMaxTokens(current int, model string) int {
	ceiling, ok := inference.ModelMaxOutput(model)
	if !ok {
		return current
	}
	// Halve the ceiling rather than double the cap: the comparison has to
	// happen before the multiply, because a caller that pinned an absurd
	// explicit cap would overflow int and come back with a negative number —
	// which is not a clamped cap, it is a malformed request.
	if current >= ceiling/2 {
		return ceiling
	}
	return current * 2
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

// checkGuards runs every configured guard against the engagement's own call
// count, returning the first notice that says stop.
func (e *Engine) checkGuards(ctx context.Context, params Params, turn int) string {
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

// answerUndispatchedCalls writes an is_error result for every call in a
// message no arm will run, so a call is either dispatched or answered within
// the turn it arrived in — never left standing.
//
// Leaving them to the next claim's repair pass would keep the transcript
// legal, since repair runs unconditionally and answers anything dangling. It
// would not keep it honest: repair's synthetic result describes an
// interrupted execution on a restored workspace, and for these messages every
// part of that is false. Nothing ran, nothing was restored, and there is no
// state to go verify.
func (e *Engine) answerUndispatchedCalls(ctx context.Context, params Params, ownerID int, calls []domain.ToolCall, notice string) error {
	at := toolResultPositions(ownerID, len(calls))
	for i, call := range calls {
		if err := e.insertToolResult(ctx, params, call, notice, true, at(i)); err != nil {
			return err
		}
	}
	return nil
}

// parkOnStop parks the engagement `open` with a notice — the disposition
// every stop reason the loop will not act on shares. Same shape as a guard
// park: the conversation is resumable, the next user message wakes it, and
// the notice is deduped against the window since the last human input so a
// re-claim that stops the same way does not restate it.
func (e *Engine) parkOnStop(ctx context.Context, params Params, started time.Time, turn int, rows []domain.Message, notice string) Result {
	if !HasNoticeSince(rows, notice) {
		e.insertNotice(ctx, params, notice)
	}
	return Result{
		Kind:       ResultParked,
		ParkNotice: notice,
		NumTurns:   turn,
		DurationMs: msSince(started),
	}
}

// stubTruncatedToolArgs blanks any tool-call arguments in the completion that
// do not parse as JSON. Licensed only under a stop that cut the message
// mid-generation, where the concatenated fragments cannot form a whole
// document; the strict parse stays in force on every other path. The stubbed
// call still persists and is answered with an instructive error, so nothing
// runs on the partial input.
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

// failed is every exit that could not continue — and the single place the
// engine decides whether "could not continue" means failure at all.
//
// A killed context reaches most of those exits as an ordinary error: the
// store call, the flush, the dispatch that was in flight when the kill landed
// returns one, and its text says "context canceled" rather than anything
// about a cancellation. Classifying that as a failure is not a cosmetic
// mislabel — a failed run discards its workspace snapshot, which is exactly
// the thing a person who just pressed stop wants kept. So the check lives
// here, once, rather than at each exit, where the next one added would
// silently omit it.
//
// The reclassification is one-way and deliberately generous: a genuine
// failure racing a simultaneous stop is recorded as a park. That is the
// better answer either way — parked-open with a workspace beats
// failed-with-the-workspace-thrown-away for a run someone had just asked to
// end.
func (e *Engine) failed(ctx context.Context, started time.Time, turn int, err error) Result {
	if ctx.Err() != nil {
		return e.cancelled(ctx, started, turn)
	}
	return Result{
		Kind:        ResultFailed,
		FailureKind: domain.ConversationFailureAgentError,
		NumTurns:    turn,
		DurationMs:  msSince(started),
		Err:         err,
	}
}

// cancelled reports the context's own error rather than a constant. Every
// caller is already inside a `ctx.Err() != nil` guard, so the value is the
// real cause — and the two causes are not interchangeable to a reader: a
// deadline is the engagement running out of time (something to tune), a
// cancel is somebody stopping it (nothing to tune). The fallback exists only
// so Err is never nil on this Kind, which its doc promises.
func (e *Engine) cancelled(ctx context.Context, started time.Time, turn int) Result {
	cause := ctx.Err()
	if cause == nil {
		cause = context.Canceled
	}
	return Result{
		Kind:       ResultCancelled,
		NumTurns:   turn,
		DurationMs: msSince(started),
		Err:        cause,
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
// call cannot fall back to anything. It is the inference sentinel itself, so
// "this conversation has no credentials" and "this env map names no provider"
// are one class to every caller matching either name.
var ErrNoCredentials = inference.ErrNoCredentials
