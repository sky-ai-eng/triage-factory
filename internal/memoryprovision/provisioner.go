// Package memoryprovision generates the memory a conversation owes when it
// ended without its agent having written one.
//
// A conversation's own agent files `_tfac/memory.md` into conversation_memory
// as it writes it, so an ended conversation with no memory row is exactly one
// whose agent never wrote a memory: a requeue, a takeover, a delegate, a team
// archive, a failure, or a step that concluded without leaving its file. That
// debt blocks the task — nothing new opens on it until the memory lands — so
// something has to settle it, and the transcript is all that settling takes.
// No filesystem, no snapshot, no cross-pod choreography.
//
// Manager is a brain-unit member the same way the fleet reaper and the
// credential provisioner are (internal/app's startBrain/stopBrain), with one
// difference: it is constructed for every brain-capable role INCLUDING local,
// because local is always the brain and the same code runs at N=1.
package memoryprovision

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/memoryentities"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

var log = logging.Component("memoryprovision")

// The three prompts below are this package's own: toolless system-job text for
// the memory generator, kept beside its consumer like every other system job's.
// Local mode sends the combined message; the direct path splits it, with the
// transcript alone in the user turn.

//go:embed prompts/generate-memory.txt
var generateMemoryPrompt string

//go:embed prompts/generate-memory-system.txt
var generateMemorySystemPrompt string

//go:embed prompts/generate-memory-user.txt
var generateMemoryUserPrompt string

// DefaultSweepInterval is the backstop-sweep cadence. Short, and for the same
// reason credprovision.DefaultAwaitingSweepInterval is: it gates a person's
// Send. Without the doorbell, this interval IS the latency from a boundary to
// the memory it owes.
const DefaultSweepInterval = 5 * time.Second

// AttemptTimeout bounds one generation's model call. The system jobs beside
// this one carry no timeout, and could not borrow one: a scoring cycle that
// runs long delays a number on a card, while a generation that runs long is a
// person watching a disabled composer.
const AttemptTimeout = 90 * time.Second

// AttemptBackoff is how recently a conversation must have been attempted for
// the sweep to leave it alone. It is spelled against the newest attempt row's
// started_at rather than held in this process, so it survives a brain restart
// and an attempt whose brain died mid-generation ages out on its own.
const AttemptBackoff = 60 * time.Second

// sweepLimit caps one sweep pass. The next tick takes the next page; a brain
// that has just come up with a fleet's backlog to settle works through it a
// page at a time rather than in one unbounded scan.
const sweepLimit = 100

// nudgeTimeout bounds the whole of a doorbell-driven Fulfil — the model call
// inside it is already capped at AttemptTimeout, so this is the store reads
// around it plus the headroom to not race that cap. Bounded rather than
// background because the doorbell's caller must not be able to leak a
// goroutine per notification.
const nudgeTimeout = 2 * time.Minute

// maxGeneratedTokens caps the model's answer. The memory contract asks for
// 150-300 words; this is several times that, so it bounds a runaway without
// being the thing that truncates a normal one.
const maxGeneratedTokens = 2048

// completer is the narrow slice of *systemllm.Recorder a generation spends
// through — declared here as the shape a caller must satisfy rather than taken
// as the concrete type, so this package's tests answer a completion without a
// provider, a subprocess or a ledger.
type completer interface {
	Complete(ctx context.Context, opts systemllm.CompleteOptions) (*systemllm.CompleteResult, error)
}

// Manager settles the memory debt of every conversation that ended owing one.
//
// inflight is process state, and that is not a shortcut: the brain is one pod,
// so "an attempt is running" is knowable here, and a column for it would be a
// durable claim that a crashed attempt leaves set forever. The durable half of
// the same question — "was one attempted recently" — is the attempt row's
// started_at, which the sweep's own read applies as AttemptBackoff.
type Manager struct {
	stores   db.Stores
	complete completer
	models   systemllm.ModelFunc
	secrets  agentproc.SecretsReader
	llm      func(ctx context.Context, orgID, model string) (map[string]string, error)
	notify   func(orgID, taskID, taskStatus string)

	// attemptTimeout is AttemptTimeout, held as a field so a test can watch
	// the bound fire without waiting out the real one.
	attemptTimeout time.Duration

	mu       sync.Mutex
	inflight map[string]struct{}
}

// NewManager builds a Manager against stores — a brain-capable role's normal
// (secret-bearing) db.Stores, never the disabled-secrets bundle an executor
// holds.
//
// models resolves the org's background-jobs model per attempt; secrets and llm
// are the credential seams every system call takes (llm nil keeps agentproc's
// raw-secret resolution, which is what local passes). notify announces the
// task whose wait just ended; nil is a Manager that generates memory and tells
// nobody, which is only ever a test.
func NewManager(
	stores db.Stores,
	recorder *systemllm.Recorder,
	models systemllm.ModelFunc,
	secrets agentproc.SecretsReader,
	llm func(ctx context.Context, orgID, model string) (map[string]string, error),
	notify func(orgID, taskID, taskStatus string),
) *Manager {
	m := &Manager{
		stores:         stores,
		models:         models,
		secrets:        secrets,
		llm:            llm,
		notify:         notify,
		attemptTimeout: AttemptTimeout,
		inflight:       make(map[string]struct{}),
	}
	// Assigned through the nil check rather than straight onto the interface
	// field: a typed nil in an interface is not nil, and every attempt would
	// then panic where it should report a fault.
	if recorder != nil {
		m.complete = recorder
	}
	return m
}

// Fulfil settles conversationID's memory debt, or establishes that there is
// none. Idempotent and safe to call repeatedly: the fast path (the doorbell),
// the sweep, and a second caller racing either all land here, and all but one
// of them return having written nothing.
//
// It writes exactly one conversation_memory_attempts row per call that reaches
// the transcript, and it returns an error only for a fault the sweep cannot
// help — an unreadable row, a write that failed. Every classified failure of
// the generation itself (no model, provider backoff, the timeout, a provider
// error) lands as an attempt row carrying its error_kind and returns nil,
// because the sweep IS the retry and a returned error would only ask a caller
// to invent a second one.
func (m *Manager) Fulfil(ctx context.Context, orgID, conversationID string) (err error) {
	// Its own root, like the credential provisioner's: the doorbell that
	// usually triggers this is lossy by design and the sweep reaches here with
	// no requester at all, so a link would assert a handoff neither side can
	// guarantee. conversation.id is the join.
	ctx, span := tracer.Start(ctx, "memory.fulfil", trace.WithNewRoot(),
		trace.WithAttributes(telemetry.ConversationID(conversationID), telemetry.OrgID(orgID)))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	conv, err := m.stores.Conversations.GetSystem(ctx, orgID, conversationID)
	if err != nil {
		return fmt.Errorf("memoryprovision: read conversation %s: %w", conversationID, err)
	}
	// Re-checked here rather than trusted from the caller: the doorbell names a
	// conversation an executor believed had ended, and the sweep's page was
	// read before the rows ahead of this one were worked.
	if conv == nil || conv.EndedAt == nil {
		span.SetAttributes(telemetry.Outcome("not_owed"))
		return nil
	}
	mem, err := m.stores.TaskMemory.GetForConversationSystem(ctx, orgID, conversationID)
	if err != nil {
		return fmt.Errorf("memoryprovision: read the memory of conversation %s: %w", conversationID, err)
	}
	if mem != nil {
		span.SetAttributes(telemetry.Outcome("already_settled"))
		return nil
	}

	if !m.claim(conversationID) {
		span.SetAttributes(telemetry.Outcome("in_flight"))
		return nil
	}
	defer m.release(conversationID)

	// The task is read before anything is written, because both writes need it:
	// the entity its memory attaches to, and the status the notify carries. A
	// conversation with no task owes its memory to nothing — the sweep never
	// enumerates one — but the doorbell can still name one, and it gets its row
	// without a join.
	var primaryEntityID, taskID, taskStatus string
	if conv.TaskID != "" {
		task, terr := m.stores.Tasks.GetSystem(ctx, orgID, conv.TaskID)
		if terr != nil {
			return fmt.Errorf("memoryprovision: read task %s of conversation %s: %w", conv.TaskID, conversationID, terr)
		}
		if task != nil {
			primaryEntityID, taskID, taskStatus = task.EntityID, task.ID, task.Status
		}
	}

	rows, err := m.stores.Conversations.ListForAssemblySystem(ctx, orgID, conversationID)
	if err != nil {
		return fmt.Errorf("memoryprovision: read the transcript of conversation %s: %w", conversationID, err)
	}

	// Opened before the branch below, so every way out of this function past
	// here is a row in the ledger. The alternative — opening it only on the
	// paths that spend — leaves the two failures that cost nothing (an
	// unwritable memory row, a conversation with nothing in it) invisible to
	// the one table that exists to explain why a task is still waiting.
	attempt, err := m.stores.MemoryAttempts.BeginAttemptSystem(ctx, orgID, conversationID)
	if err != nil {
		return fmt.Errorf("memoryprovision: open a memory attempt on conversation %s: %w", conversationID, err)
	}

	// Nothing to remember. A conversation that never reached an assistant turn
	// — failed before the model's first turn, cancelled while still queued —
	// has nothing a memory could hold, and generating one anyway produces a
	// paragraph about work that was never started. The empty row settles the
	// debt mechanically: no model call, no wait, and the reads that inject
	// memory skip it on agent_content alone.
	if !hasAssistantTurn(rows) {
		span.SetAttributes(telemetry.Outcome(string(domain.MemoryAttemptEmpty)))
		if ferr := m.file(ctx, orgID, conv, "", domain.MemorySourceNone, primaryEntityID); ferr != nil {
			m.closeAttempt(ctx, orgID, attempt.ID, domain.MemoryAttemptFailed,
				domain.MemoryAttemptErrOther, "the memory row could not be written", "", len(rows), 0)
			return ferr
		}
		m.closeAttempt(ctx, orgID, attempt.ID, domain.MemoryAttemptEmpty, "", "", "", len(rows), 0)
		m.announce(orgID, taskID, taskStatus)
		return nil
	}

	model, err := m.models.Resolve(ctx, orgID)
	switch {
	case errors.Is(err, systemllm.ErrNoModel):
		// Configuration, not a fault: an org admin picks a model and the save
		// rings the doorbell. The wrapped message names the setting, so it is
		// TF's own wording and belongs on the row a person reads.
		span.SetAttributes(telemetry.Outcome(string(domain.MemoryAttemptErrNoModel)))
		log.WarnContext(ctx, "memory generation skipped; no usable background jobs model",
			"org", orgID, "conversation", conversationID, "error", err)
		m.closeAttempt(ctx, orgID, attempt.ID, domain.MemoryAttemptFailed,
			domain.MemoryAttemptErrNoModel, err.Error(), "", len(rows), 0)
		return nil
	case err != nil:
		m.closeAttempt(ctx, orgID, attempt.ID, domain.MemoryAttemptFailed,
			domain.MemoryAttemptErrOther, "the background-jobs model could not be resolved", "", len(rows), 0)
		return fmt.Errorf("memoryprovision: resolve the background jobs model for org %s: %w", orgID, err)
	}

	win := buildWindow(rows)
	// Minted here rather than read back: recording is fire-and-forget by
	// design, so an id this row can name has to be the one handed in.
	systemLLMRunID := uuid.NewString()

	text, kind, reason := m.generate(ctx, orgID, conversationID, model, win, systemLLMRunID)
	switch {
	case kind != "":
		span.SetAttributes(telemetry.Outcome(string(kind)))
		log.WarnContext(ctx, "memory generation failed", "org", orgID, "conversation", conversationID,
			"kind", string(kind), "reason", reason)
		m.closeAttempt(ctx, orgID, attempt.ID, domain.MemoryAttemptFailed,
			kind, reason, systemLLMRunID, win.rowsTotal, win.rowsSent)
		return nil
	case ctx.Err() != nil:
		// The brain stopped mid-attempt. The row keeps completed_at NULL:
		// abandonment is derived from started_at and never stamped, and a
		// verdict written here would claim an answer nobody reached.
		span.SetAttributes(telemetry.Outcome("abandoned"))
		return nil
	}

	content := generatedHeader(win.rowsSent, win.rowsTotal) + "\n\n" + text
	if ferr := m.file(ctx, orgID, conv, content, domain.MemorySourceGenerated, primaryEntityID); ferr != nil {
		m.closeAttempt(ctx, orgID, attempt.ID, domain.MemoryAttemptFailed,
			domain.MemoryAttemptErrOther, "the memory row could not be written", systemLLMRunID, win.rowsTotal, win.rowsSent)
		return ferr
	}
	span.SetAttributes(telemetry.Outcome(string(domain.MemoryAttemptGenerated)))
	m.closeAttempt(ctx, orgID, attempt.ID, domain.MemoryAttemptGenerated, "", "", systemLLMRunID, win.rowsTotal, win.rowsSent)
	m.announce(orgID, taskID, taskStatus)
	return nil
}

// generate spends one completion on win and classifies what came back: the
// memory text on success, or the error_kind and the TF-worded reason a failed
// attempt row carries. The reason is never the upstream body — the row is read
// by a person looking at a task, and a provider's response is neither theirs to
// read nor ours to republish.
//
// A cancelled caller returns ("", "", "") — no text and no classification —
// which Fulfil reads as the attempt nobody closed out.
func (m *Manager) generate(ctx context.Context, orgID, conversationID, model string, win window, systemLLMRunID string) (string, domain.MemoryAttemptErrorKind, string) {
	if m.complete == nil {
		return "", domain.MemoryAttemptErrOther, "no model runtime is wired for memory generation"
	}

	attemptCtx, cancel := context.WithTimeout(ctx, m.attemptTimeout)
	defer cancel()
	res, err := m.complete.Complete(attemptCtx, systemllm.CompleteOptions{
		OrgID:        orgID,
		Job:          systemllm.JobMemory,
		Message:      fmt.Sprintf(generateMemoryPrompt, win.text),
		SystemPrompt: generateMemorySystemPrompt,
		UserMessage:  fmt.Sprintf(generateMemoryUserPrompt, win.text),
		Model:        model,
		MaxTokens:    maxGeneratedTokens,
		TraceID:      "memory-" + conversationID,
		Secrets:      m.secrets,
		LLMResolver:  m.llm,
		RunID:        systemLLMRunID,
		Metadata: map[string]any{
			"conversation_id":   conversationID,
			"window_rows_total": win.rowsTotal,
			"window_rows_sent":  win.rowsSent,
		},
	})

	switch {
	case ctx.Err() != nil:
		// The caller's cancellation, not our budget — tested before the
		// deadline arm below, since our own timeout also leaves attemptCtx
		// expired and only the outer ctx tells the two apart.
		return "", "", ""
	case systemllm.IsProviderBackoff(err):
		return "", domain.MemoryAttemptErrProviderBackoff, "the model provider is in backoff; the next sweep retries"
	case errors.Is(err, context.DeadlineExceeded):
		return "", domain.MemoryAttemptErrTimeout, "memory generation exceeded 90s"
	case err != nil:
		return "", domain.MemoryAttemptErrProviderError, "the model call failed"
	case res == nil || strings.TrimSpace(res.Text) == "":
		return "", domain.MemoryAttemptErrProviderError, "the model returned no memory text"
	}
	return strings.TrimSpace(res.Text), "", ""
}

// generatedHeader is the first line of every generated memory: the reader's
// cue that what follows is a reconstruction rather than the agent's own
// account, and how much of the conversation it was reconstructed from.
// Composed here rather than asked of the model, so it says the same thing
// every time and says it truthfully.
func generatedHeader(rowsSent, rowsTotal int) string {
	return fmt.Sprintf(
		"_Generated by Triage Factory from the conversation transcript (%d of %d rows); the agent did not write its own memory._",
		rowsSent, rowsTotal)
}

// file writes the conversation's memory row and attaches the entities it is
// reachable from. The attach follows every row this package writes, empty ones
// included: an empty row is hidden from the injection reads by its NULL
// content, not by having nowhere to hang.
func (m *Manager) file(ctx context.Context, orgID string, conv *domain.Conversation, content string, source domain.MemorySource, primaryEntityID string) error {
	if _, err := m.stores.TaskMemory.UpsertAgentMemorySystem(ctx, orgID, conv.ID, conv.BlueprintRunID, content, source); err != nil {
		return fmt.Errorf("memoryprovision: file the memory of conversation %s: %w", conv.ID, err)
	}
	if primaryEntityID == "" {
		return nil
	}
	memoryentities.Attach(ctx, m.stores.TaskMemory, m.stores.Artifacts, m.stores.Entities, orgID, conv.ID, primaryEntityID)
	return nil
}

// closeAttempt stamps an attempt's verdict. Best-effort by contract: the work
// it records has already happened, and a task whose memory landed must not read
// as still waiting because the ledger write failed — the next sweep finds the
// memory row and settles nothing further.
func (m *Manager) closeAttempt(ctx context.Context, orgID, attemptID string,
	outcome domain.MemoryAttemptOutcome, kind domain.MemoryAttemptErrorKind,
	reason, systemLLMRunID string, rowsTotal, rowsSent int) {
	if _, err := m.stores.MemoryAttempts.CompleteAttemptSystem(ctx, orgID, attemptID,
		outcome, kind, reason, systemLLMRunID, rowsTotal, rowsSent); err != nil {
		log.Warn("close out memory attempt failed", "attempt", attemptID, "outcome", string(outcome), "error", err)
	}
}

// announce tells the task's watchers its wait is over, so the composer
// re-enables without polling.
func (m *Manager) announce(orgID, taskID, taskStatus string) {
	if m.notify == nil || taskID == "" {
		return
	}
	m.notify(orgID, taskID, taskStatus)
}

// hasAssistantTurn reports whether the model ever spoke in this conversation —
// the whole of what separates a memory worth generating from the empty row.
func hasAssistantTurn(rows []domain.Message) bool {
	for _, r := range rows {
		if r.Role == roleAssistant {
			return true
		}
	}
	return false
}

// claim takes the in-flight slot for conversationID, reporting false when
// another attempt in this process already holds it.
func (m *Manager) claim(conversationID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, busy := m.inflight[conversationID]; busy {
		return false
	}
	m.inflight[conversationID] = struct{}{}
	return true
}

func (m *Manager) release(conversationID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inflight, conversationID)
}
