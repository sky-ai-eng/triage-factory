package credbundle

import "errors"

// The reasons a credential proxy can fail to produce a credential for one
// request. A sidecar token source wraps the one that applies, so the proxy
// answering the request can name the reason without reading the error's text,
// which may carry credential-setup detail that neither the jailed caller nor
// the log may see.
var (
	ErrNoBundle         = errors.New("credbundle: no bundle")
	ErrNoRepoToken      = errors.New("credbundle: no token for this repository")
	ErrNoCLIToken       = errors.New("credbundle: no gh CLI token")
	ErrNoJiraCredential = errors.New("credbundle: no Jira credential")
	ErrTokenExpiring    = errors.New("credbundle: token expired or about to expire")
)

// The closed vocabulary MissReason answers in. Every value is safe to show the
// jailed agent and to log: none carries a credential, a repository, or any
// other tenant data.
const (
	MissNoBundle         = "no_bundle"
	MissNoRepoToken      = "no_repo_token"
	MissNoCLIToken       = "no_cli_token"
	MissNoJiraCredential = "no_jira_credential"
	MissTokenExpiring    = "token_expiring"
	MissOther            = "other"
)

// MissReason maps a credential-lookup error to the closed vocabulary above. An
// error wrapping none of the sentinels is MissOther, which is what every
// local-mode source answers: those resolve against the live secret store and
// report their own failures.
func MissReason(err error) string {
	switch {
	case errors.Is(err, ErrNoBundle):
		return MissNoBundle
	case errors.Is(err, ErrNoRepoToken):
		return MissNoRepoToken
	case errors.Is(err, ErrNoCLIToken):
		return MissNoCLIToken
	case errors.Is(err, ErrNoJiraCredential):
		return MissNoJiraCredential
	case errors.Is(err, ErrTokenExpiring):
		return MissTokenExpiring
	default:
		return MissOther
	}
}
