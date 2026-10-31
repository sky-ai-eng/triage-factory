package deliverableteardown

import "github.com/sky-ai-eng/triage-factory/internal/logging"

// teardownLog names the approval-discard component: retiring an unresolved
// deliverable is the discard arm of the approval lifecycle, so these lines sit
// beside the per-deliverable ones in an operator's filter.
var teardownLog = logging.Component("approval-discard")
