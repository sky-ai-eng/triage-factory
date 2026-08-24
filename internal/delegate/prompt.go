// Prompt resolution + composition (mission text, the run's per-run tail, and
// the composed agent framework prompt), plus parsing of the agent's terminal
// completion JSON envelope and the small string utilities the prompt
// path needs (extra-tools merging, owner/repo splitting).

package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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
// through (asserts orgID == LocalDefaultOrgID, ignores userID), so the
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

// machinistSpec selects the delegated agent's block set from
// internal/agentprompt. Mode is resolved here rather than inside Build, which
// stays a pure function of its Spec — that is what makes both mode variants
// testable without manipulating process state.
func machinistSpec() agentprompt.Spec {
	return agentprompt.Spec{
		Surface: agentprompt.SurfaceMachinist,
		Runtime: agentprompt.RuntimeSDK,
		Family:  agentprompt.FamilyClaude,
		Mode:    agentprompt.ModeFor(string(runmode.Current())),
	}
}

// buildPrompt composes what the SDK runtime says first: the task context, the
// mission, this run's own facts, its verb reference, then the framework prompt —
// whose completion contract is last, so it is the final thing the model reads.
//
// Each section is composed on its own. BuildTaskContext is built entirely from
// external data — PR titles, ticket fields, chat messages — and is the one
// section resolveCLIPath never runs over; joining first and rewriting after
// would put a rewrite over attacker-influenced text for no gain.
//
// metadataJSON is the primary event's metadata blob ("" is fine — the context
// block just carries no event fields). skeleton is the rendered PR history
// block, empty for a task with no pull request behind it. knowledge is the
// manifest of what this launch staged under _tfac/knowledge/, empty when it
// staged nothing.
func buildPrompt(task domain.Task, metadataJSON, skeleton, mission, scope, toolsRef, binaryPath, runRoot, branchTemplate, runURL, knowledge string) string {
	cli := func(s string) string { return resolveCLIPath(s, binaryPath) }
	return joinSections(
		BuildTaskContext(task, metadataJSON, skeleton),
		cli(mission),
		runContext(scope, runRoot, branchTemplate, runURL, knowledge),
		cli("<tools>\n"+strings.TrimSpace(toolsRef)+"\n</tools>"),
		cli(agentprompt.Build(machinistSpec())),
	)
}

// resolveCLIPath points every `triagefactory exec …` invocation at the binary
// this run can actually execute: the host binary's own path in local mode, the
// canonical bind-mount inside the jail. The allowlist
// (agentproc.BuildAllowedTools) grants that absolute path and nothing else, so
// an unresolved invocation is a denied tool call rather than a cosmetic miss.
//
// Never run it over externally-authored text.
func resolveCLIPath(text, binaryPath string) string {
	return strings.ReplaceAll(text, "triagefactory exec", binaryPath+" exec")
}

// runContext renders what the framework prompt refers to but cannot contain:
// the facts of this particular run. agentprompt.Build is byte-identical for a
// fixed Spec so the fleet shares one cached prefix, which leaves anything that
// differs between two runs — what this one is pointed at, where its tree is,
// what the team calls its branches — to arrive here instead.
//
// System-authored, so unlike the task context it carries no untrusted markers.
// An absent fact renders no line: a deployment with no public URL says nothing
// about a URL rather than offering a blank one.
//
// The knowledge manifest arrives here for exactly that reason: WHICH documents
// this org happens to hold today is a fact about one run, and the framework
// blocks that tell the agent knowledge exists have to stay byte-identical
// across runs to remain a cacheable prefix. It renders last and as its own
// block, because it is a listing rather than a line — and only when something
// was actually staged, so a run with no knowledge behind it says nothing about
// knowledge at all.
func runContext(scope, runRoot, branchTemplate, runURL, knowledge string) string {
	var lines []string
	if s := strings.TrimSpace(scope); s != "" {
		lines = append(lines, s)
	}
	if runRoot != "" {
		lines = append(lines, "Run root: "+runRoot+
			" — every `_tfac/...` path in your instructions sits directly under this directory."+
			" Reach it by the absolute path, so it resolves from whatever directory you have cd'd into.")
	}
	if branchTemplate != "" {
		lines = append(lines, "Branch naming convention for this team: "+branchTemplate)
	}
	if runURL != "" {
		lines = append(lines, "Run URL: "+runURL)
	}
	if k := strings.TrimSpace(knowledge); k != "" {
		lines = append(lines, k)
	}
	if len(lines) == 0 {
		return ""
	}
	return "<run_context>\n" + strings.Join(lines, "\n") + "\n</run_context>"
}

// joinSections drops the empty sections and separates the rest by a blank line.
func joinSections(sections ...string) string {
	var kept []string
	for _, s := range sections {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	return strings.Join(kept, "\n\n")
}

// resolveBranchTemplate returns the team's branch-naming convention with the
// run's ticket id substituted, falling back to domain.DefaultBranchTemplate
// when the team has no setting (or the teams store isn't wired — test
// fixtures). Read under the admin pool: this runs in a background goroutine
// with no JWT claims, like the rest of the spawner's reads.
func (s *Spawner) resolveBranchTemplate(ctx context.Context, task domain.Task) string {
	tmpl := domain.DefaultBranchTemplate
	if s.teams != nil && task.TeamID != nil && *task.TeamID != "" {
		if ts, err := s.teams.GetSettingsSystem(ctx, *task.TeamID); err == nil && ts.BranchTemplate != "" {
			tmpl = ts.BranchTemplate
		}
	}
	return renderBranchTemplate(tmpl, task)
}

// renderBranchTemplate substitutes the literal "<ticket-id>" in a branch-name
// template with the run's ticket id when one is known — the Jira issue key. For
// runs with no ticket (GitHub PR, taskless), the placeholder is left verbatim
// so the agent fills in a sensible identifier itself. This is envelope guidance
// only; the push gate authorizes whatever branch the worktree lands on, so a
// branch named off-template is never blocked.
func renderBranchTemplate(tmpl string, task domain.Task) string {
	if task.EntitySource == "jira" && task.EntitySourceID != "" {
		return strings.ReplaceAll(tmpl, "<ticket-id>", task.EntitySourceID)
	}
	return tmpl
}

func (s *Spawner) cachedAgentTools() string {
	// Local mode only. The scan reads the TF process's own
	// ~/.claude/agents — in local mode that's the single trusted user's
	// machine, so their agent-declared tools are theirs to grant. In
	// multi mode the process is shared infrastructure: whatever agent
	// files exist on the orchestrator host belong to the operator, not
	// any tenant, and merging them here would silently widen
	// --allowedTools for every org's runs. Multi-mode runs get extra
	// tools only from the prompt's own allowed_tools column.
	if runmode.Current() != runmode.ModeLocal {
		return ""
	}
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
	// Outcome is the single terminal vocabulary (continue|finish|abort).
	// See domain.ConversationOutcome and the completion block in
	// internal/agentprompt.
	Outcome string `json:"outcome"`
	// Summary is the natural-language "what I did" — required on a
	// finish/continue. Maps to conversations.result_summary.
	Summary string `json:"summary"`
	// Reason is the natural-language "why I stopped / what a human needs
	// to do" — required on (and only meaningful for) an abort outcome. Maps
	// to conversations.outcome_reason, kept distinct from Summary.
	Reason string         `json:"reason"`
	Links  map[string]any `json:"links"` // keyed URLs (pr_review, pr, jira_issues)
}

// isValid reports whether the envelope is a recognized conclusion carrying its
// required companion field: finish/continue need a non-empty summary; abort
// needs a non-empty reason (abort is the agent's *voluntary* "I'm functioning
// but choosing to stop" decision, so the why is exactly the thing worth
// collecting — involuntary death is the separate `failed` status). Any other
// outcome token, or a recognized one missing its companion, is not a valid
// conclusion — it's an invalid attempt the driver re-prompts to fix.
func (r *agentResult) isValid() bool {
	switch domain.ConversationOutcome(r.Outcome) {
	case domain.ConversationOutcomeContinue, domain.ConversationOutcomeFinish:
		return r.Summary != ""
	case domain.ConversationOutcomeAbort:
		return r.Reason != ""
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

// turnClass is the three-way classification of an agent turn-end: the run is
// either concluded (valid), made a malformed conclusion attempt (invalid), or
// did neither (none — prose / nothing, i.e. an open run).
type turnClass int

const (
	// turnNone — no envelope attempt at all: prose, nothing, or unrelated
	// JSON with no `outcome` key. The run is open: not concluded, not
	// executing, no claim about why it stopped.
	turnNone turnClass = iota
	// turnInvalid — the output IS an envelope attempt (a JSON object carrying
	// an `outcome` key, or clearly envelope-shaped output that won't parse)
	// but it fails validation: malformed JSON, an unrecognized outcome, or a
	// recognized outcome missing its required companion. The driver re-prompts
	// to fix, then fails on exhaustion.
	turnInvalid
	// turnValid — a recognized conclusion with its required companion field.
	turnValid
)

// classifyAgentResult sorts the agent's final message text into the three
// turn-end buckets. It looks for the agent's intended completion envelope — the
// first brace-delimited JSON object that decodes AND carries an `outcome` key
// (envelopeObject) — tolerating leading prose, trailing text, markdown fences,
// and nested objects. If such an object is found it's valid or invalid per
// isValid; if none decodes but the text still carries an `"outcome"` key (a
// malformed object that wouldn't parse) it's an invalid attempt the driver
// re-prompts; anything else — prose, an empty or unrelated JSON object — is
// none, and the run is open. The parsed result is returned only for valid.
//
// Known limitation: the malformed-envelope fallback is a substring check for
// the quoted `"outcome"` token, so prose that literally contains it with no
// real envelope is treated as a (malformed) attempt and re-prompted once. The
// contract is "ONLY a JSON object," so that's a rare violation the re-prompt
// corrects.
func classifyAgentResult(text string) (turnClass, *agentResult) {
	text = strings.TrimSpace(text)
	if text == "" {
		return turnNone, nil
	}
	if candidate, ok := envelopeObject(text); ok {
		var result agentResult
		_ = json.Unmarshal([]byte(candidate), &result) // envelopeObject already decoded it
		if result.isValid() {
			return turnValid, &result
		}
		return turnInvalid, nil // a parseable envelope attempt that fails validation
	}
	// No decodable object carried an `outcome`. If the text is nonetheless
	// clearly an envelope attempt that didn't parse (a malformed object with an
	// "outcome" key), treat it as invalid so the driver re-prompts; otherwise
	// it's prose / unrelated JSON and the run is open.
	if looksMalformedEnvelope(text) {
		return turnInvalid, nil
	}
	return turnNone, nil
}

// envelopeObject returns the agent's intended completion envelope: the first
// brace-delimited JSON object in text that decodes AND carries an `outcome`
// key, as an exactly-bounded span re-parseable with json.Unmarshal. A streaming
// json.Decoder started at each `{` consumes one balanced object and ignores the
// rest, so this tolerates leading prose, a trailing ``` fence, text after the
// JSON, and nested objects — cases the cruder first-{-to-last-} span could not
// (e.g. "Config {x} done. {\"outcome\":\"finish\",...}"). Objects without an
// `outcome` key (a stray prose `{...}`, a nested links object, `{}`) are
// skipped. Returns ok=false when no such object exists.
func envelopeObject(text string) (string, bool) {
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(text[i:]))
		var probe map[string]any
		if dec.Decode(&probe) != nil {
			continue // not a JSON object starting at this brace
		}
		if _, hasOutcome := probe["outcome"]; !hasOutcome {
			continue // a JSON object, but not the envelope
		}
		return text[i : i+int(dec.InputOffset())], true
	}
	return "", false
}

// looksMalformedEnvelope reports whether text is clearly an envelope attempt
// that no extraction could parse — the heuristic is the presence of the
// `"outcome"` JSON key. Prose rarely carries that exact token, so a positive
// here means "the agent tried to emit an envelope and botched the JSON," which
// the driver re-prompts rather than treating as an open run.
func looksMalformedEnvelope(text string) bool {
	return strings.Contains(text, `"outcome"`)
}

// parseAgentResult returns the parsed envelope iff the text is a valid
// conclusion (a recognized outcome with its required companion field), else
// nil. Used by the terminal-recording path (processCompletion) where only a
// usable conclusion is acted on; the driver uses classifyAgentResult directly
// to tell invalid attempts apart from open runs.
func parseAgentResult(text string) *agentResult {
	if class, result := classifyAgentResult(text); class == turnValid {
		return result
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
