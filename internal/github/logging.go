package github

import "github.com/sky-ai-eng/triage-factory/internal/logging"

// Component loggers for the github package (see internal/logging). "github"
// covers the REST/GraphQL client; "gh-resolver" covers credential-tier
// resolution.
var (
	githubLog     = logging.Component("github")
	ghResolverLog = logging.Component("gh-resolver")
)
