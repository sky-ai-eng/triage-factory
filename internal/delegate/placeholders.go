package delegate

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// BuildPromptReplacer composes the full placeholder substitution for a
// delegation prompt. Always-available keys come from the spawner
// (run/binary/scope/tools plus task identity); source-specific keys are
// extracted from the task's entity source ID (owner/repo/pr_number or
// issue_key/project); event-specific keys are pulled from the primary
// event's metadata JSON.
//
// Unresolved placeholders render empty. Empty beats "N/A" because prompts
// sometimes reference the value non-prose-style (e.g. as an argument to a
// CLI command) where a literal "N/A" would be strictly worse than empty.
// Empty also beats leaving the literal "{{X}}" because prompt authors can
// write "The failing run is {{WORKFLOW_RUN_ID}}." and get a coherent
// sentence when the task has one, and a slightly-degraded-but-not-broken
// sentence when it doesn't.
//
// metadataJSON is the primary event's metadata blob — "" is fine; all
// event-derived placeholders just stay empty.
//
// runRoot is the run-root path AS THE AGENT SEES IT (agentproc.AgentVisibleRoot
// — "/work" under the sandbox, the host run-root otherwise); blueprintRunID
// names the workflow run, which prompts use for scratch paths the steps of one
// run share (the parallel review passes' drop point). We register them under
// both the canonical {{RUN_ROOT}} / {{BLUEPRINT_RUN_ID}} placeholders and the
// shell-style $TRIAGE_FACTORY_CONVERSATION_ROOT / $TRIAGE_FACTORY_BLUEPRINT_RUN_ID
// names: the env vars are exported for Bash, but the agent's file tools do no
// shell expansion, so any prompt that references the bare env var would otherwise
// write to a literal "$TRIAGE_FACTORY_CONVERSATION_ROOT/..." path that resolves
// nowhere. Pre-expanding both forms makes the path resolve regardless of which
// tool the agent reaches for or which form a (possibly user-authored) prompt used.
//
// {{SCOPE}} and {{TOOLS_REFERENCE}} are deliberately NOT handled here — buildPrompt
// inlines those sections into the template text before this pass, because
// strings.Replacer does not re-scan replacement values and the tools docs carry
// their own placeholders ({{BINARY_PATH}}, the run-root memory paths).
func BuildPromptReplacer(task domain.Task, metadataJSON, runID, binaryPath, runRoot, blueprintRunID, branchTemplate, runURL string) *strings.Replacer {
	pairs := []string{
		"{{RUN_ID}}", runID,
		"{{BINARY_PATH}}", binaryPath,
		"{{RUN_ROOT}}", runRoot,
		"{{BLUEPRINT_RUN_ID}}", blueprintRunID,
		"$TRIAGE_FACTORY_CONVERSATION_ROOT", runRoot,
		"$TRIAGE_FACTORY_BLUEPRINT_RUN_ID", blueprintRunID,
		"{{TASK_TITLE}}", task.Title,
		"{{EVENT_TYPE}}", task.EventType,
		"{{EVENT_METADATA_JSON}}", metadataJSON,
		// Branch-naming convention (team setting, ticket-id-resolved), surfaced
		// as envelope + tools-doc guidance. Resolved in the second pass — not
		// buildPrompt's pre-pass — so it also interpolates inside the injected
		// {{TOOLS_REFERENCE}} value (which the non-rescanning pre-pass can't reach).
		"{{BRANCH_TEMPLATE}}", branchTemplate,
		// Deep link back to this run in the TF UI (TFAC-591). Computed
		// spawner-side (Spawner.runURLFor) from the deployment's public URL;
		// empty when no public URL is configured — never a fabricated
		// localhost link. Guidance in the envelope tells the agent to omit a
		// blank URL rather than invent one.
		"{{RUN_URL}}", runURL,
	}

	var meta map[string]any
	if metadataJSON != "" {
		_ = json.Unmarshal([]byte(metadataJSON), &meta)
	}

	// Declare every source-specific placeholder unconditionally so a
	// cross-source prompt (e.g., a trigger that matches both GitHub and
	// Jira events) renders the non-applicable side as empty rather than
	// leaking literal "{{OWNER}}" through. Upholds the doc contract:
	// "Unresolved placeholders render empty."
	owner, repo, prNumber := "", "", ""
	issueKey, project := "", ""
	switch task.EntitySource {
	case "github":
		owner, repo, prNumber = parseGitHubEntitySourceID(task.EntitySourceID)
	case "jira":
		issueKey = task.EntitySourceID
		project = projectFromJiraKey(issueKey)
	case "slack":
		// No source-specific pairs yet — Slack context (channel, thread ts,
		// message text) rides {{EVENT_METADATA_JSON}} instead. Case kept
		// explicit so this switch stays exhaustive-by-intent as sources
		// are added.
	}
	pairs = append(pairs,
		"{{OWNER}}", owner,
		"{{REPO}}", repo,
		"{{PR_NUMBER}}", prNumber,
		"{{ISSUE_KEY}}", issueKey,
		"{{PROJECT}}", project,
	)

	// Event-specific placeholders. We list every name the shipped prompts
	// reference (plus a few adjacent ones that user-authored prompts would
	// reach for) unconditionally — unresolved names render empty. The
	// `omitempty` tags on CI metadata (WorkflowRunID) keep zero-valued IDs
	// out of the JSON, so third-party CI renders "" rather than "0".
	pairs = append(pairs,
		"{{HEAD_SHA}}", metaString(meta, "head_sha"),
		"{{WORKFLOW_RUN_ID}}", metaInt(meta, "workflow_run_id"),
		"{{CHECK_NAME}}", metaString(meta, "check_name"),
		"{{CHECK_RUN_ID}}", metaInt(meta, "check_run_id"),
		"{{CHECK_URL}}", metaString(meta, "check_url"),
		"{{CONCLUSION}}", metaString(meta, "conclusion"),
		"{{REVIEWER}}", metaString(meta, "reviewer"),
		"{{REVIEW_TYPE}}", metaString(meta, "review_type"),
		"{{ASSIGNEE}}", metaString(meta, "assignee"),
		"{{STATUS}}", metaString(meta, "status"),
		"{{PRIORITY}}", metaString(meta, "priority"),
		"{{ISSUE_TYPE}}", metaString(meta, "issue_type"),
		"{{SUMMARY}}", metaString(meta, "summary"),
	)

	return strings.NewReplacer(pairs...)
}

// parseGitHubEntitySourceID splits "owner/repo#42" into its parts. Returns
// empty strings on parse failure — callers render the placeholder empty.
func parseGitHubEntitySourceID(s string) (owner, repo, prNumber string) {
	hashIdx := strings.LastIndex(s, "#")
	if hashIdx < 0 {
		return "", "", ""
	}
	prNumber = s[hashIdx+1:]
	repoStr := s[:hashIdx]
	slashIdx := strings.LastIndex(repoStr, "/")
	if slashIdx < 0 {
		return "", "", prNumber
	}
	return repoStr[:slashIdx], repoStr[slashIdx+1:], prNumber
}

// projectFromJiraKey pulls "PROJ" out of "PROJ-123". Mirrors the tracker's
// extractProject helper so the placeholder matches what the scorer sees.
func projectFromJiraKey(key string) string {
	if i := strings.IndexByte(key, '-'); i > 0 {
		return key[:i]
	}
	return key
}

// metaString returns the string value at key, or "" if absent or non-string.
func metaString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// metaInt returns the numeric value at key as a decimal string, or "" if
// absent / non-numeric. JSON numbers come through as float64; we format via
// int64 so large IDs (workflow run IDs regularly hit 10^10+) don't surface
// in scientific notation, and so the output is usable directly as a CLI
// argument.
func metaInt(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch n := v.(type) {
	case float64:
		return strconv.FormatInt(int64(n), 10)
	case int:
		return strconv.Itoa(n)
	case int64:
		return strconv.FormatInt(n, 10)
	case string:
		return n
	default:
		return ""
	}
}
