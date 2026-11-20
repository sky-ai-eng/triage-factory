package delegate

import "github.com/sky-ai-eng/triage-factory/internal/agentprompt"

// runContext is the <run_context> block a claim renders from these facts.
func runContext(scope, taskState, siblingWork, runRoot, prCheckout, baseCheckout, branchTemplate, runURL, sourceNotes, knowledge string) string {
	return renderRunContext(runContextLines(scope, taskState, siblingWork, runRoot, prCheckout, baseCheckout, branchTemplate, runURL, sourceNotes, knowledge))
}

// tools is the <tools> section a claim renders for these sources.
func (d sourceDocs) tools(runtime agentprompt.Runtime) string {
	return agentprompt.ToolsReferenceForSources(runtime, d.kinds)
}
