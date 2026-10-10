package agentprompt

import (
	"slices"
	"strings"
)

// The tools reference is agent-facing prompt text that varies on the run's
// entity sources rather than on any Spec axis — a Jira run sees the Jira verbs,
// a Slack-thread run sees the Slack verbs, and both see GitHub's. So it is not
// a manifest section: the caller picks the set and appends it as the <tools>
// section of the per-run tail, which the harness blocks point at.
//
// Core's sets are embedded from the same blocks tree as everything else.
// Non-core sources register at init, which is the whole reason the registry
// exists: ee/ cannot be imported by core, so an ee package contributes its
// text inward instead of core reaching outward for it.
//
// The issue trackers share one more block, the workspace materialization: a
// run on a tracker issue starts with no checkout, and how to get one is the
// same whichever tracker the issue lives in. It is its own file so a set
// naming both trackers carries it once.

// The core blocks are resolved once at init, not per call. Every run dispatch
// reads them (a Jira run reads three), and they never vary, so there is no
// embed read, copy, or concatenation on the dispatch path. Package-level so a
// missing block fails at process start rather than on the first dispatch.
var (
	githubTools    = block(blockToolsGitHub)
	jiraTools      = block(blockToolsJira)
	linearTools    = block(blockToolsLinear)
	workspaceTools = block(blockToolsWorkspace)

	githubReference = githubTools + "\n"
	jiraReference   = jiraTools + "\n\n" + workspaceTools + "\n"
	linearReference = linearTools + "\n\n" + workspaceTools + "\n"
)

// GitHubToolsReference is the agent-facing docs for the `exec gh` verb family.
func GitHubToolsReference() string { return githubReference }

// JiraToolsReference is the agent-facing docs for the `exec jira` verb family
// plus the per-run workspace materialization every codebase-less run needs:
// what ToolsReferenceForSources composes for a set naming Jira alone, with a
// trailing newline.
func JiraToolsReference() string { return jiraReference }

// LinearToolsReference is the agent-facing docs for the `exec linear` verb
// family plus the per-run workspace materialization, under the same contract
// as JiraToolsReference.
func LinearToolsReference() string { return linearReference }

// toolsReferenceRegistry is the process-global map of non-core entity sources
// (e.g. "slack") to their agent-facing tools-reference text. Same no-mutex
// startup-write/steady-read contract as routing.sourceRegistry
// (internal/routing/source_registry.go:44-48): an ee package registers its text
// once from its own init()/install time, before any delegated run reaches for
// it.
var toolsReferenceRegistry = map[string]string{}

// coreToolsReferenceSources are the entity sources core already ships a static
// embedded tools doc for. A registration for one of these would be a wiring bug
// — a double-source collision or a stale rename — never a legitimate ee
// contribution.
var coreToolsReferenceSources = map[string]bool{
	"github": true, "jira": true, "linear": true,
}

// RegisterToolsReference registers the tools-reference text for a non-core
// entity source (e.g. "slack"). Called from an ee package's init(); panics
// on empty source/text, a duplicate source, or a core source ("github",
// "jira", "linear") — a wiring bug that must fail at boot, not silently
// degrade a run's tool docs. Registered text is literal, like the embedded
// blocks: write `triagefactory exec <verb>` (the prompt builder points it at
// the run's binary), and name a per-run fact by pointing at the section of the
// prompt that carries it.
func RegisterToolsReference(source, text string) {
	if source == "" {
		panic("agentprompt.RegisterToolsReference: source must not be empty")
	}
	if text == "" {
		panic("agentprompt.RegisterToolsReference: text must not be empty")
	}
	if coreToolsReferenceSources[source] {
		panic("agentprompt.RegisterToolsReference: " + source + " is a core source with a static embedded template and cannot be registered")
	}
	if _, exists := toolsReferenceRegistry[source]; exists {
		panic("agentprompt.RegisterToolsReference: " + source + " is already registered")
	}
	toolsReferenceRegistry[source] = text
}

// ToolsReferenceForSources composes the <tools> section for a set of sources.
//
// The set is the ORG's, not the run's. What a delegated agent may be told about
// follows from what this deployment can reach — a run working a pull request
// that references a ticket needs the Jira verbs, and the kind of entity that
// triggered it says nothing about whether the org has Jira. Selecting on the
// trigger is how an agent ends up unable to touch the ticket its own PR names.
//
// Over-inclusion is the safe direction and under-inclusion is not, which is why
// callers union rather than intersect and fall back to a superset when they
// cannot resolve. This block is DOCUMENTATION; the gate that actually stops a
// verb is the credential funnel it resolves through, and an advertised verb
// that refuses tells the agent exactly what happened. A missing one just makes
// it improvise.
//
// Order is canonical, not the caller's: core first, then registered sources
// alphabetically, deduplicated. Two callers assembling the same set produce the
// same bytes, which is what keeps a cached prefix cached.
//
// The workspace block closes the core group: it follows the tracker blocks,
// which say how to read the issue while it says how to get the code the issue
// concerns, and it precedes every registered source. Its inclusion is decided
// by core sources alone, so placing it inside the core group means a
// registered source's presence never moves it: the core portion of the
// section is byte-identical whatever ee has registered.
func ToolsReferenceForSources(kinds []string) string {
	seen := make(map[string]bool, len(kinds))
	var core, registered []string
	for _, k := range kinds {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		switch {
		case coreToolsReferenceSources[k]:
			core = append(core, k)
		case toolsReferenceRegistry[k] != "":
			registered = append(registered, k)
		}
	}
	slices.Sort(core) // "github", then "jira", then "linear"
	slices.Sort(registered)

	parts := make([]string, 0, len(core)+1+len(registered))
	tracker := false
	for _, k := range core {
		switch k {
		case "github":
			parts = append(parts, githubTools)
		case "jira":
			parts = append(parts, jiraTools)
			tracker = true
		case "linear":
			parts = append(parts, linearTools)
			tracker = true
		}
	}
	if tracker {
		parts = append(parts, workspaceTools)
	}
	for _, k := range registered {
		parts = append(parts, strings.TrimSpace(toolsReferenceRegistry[k]))
	}
	return strings.Join(parts, "\n\n")
}

// ToolsReferenceFor returns the registered tools-reference text for source
// and whether one exists.
func ToolsReferenceFor(source string) (string, bool) {
	text, ok := toolsReferenceRegistry[source]
	return text, ok
}

// RegisteredToolsReferenceSources returns the registered (non-core) sources
// in sorted order — the composition-root parity guard reads it to hold each
// source's tools reference and its exec CLI registration to one another.
func RegisteredToolsReferenceSources() []string {
	out := make([]string, 0, len(toolsReferenceRegistry))
	for source := range toolsReferenceRegistry {
		out = append(out, source)
	}
	slices.Sort(out)
	return out
}

// ResetToolsReferences clears the registry (tests only).
func ResetToolsReferences() {
	toolsReferenceRegistry = map[string]string{}
}
