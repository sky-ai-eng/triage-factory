package ai

// toolsReferenceRegistry is the process-global map of non-core entity
// sources (e.g. "slack") to their agent-facing tools-reference text. Same
// no-mutex startup-write/steady-read contract as routing.sourceRegistry
// (internal/routing/source_registry.go:44-48): an ee package registers its
// text once from its own init()/install time, before any delegated run
// reaches for it.
var toolsReferenceRegistry = map[string]string{}

// coreToolsReferenceSources are the entity sources core already ships a
// static //go:embed tools doc for (GHToolsTemplate, JiraToolsTemplate). A
// registration for one of these would be a wiring bug — a double-source
// collision or a stale rename — never a legitimate ee contribution.
var coreToolsReferenceSources = map[string]bool{
	"github": true, "jira": true,
}

// RegisterToolsReference registers the tools-reference text for a non-core
// entity source (e.g. "slack"). Called from an ee package's init(); panics
// on empty source/text, a duplicate source, or a core source ("github",
// "jira") — a wiring bug that must fail at boot, not silently degrade a
// run's tool docs. The registered text may itself carry placeholders like
// {{BINARY_PATH}} — the same {{TOOLS_REFERENCE}} pre-pass that handles
// GHToolsTemplate (internal/delegate/prompt.go) covers it.
func RegisterToolsReference(source, text string) {
	if source == "" {
		panic("ai.RegisterToolsReference: source must not be empty")
	}
	if text == "" {
		panic("ai.RegisterToolsReference: text must not be empty")
	}
	if coreToolsReferenceSources[source] {
		panic("ai.RegisterToolsReference: " + source + " is a core source with a static embedded template and cannot be registered")
	}
	if _, exists := toolsReferenceRegistry[source]; exists {
		panic("ai.RegisterToolsReference: " + source + " is already registered")
	}
	toolsReferenceRegistry[source] = text
}

// ToolsReferenceFor returns the registered tools-reference text for source
// and whether one exists.
func ToolsReferenceFor(source string) (string, bool) {
	text, ok := toolsReferenceRegistry[source]
	return text, ok
}

// ResetToolsReferences clears the registry (tests only).
func ResetToolsReferences() {
	toolsReferenceRegistry = map[string]string{}
}
