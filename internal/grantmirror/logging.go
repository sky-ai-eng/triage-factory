package grantmirror

import "github.com/sky-ai-eng/triage-factory/internal/logging"

// Component logger for the grantmirror package (see internal/logging).
// "grantmirror" covers the by-pull reconcile of App-installation existence and
// the repositories each installation can reach.
var grantMirrorLog = logging.Component("grantmirror")
