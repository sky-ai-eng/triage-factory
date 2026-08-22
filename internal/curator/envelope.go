package curator

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// curatorSpec selects the curator's block set from internal/agentprompt. The
// mode is resolved at the call site rather than inside Build (which stays a
// pure function of its Spec); the curator takes neither mode arm today, but
// passing the real posture keeps the seam honest if it ever grows one.
func curatorSpec() agentprompt.Spec {
	return agentprompt.Spec{
		Surface: agentprompt.SurfaceCurator,
		Runtime: agentprompt.RuntimeSDK,
		Family:  agentprompt.FamilyClaude,
		Mode:    agentprompt.ModeFor(string(runmode.Current())),
	}
}

// envelopeInputs is the set of values the curator envelope template
// consumes. Built once per dispatch from the project row + the running
// process's selfBin.
type envelopeInputs struct {
	ProjectName        string
	ProjectDescription string
	PinnedRepos        []string
	JiraProjectKey     string
	LinearProjectKey   string
	BinaryPath         string
	// AvailableSources are the org's reachable event sources, which decide
	// which verb families the curator is told about. Empty falls back to core's
	// two, which is the superset — over-inclusion costs a refused verb with an
	// error that explains itself, and under-inclusion costs the agent a
	// capability it needed.
	AvailableSources []string
}

// renderEnvelope composes the --append-system-prompt payload for one curator
// dispatch: the curator block set, then this project's own facts and the verb
// reference the blocks point at. Static at
// the *project* level (project name + description don't change
// session-to-session) but rendered fresh each turn so that a) the
// pinned-repo / tracker snapshot reflects current state on first
// dispatch of a new session, and b) we don't depend on Claude Code
// honoring a changed prompt across resumed sessions — the hidden
// context_change channel is the source of truth for drift, but we
// keep the envelope current too so the values never go further out
// of date than one turn.
func renderEnvelope(in envelopeInputs) string {
	// The composed blocks and the verb docs are ours, so they name the CLI as
	// `triagefactory exec <verb>` and get pointed at this process's own binary.
	// The project's name and description are the user's text and are rendered
	// verbatim — no pass runs over them.
	cli := func(s string) string { return strings.ReplaceAll(s, "triagefactory exec", in.BinaryPath+" exec") }

	return strings.Join([]string{
		cli(strings.TrimSpace(agentprompt.Build(curatorSpec()))),
		renderProjectContext(in),
		cli("<tools>\n" + strings.TrimSpace(renderToolsReference(in.AvailableSources)) + "\n</tools>"),
	}, "\n\n")
}

// renderProjectContext renders the per-project facts the curator blocks refer
// to but cannot contain: which project this session is about, what is pinned to
// it, and which tracker it is linked to. The blocks stay identical across every
// project so their prefix is one cached string per deployment.
func renderProjectContext(in envelopeInputs) string {
	return "<project_context>\n" +
		"Project: " + projectNameOrFallback(in.ProjectName) + "\n\n" +
		"Description:\n" + projectDescriptionOrFallback(in.ProjectDescription) + "\n\n" +
		"Pinned repositories, materialized as read-only working trees:\n" +
		renderPinnedReposBlock(in.PinnedRepos) + "\n\n" +
		renderTrackersBlock(in.JiraProjectKey, in.LinearProjectKey) + "\n" +
		"</project_context>"
}

func projectNameOrFallback(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "(unnamed)"
	}
	return name
}

func projectDescriptionOrFallback(desc string) string {
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return "(no description provided — ask the user for context if you need it)"
	}
	return desc
}

// renderPinnedReposBlock formats the pinned-repo list as a bullet list
// the agent can read at a glance, including the on-disk path each repo
// is materialized at. The path matters: the agent's mental model is
// "use `git -C ./repos/<owner>/<repo>` for read-only inspection" and
// stating it explicitly removes a guessing step.
func renderPinnedReposBlock(repos []string) string {
	if len(repos) == 0 {
		return "(no repositories pinned — ask the user to pin one if they want repo context.)"
	}
	sorted := append([]string(nil), repos...)
	sort.Strings(sorted)
	var b strings.Builder
	for _, slug := range sorted {
		fmt.Fprintf(&b, "- %s — ./repos/%s\n", slug, slug)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderTrackersBlock describes which trackers, if any, this project is
// linked to. Both columns are independent (the user can have Jira,
// Linear, both, or neither). When neither is set, the block still
// renders a line so the agent knows the absence is intentional rather
// than a templating bug.
func renderTrackersBlock(jiraKey, linearKey string) string {
	jira := strings.TrimSpace(jiraKey)
	linear := strings.TrimSpace(linearKey)
	if jira == "" && linear == "" {
		return "No issue tracker is linked to this project. Ticket numbers the user mentions cannot be auto-resolved; ask which tracker they came from if it matters."
	}
	var lines []string
	if jira != "" {
		lines = append(lines, fmt.Sprintf("Jira project key: %s. Tickets shaped %s-NNN belong to this project.", jira, jira))
	}
	if linear != "" {
		lines = append(lines, fmt.Sprintf("Linear project: %s.", linear))
	}
	return strings.Join(lines, "\n")
}

// renderToolsReference inlines the tool docs for the sources this org can
// actually reach, so the curator and a delegated agent share both the same
// vocabulary for `triagefactory exec` invocations and the same rule for which
// families appear at all. Cheap copy — these strings are kilobytes, not
// megabytes.
func renderToolsReference(sources []string) string {
	if len(sources) == 0 {
		sources = []string{eventsource.KindGitHub, eventsource.KindJira}
	}
	return agentprompt.ToolsReferenceForSources(sources)
}

// pendingChangesNote renders the hidden [system note] block that gets
// prepended to the user's next message when the turn consumed pending
// 'injection:context' rows. Returns "" when there is nothing to inject so
// the caller can short-circuit without prepending an empty fence.
//
// Each consumed row carries a baseline (snapshot remembered when the change
// was queued) and the renderer diffs that against the project row's
// *current* state, so an A→B→A round-trip naturally collapses to "no actual
// change" and is omitted.
func pendingChangesNote(rows []domain.CuratorContextChange, current envelopeInputs) string {
	var lines []string
	for _, row := range rows {
		line := renderPendingRow(row, current)
		if line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[system note] The user changed project settings since your last response. Treat the values below as the current ground truth:\n")
	for _, line := range lines {
		b.WriteString("- ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderPendingRow turns one (change_type, baseline → current) pair
// into a human-readable diff line. Returns "" when the baseline
// matches the current value (round-trip no-op) so the caller can drop
// it cleanly.
func renderPendingRow(row domain.CuratorContextChange, current envelopeInputs) string {
	switch row.ChangeType {
	case domain.ChangeTypePinnedRepos:
		return renderPinnedReposDiff(row.BaselineValue, current.PinnedRepos)
	case domain.ChangeTypeJiraProjectKey:
		return renderTrackerDiff("Jira project key", row.BaselineValue, current.JiraProjectKey)
	case domain.ChangeTypeLinearProjectKey:
		return renderTrackerDiff("Linear project", row.BaselineValue, current.LinearProjectKey)
	}
	return ""
}

func renderPinnedReposDiff(baselineJSON string, current []string) string {
	baseline, err := decodeStringSlice(baselineJSON)
	if err != nil {
		return ""
	}
	added, removed := stringSliceDiff(baseline, current)
	if len(added) == 0 && len(removed) == 0 {
		return ""
	}
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "added "+joinQuoted(added))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed "+joinQuoted(removed))
	}
	return "Pinned repositories: " + strings.Join(parts, "; ") + "."
}

func renderTrackerDiff(label, baselineJSON, current string) string {
	baseline, err := decodeNullableString(baselineJSON)
	if err != nil {
		return ""
	}
	current = strings.TrimSpace(current)
	if baseline == current {
		return ""
	}
	switch {
	case baseline == "" && current != "":
		return fmt.Sprintf("%s linked to %q (was unset).", label, current)
	case baseline != "" && current == "":
		return fmt.Sprintf("%s unlinked (was %q).", label, baseline)
	default:
		return fmt.Sprintf("%s changed from %q to %q.", label, baseline, current)
	}
}
