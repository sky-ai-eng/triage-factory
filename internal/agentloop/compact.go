package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
)

// Compaction replaces a conversation's history with a summary once the
// context window fills. Three arms share one span-selection and one commit:
//
//   - warm (proactive, trip-time): the model itself summarizes, riding the
//     hot cache — the summarize call is byte-identical to the call the loop
//     would otherwise make, plus one appended user message;
//   - forced-shape (resume-time, and the warm arm's failure fallback): an
//     out-of-band call built for compaction — dedicated system prompt,
//     cheapest model, exactly one required tool — that cannot fail on format;
//   - reactive (in the stream error path): a provider context-overflow error
//     is a compaction trigger, not a run failure.
//
// Summary-only replacement, no retained conversational tail: continuity
// lives in summary fidelity, in mechanical re-injection (the pinned mission
// opening, or the original request copied into the result row), and in
// re-derivable working state (the worktree persists). The commit is one
// fenced store transaction; everything the compactor decides is derived from
// rows, never kept as engagement state.

const (
	// DefaultCompactionThreshold is the fraction of the model's window at
	// which the proactive trip fires. High enough that compaction is rare,
	// low enough that the summarize call itself (and one more working turn)
	// still fits comfortably under the hard limit.
	DefaultCompactionThreshold = 0.80

	// DefaultColdCompactionModel is the forced-shape call's model — the
	// cheapest enabled model, which today is always haiku, so it ships as a
	// constant until model configuration makes "cheapest enabled" resolvable.
	DefaultColdCompactionModel = "claude-haiku-4-5"

	// providerCacheTTL mirrors the stamping policy's unset default (the
	// provider's 5-minute ephemeral TTL — see inference/cachecontrol.go).
	// Warmth is derived from the newest assistant row's age against it,
	// never stored. The proxy is deliberately coarse: at a cold resume
	// compaction already pays a full re-read, so a wrong guess changes
	// nothing — which is exactly why this proxy is NOT sufficient for
	// elision, whose entire value is being free.
	providerCacheTTL = 5 * time.Minute
)

// The compactionFail* constants are the machine-readable discriminator values
// settled into the request row's metadata when a warm attempt is discarded.
const (
	compactionFailToolCalls = "tool-calls-in-reply"
	compactionFailNoSummary = "no-parseable-summary"
	compactionFailUnmapped  = "unmappable-reply"
)

// originalRequestCap bounds the verbatim re-injection of a taskless
// conversation's first message into the result row. Beyond it the text is
// cut (the full row is retained in the compacted history) — re-injecting a
// pasted log wholesale would rebuild the bloat compaction just shed.
const originalRequestCap = 4096

// compactionDue reports whether the window is full enough to compact:
// occupancy at or over the threshold fraction of the model's datasheet
// window. An unknown model never trips proactively — the reactive arm still
// protects it.
func (e *Engine) compactionDue(params Params, rows []domain.Message) bool {
	limit, ok := inference.ModelWindow(params.Model)
	if !ok {
		return false
	}
	threshold := params.CompactionThreshold
	if threshold <= 0 {
		threshold = DefaultCompactionThreshold
	}
	return occupancy(rows) >= int(threshold*float64(limit))
}

// occupancy is the newest assistant row's whole-prompt token accounting —
// input + output + cache read + cache creation, which is what the NEXT
// call's prompt approximately costs. Read straight off stamped usage; no
// estimator, ever: an estimate may only place text, never trip machinery.
func occupancy(rows []domain.Message) int {
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if r.Role != "assistant" {
			continue
		}
		total := 0
		for _, p := range []*int{r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheCreationTokens} {
			if p != nil {
				total += *p
			}
		}
		return total
	}
	return 0
}

// warmCacheLive reports whether the provider cache from the newest assistant
// turn is plausibly still alive.
func warmCacheLive(rows []domain.Message) bool {
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Role == "assistant" {
			return time.Since(rows[i].CreatedAt) < providerCacheTTL
		}
	}
	return false
}

// compactOnResume is the claim-start arm, run after repair and before the
// first drain — so the span the summarize call reads is exactly what the
// previous engagement left, and queued input stays queued through it. A
// warm-cache resume (a fast retry) defers to the loop's own trip instead of
// paying a full-price re-read the warm path exists to avoid.
func (e *Engine) compactOnResume(ctx context.Context, params Params) error {
	rows, err := e.Transcript.ListForAssembly(ctx, params.OrgID, params.ConversationID)
	if err != nil {
		return err
	}
	if !e.compactionDue(params, rows) {
		return nil
	}
	if warmCacheLive(rows) {
		return nil
	}
	return e.compactCold(ctx, params)
}

// compactWindowFull is the reactive arm's dispatcher: warm semantics while
// the prior call's cache is live, forced-shape otherwise.
//
// It serves both ways a full window announces itself — the provider rejecting
// the request outright, and the provider accepting it and stopping
// mid-generation at the wall. The two differ only in whether a partial
// message came back with the news; what they say about the transcript, and
// therefore the remedy, is identical.
func (e *Engine) compactWindowFull(ctx context.Context, params Params) error {
	rows, err := e.Transcript.ListForAssembly(ctx, params.OrgID, params.ConversationID)
	if err != nil {
		return err
	}
	if warmCacheLive(rows) {
		return e.compactWarm(ctx, params)
	}
	return e.compactCold(ctx, params)
}

// compactWarm runs the model's own summarize turn on the hot cache.
//
// One attempt, ever. The request inherits the working system prompt and the
// live tools — it must, the cache is keyed on them — so the model frequently
// does not behave like a summarizer (~1/3 observed in the reference
// implementation): it answers with tool calls, or without a parseable
// summary. Both are failure, both are expected, and both hand off to the
// forced-shape call rather than re-asking — a model that ignored the request
// once will likely ignore it again, and a retry burns latency on a cache
// that is about to be invalidated anyway.
func (e *Engine) compactWarm(ctx context.Context, params Params) error {
	rows, err := e.Transcript.ListForAssembly(ctx, params.OrgID, params.ConversationID)
	if err != nil {
		return err
	}

	// Re-entry after a crash (a delivered request with no reply) reuses the
	// standing ask; otherwise insert one. Inserted DELIVERED, not pending:
	// the row is in the transcript atomically, other pending input stays
	// queued through the compaction (the solo-flush contract — bundling it
	// into the summarize call would compact it into the summary instead of
	// letting it survive as live input), and there is no pending window for
	// a crash to leave half-entered.
	reqRow := unansweredCompactionRequest(rows)
	if reqRow == nil {
		row := domain.Message{
			ConversationID: params.ConversationID,
			UserID:         params.UserID,
			Role:           "user",
			Subtype:        domain.MessageSubtypeInjectionCompactionRequest,
			Content:        warmCompactionRequest,
		}
		if _, err := e.Transcript.Insert(ctx, params.OrgID, &row); err != nil {
			return fmt.Errorf("insert compaction request: %w", err)
		}
		rows = append(rows, row)
		reqRow = &row
	}

	provider, client, release, err := e.Credentials.ForCall(ctx)
	if err != nil {
		return err
	}
	callStarted := time.Now()
	// The request-invariance contract: this is buildRequest — the same
	// constructor the working loop calls, same system prompt, same tools,
	// same tool_choice (absent), same model, same effort — over the same
	// transcript plus the one request row. The only cache invalidation in
	// the whole compaction process is the commit below.
	completion, err := e.streamWithRetry(ctx, client, e.buildRequest(params, provider, rows, callMaxTokens(params, provider)))
	if release != nil {
		release()
	}
	if err != nil {
		if errors.Is(err, inference.ErrContextOverflow) {
			// The ask itself did not fit; the cache is forfeit regardless now.
			return e.compactCold(ctx, params)
		}
		return err
	}

	summary, failReason := judgeWarmReply(completion)
	if failReason != "" {
		// Discarded, never inserted: a botched summarize attempt is not a
		// conversation event — no tool call in it is ever dispatched and no
		// dangling tool_use can exist. Its dollars are real, so they settle
		// on the request row, the one transcript row this attempt owns.
		var costUSD *float64
		if cost, ok := inference.CostForUsage(modelForRow(completion, params), completion.Usage); ok {
			costUSD = &cost
		}
		u := completion.Usage
		if err := e.Transcript.SettleCompactionRequest(ctx, params.OrgID, params.ConversationID,
			reqRow.ID, u.NonCachedInputTokens(), u.OutputTokens, u.CacheReadTokens, u.CacheCreationTokens,
			costUSD, failReason); err != nil {
			return fmt.Errorf("settle failed compaction attempt: %w", err)
		}
		e.info("warm compaction attempt failed; handing off to the forced-shape call",
			"conversation", params.ConversationID, "reason", failReason)
		return e.compactCold(ctx, params)
	}

	// Success: the reply is a genuine transcript event — persist it like any
	// assistant turn (usage, cost, duration), then flip it inactive with the
	// span it summarized.
	replyRow, err := e.persistAssistant(ctx, params, completion, msSince(callStarted))
	if err != nil {
		return err
	}
	span := append(compactionSpan(rows, params.MissionAnchored), replyRow.ID)
	return e.commitCompaction(ctx, params, rows, nil, summary, span)
}

// compactCold is the forced-shape call: nothing inherited, nothing that can
// fail on format. A dedicated system prompt, the cheapest enabled model, and
// exactly one tool the call is forced to use — the schema is the parse.
func (e *Engine) compactCold(ctx context.Context, params Params) error {
	rows, err := e.Transcript.ListForAssembly(ctx, params.OrgID, params.ConversationID)
	if err != nil {
		return err
	}
	model := params.ColdCompactionModel
	if model == "" {
		model = DefaultColdCompactionModel
	}

	provider, client, release, err := e.Credentials.ForCall(ctx)
	if err != nil {
		return err
	}
	callStarted := time.Now()
	// Rows assemble with undelivered rows excluded (the default), so queued
	// input is never summarized; the pinned opening rides along as context
	// in the anchored arm and is simply excluded from the span below.
	completion, err := e.streamWithRetry(ctx, client, inference.Request{
		Provider:     provider,
		Model:        model,
		SystemPrompt: coldCompactionSystemPrompt,
		Rows:         rows,
		Tools:        []schemas.ChatTool{compactionTool()},
		ToolChoice:   forcedCompactionChoice(),
		// The cap is resolved for the SUMMARIZE model, not the
		// conversation's: this call's whole output is one tool call carrying
		// the summary, and a summary cut in half is the only failure this
		// path has no fallback beneath.
		MaxTokens: inference.MaxOutputTokens(provider, model),
	})
	if release != nil {
		release()
	}
	if err != nil {
		return fmt.Errorf("forced-shape compaction call: %w", err)
	}
	analysis, summary, err := parseForcedCompaction(completion)
	if err != nil {
		return err
	}

	// The path-independent artifact: reconstruct the reply row a successful
	// warm attempt would have left, byte-shape identical, honestly
	// attributed to the model that produced it, inactive from the moment it
	// exists. Cost lands on the row carrying the call's output.
	durationMs := msSince(callStarted)
	replyRow := &domain.Message{
		// Sanitized like every persisted model text: the analysis and
		// summary arrive raw from the tool-call arguments, and a summary
		// that quotes command output can carry the NUL bytes Postgres
		// cannot store — inside the fenced commit, one would fail the
		// whole compaction.
		Content:  sanitizeForStore(canonicalCompactionArtifact(analysis, summary)),
		Role:     "assistant",
		Model:    coldModelForRow(completion, model),
		Metadata: map[string]any{"compaction_path": "forced"},
	}
	stampUsage(replyRow, completion.Usage)
	stampFinishReason(replyRow, completion.FinishReason)
	if durationMs > 0 {
		replyRow.DurationMs = &durationMs
	}
	if cost, ok := inference.CostForUsage(replyRow.Model, completion.Usage); ok {
		replyRow.CostUSD = &cost
	}

	span := compactionSpan(rows, params.MissionAnchored)
	return e.commitCompaction(ctx, params, rows, replyRow, summary, span)
}

// commitCompaction composes the result row and commits the whole compaction
// through the transcript's single fenced transaction.
func (e *Engine) commitCompaction(ctx context.Context, params Params, rows []domain.Message, replyRow *domain.Message, summary string, span []int) error {
	resultRow := composeResultRow(params, rows, summary)
	if err := e.Transcript.Compact(ctx, params.OrgID, params.ConversationID, replyRow, resultRow, span); err != nil {
		return fmt.Errorf("commit compaction: %w", err)
	}
	e.info("conversation compacted",
		"conversation", params.ConversationID, "rows_compacted", len(span), "result_row", resultRow.ID)
	return nil
}

// compactionSpan selects the row ids a compaction supersedes: every
// delivered row in the current window, minus the pinned opening on a
// mission-anchored conversation. Undelivered rows never flip — the model has
// not seen them, and they survive as live queued input ordered after the
// result row by the commit's re-seq.
//
// The pin is the leading run of rows before the first assistant turn OR the
// first compaction result, whichever comes first. The second bound is what
// keeps summaries from accumulating: after one compaction the window opens
// with [mission..., result], and pinning "everything before the first
// assistant row" would grandfather the old summary into every window that
// follows. The mission is the only text with a standing claim to survival.
func compactionSpan(rows []domain.Message, anchored bool) []int {
	start := 0
	if anchored {
		for i, r := range rows {
			if r.Role == "assistant" || r.Subtype == domain.MessageSubtypeInjectionCompactionResult {
				break
			}
			start = i + 1
		}
	}
	var ids []int
	for _, r := range rows[start:] {
		if !isDelivered(r) {
			continue
		}
		ids = append(ids, r.ID)
	}
	return ids
}

// unansweredCompactionRequest finds a delivered compaction request with no
// assistant reply after it — the derived "compaction in flight" state a
// crash between the ask and the commit leaves behind. The transcript is the
// state.
func unansweredCompactionRequest(rows []domain.Message) *domain.Message {
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if r.Role == "assistant" {
			return nil
		}
		if r.Role == "user" && r.Subtype == domain.MessageSubtypeInjectionCompactionRequest && isDelivered(r) {
			rr := r
			return &rr
		}
	}
	return nil
}

// judgeWarmReply classifies a warm attempt: the extracted summary on
// success, or the failure reason. A reply carrying tool calls failed even if
// it also happens to contain summary-shaped text — the model kept working,
// and those calls must never run.
func judgeWarmReply(completion *inference.Completion) (summary, failReason string) {
	row, err := inference.MessageToRow(completion.Message)
	if err != nil {
		return "", compactionFailUnmapped
	}
	if len(row.ToolCalls) > 0 {
		return "", compactionFailToolCalls
	}
	summary, ok := extractSummary(row.Content)
	if !ok {
		return "", compactionFailNoSummary
	}
	return summary, ""
}

// extractSummary pulls the <summary> block out of a warm reply. First open
// tag to last close tag, so a summary that itself quotes the tags still
// parses. Kept as a small seam: a JSON arm lands here when a non-Claude
// family does.
func extractSummary(text string) (string, bool) {
	start := strings.Index(text, "<summary>")
	end := strings.LastIndex(text, "</summary>")
	if start < 0 || end < 0 || end <= start {
		return "", false
	}
	body := strings.TrimSpace(text[start+len("<summary>") : end])
	if body == "" {
		return "", false
	}
	return body, true
}

// composeResultRow builds the machine-composed row that replaces the span —
// the only compaction text that enters the active window. The summary is the
// model's; everything around it is mechanical, so the objective can never
// depend on the model having preserved it: the anchored arm keeps the
// mission rows themselves, and the unanchored arm carries the original
// request verbatim right here.
func composeResultRow(params Params, rows []domain.Message, summary string) *domain.Message {
	var b strings.Builder
	b.WriteString(compactionPreamble)
	if !params.MissionAnchored {
		if orig, ok := originalRequest(rows); ok {
			b.WriteString("\n\n<original_request>\n" + orig + "\n</original_request>")
		}
	}
	b.WriteString("\n\n<summary>\n" + summary + "\n</summary>")
	return &domain.Message{
		ConversationID: params.ConversationID,
		Role:           "user",
		Subtype:        domain.MessageSubtypeInjectionCompactionResult,
		// Sanitized here rather than upstream: the warm summary is extracted
		// from the raw completion (persistAssistant sanitizes its own copy,
		// not this one), and a summary quoting binary command output would
		// otherwise fail the fenced commit's insert on Postgres.
		Content: sanitizeForStore(b.String()),
	}
}

// originalRequest returns the conversation's first genuine human message,
// capped — content is retained on the inactive row either way, so the cut
// loses nothing recoverable.
func originalRequest(rows []domain.Message) (string, bool) {
	for _, r := range rows {
		if r.Role != "user" || !IsHumanInput(r) {
			continue
		}
		text := r.Content
		if len(text) > originalRequestCap {
			cut := originalRequestCap
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
			text = text[:cut] + fmt.Sprintf("\n[truncated — the original message was %d bytes; the full text remains in the compacted history]", len(r.Content))
		}
		return text, true
	}
	return "", false
}

// canonicalCompactionArtifact renders the forced-shape call's output in the
// byte shape a successful warm reply has, so every compaction leaves one
// uniform artifact regardless of which path produced it.
func canonicalCompactionArtifact(analysis, summary string) string {
	return "<analysis>\n" + analysis + "\n</analysis>\n\n<summary>\n" + summary + "\n</summary>"
}

// coldModelForRow prefers the served model the provider reported, same rule
// as modelForRow on the conversational path.
func coldModelForRow(completion *inference.Completion, requested string) string {
	if completion.Model != "" {
		return completion.Model
	}
	return requested
}

// compactionToolName is the forced-shape call's single tool.
const compactionToolName = "record_compaction"

// compactionTool is the one tool the forced-shape call exposes. The schema
// IS the parse: two required strings, nothing else, so there is no format
// failure mode and no retry loop.
func compactionTool() schemas.ChatTool {
	props := schemas.NewOrderedMap()
	analysis := schemas.NewOrderedMap()
	analysis.Set("description", "Your reasoning about the state of the work: the goal, what happened, what was learned, what is unfinished. Scratch space — it is kept for the record but not shown to the continuing agent.")
	analysis.Set("type", "string")
	props.Set("analysis", analysis)
	summary := schemas.NewOrderedMap()
	summary.Set("description", "The replacement summary, following the section structure in your instructions. This is the only account of the conversation the continuing agent will see.")
	summary.Set("type", "string")
	props.Set("summary", summary)

	description := "Record the compaction of the conversation. Call exactly once."
	return schemas.ChatTool{
		Type: schemas.ChatToolTypeFunction,
		Function: &schemas.ChatToolFunction{
			Name:        compactionToolName,
			Description: &description,
			Parameters: &schemas.ToolFunctionParameters{
				Type:       "object",
				Properties: props,
				Required:   []string{"analysis", "summary"},
			},
		},
	}
}

// forcedCompactionChoice pins tool_choice to the compaction tool. Legal here
// and only here: this call has already forfeited the cache, which is the one
// reason the conversational path must never set tool_choice.
func forcedCompactionChoice() *schemas.ChatToolChoice {
	return &schemas.ChatToolChoice{
		ChatToolChoiceStruct: &schemas.ChatToolChoiceStruct{
			Type:     schemas.ChatToolChoiceTypeFunction,
			Function: &schemas.ChatToolChoiceFunction{Name: compactionToolName},
		},
	}
}

// parseForcedCompaction reads the forced tool call's two arguments. The
// choice above makes a missing or malformed call a provider contract
// violation, so failing loudly is correct — there is deliberately no
// fallback beneath the path that IS the fallback.
func parseForcedCompaction(completion *inference.Completion) (analysis, summary string, err error) {
	assistant := completion.Message.ChatAssistantMessage
	if assistant == nil || len(assistant.ToolCalls) == 0 {
		return "", "", errors.New("forced-shape compaction call returned no tool call")
	}
	for _, call := range assistant.ToolCalls {
		name := ""
		if call.Function.Name != nil {
			name = *call.Function.Name
		}
		if name != compactionToolName {
			continue
		}
		var args struct {
			Analysis string `json:"analysis"`
			Summary  string `json:"summary"`
		}
		if uerr := json.Unmarshal([]byte(call.Function.Arguments), &args); uerr != nil {
			return "", "", fmt.Errorf("forced-shape compaction arguments did not parse: %w", uerr)
		}
		if strings.TrimSpace(args.Summary) == "" {
			return "", "", errors.New("forced-shape compaction call produced an empty summary")
		}
		return args.Analysis, args.Summary, nil
	}
	return "", "", fmt.Errorf("forced-shape compaction call answered with the wrong tool")
}

// --- Prompt text ---
//
// These live with the compactor (the system-job rule: next to the consumer)
// and are deliberately surface-neutral: no branches, no
// pull requests, no blueprints — any conversation type can adopt this
// compactor unchanged.

// compactionSections is the summary's section contract, shared verbatim by
// both prompts so the two paths produce the same document.
const compactionSections = `1. Progress — what has been accomplished and where the work stands.
2. User messages — every message the user sent, listed near-verbatim. Do not paraphrase them into a gestalt.
3. Unactioned instructions — anything the user asked for that is not yet fully done, quoted verbatim.
4. Information gathered — key findings, preserving exact values that would be hard to recover: file paths, identifiers, URLs, error messages, command output, code fragments.
5. Approaches tried — what worked, and what failed with why. Mark failed approaches "do not retry".
6. Decisions — choices already settled that must not be relitigated, with their reasons.
7. Current work — precisely what was in flight at this moment: which files, which symbols, what the in-progress change was.
8. Next step — the concrete immediate action a continuation should take.`

// warmCompactionRequest is the ask appended to the live transcript. It
// renders bare on the wire — the subtype is provenance, not an envelope —
// and must redirect a model that is mid-task with its working tools still
// in hand, which is why it opens by forbidding them.
const warmCompactionRequest = `<system-note kind="compaction">
STOP. Do not call any tools and do not continue the task in this reply.

This conversation has nearly filled its context window and is being compacted: everything before this message will be REPLACED by the summary you write now. Information you do not carry into the summary is lost forever. There is no one to ask for clarification — be thorough.

First, think through the state of the work inside <analysis> tags. The analysis is scratch space and does not survive; only the summary does.

Then write the replacement summary inside <summary> tags, with these sections:

` + compactionSections + `

Do not restate the original request or mission — it is preserved separately and will accompany this summary. Reply with the analysis and the summary only.
</system-note>`

// coldCompactionSystemPrompt is the forced-shape call's entire system
// prompt: the model's only job, the one tool, and the same section contract.
const coldCompactionSystemPrompt = `You are a compaction assistant. The conversation you are given is a working agent's transcript that has outgrown its context window. Your only job is to produce the summary that will REPLACE that history for the agent that continues the work. Information you do not carry into the summary is lost forever, and there is no one to ask for clarification — be thorough.

Call the ` + compactionToolName + ` tool exactly once. Put your reasoning about the state of the work in the analysis argument (scratch space; the continuing agent never sees it). Put the replacement summary in the summary argument, with these sections:

` + compactionSections + `

Do not restate the original request or mission in the summary — it is preserved separately and will accompany it.`

// compactionPreamble opens the result row — the only compaction text that
// enters the active window.
const compactionPreamble = `<system-note kind="compaction">
This conversation ran long and was compacted. The summary below replaces the prior message history; continue the work from it.
</system-note>`
