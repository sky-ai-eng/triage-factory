package credbundle

import (
	"strings"
	"time"
)

// RepoTokenSource records which credential ResolveRepoToken actually
// selected. GitHubCreds.Mode is the org's configured tier, but the
// provisioner falls back to the PAT per-repo (credprovision.resolveGitHub
// flips Mode to "pat" the moment ANY repo in the run's authorized set
// resolves without an expiry) — so a single bundle can carry a "pat" Mode
// while RepoTokens still holds real App-tier tokens for other repos, or an
// "app" Mode while this specific repo has no RepoTokens entry and falls
// through to the PAT. Callers that report an Identity (the agenthost exec-gh
// surface) must key off this — the credential actually resolved — never off
// Mode, or the reported identity can disagree with the token on the wire.
type RepoTokenSource int

const (
	// RepoTokenNone means no token resolved — no RepoTokens match and no PAT.
	RepoTokenNone RepoTokenSource = iota
	// RepoTokenApp is a repo-scoped GitHub App installation token.
	RepoTokenApp
	// RepoTokenPAT is the org-wide PAT (tier-3 borrow, unscoped).
	RepoTokenPAT
)

// ResolveRepoToken resolves the GitHub credential a bundle carries for
// (owner, repo): an exact repo-scoped installation token when repo is
// known, falling through to the single org-wide PAT (a PAT can't be
// narrowed, so it answers for every repo) if no repo-scoped entry matches.
//
// repo == "" (the caller has no specific repo in view) resolves ONLY when
// owner has exactly one RepoTokens entry — mirroring the production
// resolver's installationFor ambiguity rule ("an empty target only
// resolves when there's exactly one installation"). Map iteration order is
// otherwise undefined, and a bundle's RepoTokens are scoped per-repo
// (unlike the live resolver's account-wide App installation token), so
// picking an arbitrary sibling repo's token when the run's authorized set
// covers more than one repo under owner would 403 every call it's used
// for. Every real caller passes a concrete repo; the ambiguous-repo case
// only falls through to the PAT, never to a wrong App token.
func ResolveRepoToken(gh *GitHubCreds, owner, repo string) (token string, expiresAt time.Time, source RepoTokenSource) {
	if gh == nil {
		return "", time.Time{}, RepoTokenNone
	}
	if repo != "" {
		for repoID, rt := range gh.RepoTokens {
			o, r, cut := strings.Cut(repoID, "/")
			if cut && strings.EqualFold(o, owner) && strings.EqualFold(r, repo) {
				return rt.Token, rt.ExpiresAt, RepoTokenApp
			}
		}
	} else {
		var match RepoToken
		matches := 0
		for repoID, rt := range gh.RepoTokens {
			o, _, cut := strings.Cut(repoID, "/")
			if cut && strings.EqualFold(o, owner) {
				matches++
				match = rt
			}
		}
		if matches == 1 {
			return match.Token, match.ExpiresAt, RepoTokenApp
		}
	}
	if gh.PAT != "" {
		return gh.PAT, time.Time{}, RepoTokenPAT
	}
	return "", time.Time{}, RepoTokenNone
}
