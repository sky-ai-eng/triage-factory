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
func BuildPromptReplacer(task domain.Task, metadataJSON, runID, binaryPath, scope, toolsRef string) *strings.Replacer {
	pairs := []string{
		"{{RUN_ID}}", runID,
		"{{BINARY_PATH}}", binaryPath,
		"{{SCOPE}}", scope,
		"{{TOOLS_REFERENCE}}", toolsRef,
		"{{TASK_TITLE}}", task.Title,
		"{{EVENT_TYPE}}", task.EventType,
		"{{EVENT_METADATA_JSON}}", metadataJSON,
	}

	var meta map[string]any
	if metadataJSON != "" {
		_ = json.Unmarshal([]byte(metadataJSON), &meta)
	}

	switch task.EntitySource {
	case "github":
		owner, repo, prNumber := parseGitHubEntitySourceID(task.EntitySourceID)
		pairs = append(pairs,
			"{{OWNER}}", owner,
			"{{REPO}}", repo,
			"{{PR_NUMBER}}", prNumber,
		)
	case "jira":
		issueKey := task.EntitySourceID
		pairs = append(pairs,
			"{{ISSUE_KEY}}", issueKey,
			"{{PROJECT}}", projectFromJiraKey(issueKey),
		)
	}

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

// projectFromJiraKey pulls "SKY" out of "SKY-123". Mirrors the tracker's
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
