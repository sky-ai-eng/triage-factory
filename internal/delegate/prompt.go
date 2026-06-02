// Prompt resolution + composition (mission text + envelope template +
// placeholder interpolation), plus parsing of the agent's terminal
// completion JSON envelope and the small string utilities the prompt
// path needs (extra-tools merging, owner/repo splitting).

package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
)

// Sentinel errors the delegate HTTP handler uses to map prompt-resolution
// failures to 4xx instead of 5xx.
var (
	// ErrPromptNotFound — Delegate's caller passed a prompt id that
	// doesn't resolve to any row. Race-correctable (the prompt was
	// deleted between snapshot fetch and drop, or the id was simply
	// wrong) — 400 Bad Request, not 5xx.
	ErrPromptNotFound = errors.New("delegate: prompt not found")

	// ErrPromptUnspecified — Delegate's caller passed an empty prompt
	// id. The picker should have prevented this; 400 Bad Request when
	// the contract is violated.
	ErrPromptUnspecified = errors.New("delegate: no prompt specified")

	// ErrBlueprintNotFound — Delegate's caller passed a blueprint id that
	// doesn't resolve to any row. Race-correctable — 400 Bad Request.
	ErrBlueprintNotFound = errors.New("delegate: blueprint not found")

	// ErrBlueprintUnspecified — Delegate's caller passed an empty blueprint
	// id. The picker / trigger should have prevented this; 400 Bad Request.
	ErrBlueprintUnspecified = errors.New("delegate: no blueprint specified")
)

// resolveBlueprint finds the blueprint to fire from an explicit blueprint ID.
// Manual delegation requires the caller to pick a blueprint; auto-delegation
// supplies the blueprint_id from the trigger row. Routing splits on
// triggerType to honor RLS exactly as resolvePrompt does: "manual" loads via
// the app pool under the user's synthetic claims (so a caller can't run a
// blueprint they can't see); "event" stays on the admin pool (the router is a
// system actor with no user identity).
func (s *Spawner) resolveBlueprint(orgID, explicitBlueprintID, triggerType, creatorUserID string) (*domain.Blueprint, error) {
	if explicitBlueprintID == "" {
		return nil, fmt.Errorf("%w — select one from the picker", ErrBlueprintUnspecified)
	}
	var (
		b   *domain.Blueprint
		err error
	)
	if triggerType == "manual" {
		err = s.tx.SyntheticClaimsWithTx(context.Background(), orgID, creatorUserID, func(ts db.TxStores) error {
			got, gErr := ts.Blueprints.Get(context.Background(), orgID, explicitBlueprintID)
			if gErr != nil {
				return gErr
			}
			b = got
			return nil
		})
	} else {
		b, err = s.blueprints.GetSystem(context.Background(), orgID, explicitBlueprintID)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load blueprint %s: %w", explicitBlueprintID, err)
	}
	if b == nil {
		return nil, fmt.Errorf("%w: %s", ErrBlueprintNotFound, explicitBlueprintID)
	}
	return b, nil
}

// resolveBlueprintSteps loads a blueprint's ordered step list under the pool
// the trigger type dictates (same RLS rationale as resolveBlueprint).
func (s *Spawner) resolveBlueprintSteps(orgID, blueprintID, triggerType, creatorUserID string) ([]domain.BlueprintStep, error) {
	var (
		steps []domain.BlueprintStep
		err   error
	)
	if triggerType == "manual" {
		err = s.tx.SyntheticClaimsWithTx(context.Background(), orgID, creatorUserID, func(ts db.TxStores) error {
			got, gErr := ts.Blueprints.ListSteps(context.Background(), orgID, blueprintID)
			steps = got
			return gErr
		})
	} else {
		steps, err = s.blueprints.ListStepsSystem(context.Background(), orgID, blueprintID)
	}
	return steps, err
}

// resolvePrompt finds the prompt for a task from an explicit prompt ID.
// Manual delegation always requires the caller to pick a prompt; auto-
// delegation supplies the prompt_id from the trigger row.
//
// Routing splits on triggerType to honor the prompts_select RLS policy:
//
//   - triggerType == "event": router-fired runs have no user identity to
//     check against. Stay on the admin pool (GetSystem) — the event
//     subscriber is a system actor.
//   - triggerType == "manual": load via the app pool under the
//     requesting user's synthetic claims so prompts_select filters out
//     prompts the user can't see. Without this, a caller could supply a
//     guessed prompt_id belonging to another user's private prompt and
//     the agent would run under it.
//
// In SQLite (local mode), SyntheticClaimsWithTx is essentially a pass-
// through (asserts orgID == LocalDefaultOrg, ignores userID), so the
// "manual" branch resolves identically to the "event" branch. The
// routing only changes behavior under Postgres + RLS.
//
// creatorUserID is required when triggerType == "manual"; ignored for
// "event" since the admin pool doesn't read JWT claims.
func (s *Spawner) resolvePrompt(orgID string, task domain.Task, explicitPromptID, triggerType, creatorUserID string) (*domain.Prompt, error) {
	if explicitPromptID == "" {
		return nil, fmt.Errorf("%w — select one from the prompt picker", ErrPromptUnspecified)
	}

	var (
		p   *domain.Prompt
		err error
	)
	if triggerType == "manual" {
		err = s.tx.SyntheticClaimsWithTx(context.Background(), orgID, creatorUserID, func(ts db.TxStores) error {
			got, gErr := ts.Prompts.Get(context.Background(), orgID, explicitPromptID)
			if gErr != nil {
				return gErr
			}
			p = got
			return nil
		})
	} else {
		p, err = s.prompts.GetSystem(context.Background(), orgID, explicitPromptID)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load prompt %s: %w", explicitPromptID, err)
	}
	if p == nil {
		return nil, fmt.Errorf("%w: %s", ErrPromptNotFound, explicitPromptID)
	}
	return p, nil
}

// buildPrompt composes: mission + envelope (scope, tools, task memory, completion contract).
// buildPrompt composes mission + envelope and interpolates all placeholders
// in one pass. See placeholders.go for the full catalog — every {{X}} in
// the mission or envelope gets resolved here, with unknown names falling
// through as literal braces so they're obvious to prompt authors on first
// run. metadataJSON is the primary event's metadata blob ("" is fine —
// event-derived placeholders just render empty).
func buildPrompt(task domain.Task, metadataJSON, mission, scope, toolsRef, binaryPath, runID string) string {
	// Compatibility shim: some early prompts were written with the literal
	// "triagefactory exec" prefix on CLI invocations, assuming the binary
	// was on PATH. The binary lives at an absolute path in the worktree
	// session, so rewrite those before interpolation. New prompts should
	// use {{BINARY_PATH}} directly.
	body := strings.ReplaceAll(mission, "triagefactory exec", binaryPath+" exec")
	full := body + "\n\n" + ai.EnvelopeTemplate
	return BuildPromptReplacer(task, metadataJSON, runID, binaryPath, scope, toolsRef).Replace(full)
}

func (s *Spawner) cachedAgentTools() string {
	s.agentToolsOnce.Do(func() {
		s.agentToolsCache = skills.ScanAgentTools()
	})
	return s.agentToolsCache
}

// collectExtraTools merges a prompt's declared allowed_tools with tools
// discovered from agent definitions (~/.claude/agents/*.md).
func (s *Spawner) collectExtraTools(promptAllowedTools string) string {
	agentTools := s.cachedAgentTools()
	if promptAllowedTools == "" && agentTools == "" {
		return ""
	}
	return skills.NormalizeToolList(promptAllowedTools + "," + agentTools)
}

type agentResult struct {
	// Outcome is the single terminal vocabulary (continue|finish|abort|
	// yield) — renamed from the legacy `status` field for clarity. See
	// domain.RunOutcome and internal/ai/prompts/envelope.txt.
	Outcome string `json:"outcome"`
	// Summary is the always-present natural-language "what I did". Maps
	// to runs.result_summary.
	Summary string `json:"summary"`
	// Reason is the natural-language "why I stopped / what a human needs
	// to do" — populated only on an abort outcome. Maps to
	// runs.outcome_reason, kept distinct from Summary.
	Reason string         `json:"reason"`
	Links  map[string]any `json:"links"` // keyed URLs (pr_review, pr, jira_issues)

	// Yield is populated when Outcome == "yield". The agent is asking
	// the user a question and the run should park in awaiting_input
	// rather than completing. See domain.YieldRequest and SKY-139 /
	// internal/ai/prompts/envelope.txt for the agent-facing contract.
	Yield *domain.YieldRequest `json:"yield,omitempty"`
}

// isValid reports whether the parsed envelope contains enough to act on.
// Two terminal shapes are accepted:
//   - continue / finish / abort: Summary is non-empty (every successful
//     or stopped envelope carries a natural-language summary)
//   - yield: a well-formed yield envelope (see isYield)
//
// Anything else is treated as "didn't parse cleanly" — the parser
// falls through to its markdown-fence and brace-extraction paths
// before giving up. Rejecting malformed yield payloads at parse time
// matters because once a yield parks the run in awaiting_input, the
// user can't respond unless the modal can render meaningfully —
// e.g. a choice yield with no options has no buttons to click.
func (r *agentResult) isValid() bool {
	if r.Summary != "" {
		return true
	}
	return r.isYield()
}

// isYield reports whether this is a well-formed yield envelope: outcome
// "yield" AND a yield payload that passes YieldRequest.Validate (known type,
// non-empty message, well-formed options for choice yields, no duplicate
// option ids). Both the run-completion yield routing and hasValidOutcome
// gate on it so a "yield" carrying a missing/malformed payload is never
// (a) parked in awaiting_input with a modal the user can't act on, nor
// (b) waved through the outcome gate and then treated as a normal completion
// that closes the task. A non-empty summary does NOT make a payload-less
// yield valid — the payload is what the UI needs.
func (r *agentResult) isYield() bool {
	return r.Outcome == string(domain.RunOutcomeYield) && r.Yield != nil && r.Yield.Validate() == nil
}

// hasValidOutcome reports whether the envelope carries a usable outcome.
// continue/finish need only the outcome token. abort additionally requires a
// non-empty reason: abort is the agent's *voluntary* "I'm functioning but
// choosing to stop" decision (involuntary death is the separate `failed`
// status), so the why is exactly the thing worth collecting — a reasonless
// abort is re-prompted by the outcome gate. yield requires a well-formed
// payload (isYield) — otherwise a payload-less "yield" would satisfy the gate
// and then fall through to the normal completion path. Looser than isValid
// only in that a summary alone never substitutes for a missing/incomplete
// outcome: a summary-only envelope parses but trips the gate.
func (r *agentResult) hasValidOutcome() bool {
	switch domain.RunOutcome(r.Outcome) {
	case domain.RunOutcomeContinue, domain.RunOutcomeFinish:
		return true
	case domain.RunOutcomeAbort:
		return r.Reason != ""
	case domain.RunOutcomeYield:
		return r.isYield()
	default:
		return false
	}
}

// PrimaryLink returns the most relevant URL from the result.
func (r *agentResult) PrimaryLink() string {
	for _, key := range []string{"pr_review", "pr"} {
		if v, ok := r.Links[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	if v, ok := r.Links["jira_issues"]; ok {
		if arr, ok := v.([]any); ok && len(arr) > 0 {
			if s, ok := arr[0].(string); ok {
				return s
			}
		}
	}
	return ""
}

// parseAgentResult extracts the structured {outcome, summary, reason, links}
// JSON from the agent's final message. Handles markdown fences, leading/
// trailing text. Recognizes both completion envelopes (outcome: continue |
// finish | abort with a non-empty summary) and yield envelopes (outcome:
// yield with a typed yield payload — SKY-139). See agentResult.isValid for
// the acceptance rule.
func parseAgentResult(text string) *agentResult {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	var result agentResult
	if json.Unmarshal([]byte(text), &result) == nil && result.isValid() {
		return &result
	}

	stripped := text
	if idx := strings.Index(stripped, "```"); idx >= 0 {
		stripped = stripped[idx+3:]
		if nl := strings.Index(stripped, "\n"); nl >= 0 {
			stripped = stripped[nl+1:]
		}
		if end := strings.LastIndex(stripped, "```"); end >= 0 {
			stripped = stripped[:end]
		}
		stripped = strings.TrimSpace(stripped)
		if json.Unmarshal([]byte(stripped), &result) == nil && result.isValid() {
			return &result
		}
	}

	if start := strings.Index(text, "{"); start >= 0 {
		if end := strings.LastIndex(text, "}"); end > start {
			candidate := text[start : end+1]
			if json.Unmarshal([]byte(candidate), &result) == nil && result.isValid() {
				return &result
			}
		}
	}

	return nil
}

func parseOwnerRepo(s string) (string, string) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}
