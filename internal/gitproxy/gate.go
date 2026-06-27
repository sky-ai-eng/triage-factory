package gitproxy

import (
	"net/http"
	"strings"
)

// git smart-HTTP operation discriminators, returned by parseGitPath and used
// as the DeniedGitOp.Op token in the audit log.
const (
	gitOpAdvertise = "advertise" // GET  /info/refs?service=...
	gitOpFetch     = "fetch"     // POST /git-upload-pack
	gitOpPush      = "push"      // POST /git-receive-pack
	gitOpPath      = "path"      // a non-git path rejected by the allowlist
)

// parseGitPath recognizes exactly the three git smart-HTTP request shapes and
// extracts the (owner, repo) they target. Anything else — a GHES API path
// "/api/v3/...", Git-LFS, a web route — returns ok=false so the Handler rejects
// it with a 403 before any credential is injected. This is the path-shape
// allowlist that makes "the git proxy can only do git" hold even on GHES,
// where the REST API shares the git host (unlike github.com's api.github.com
// split).
//
// Allowed (the ?service=git-(upload|receive)-pack query a real git client
// sends on info/refs is not inspected — both advertise variants are repo-gated
// only, and info/refs is unambiguously a git path):
//
//	GET  /{owner}/{repo}[.git]/info/refs        → advertise
//	POST /{owner}/{repo}[.git]/git-upload-pack  → fetch
//	POST /{owner}/{repo}[.git]/git-receive-pack → push
func parseGitPath(method, path string) (owner, repo, op string, ok bool) {
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segs) < 3 {
		return "", "", "", false
	}
	owner = segs[0]
	repo = strings.TrimSuffix(segs[1], ".git")
	if !validRepoSeg(owner) || !validRepoSeg(repo) {
		return "", "", "", false
	}
	rest := segs[2:]
	switch {
	case method == http.MethodGet && len(rest) == 2 && rest[0] == "info" && rest[1] == "refs":
		return owner, repo, gitOpAdvertise, true
	case method == http.MethodPost && len(rest) == 1 && rest[0] == "git-upload-pack":
		return owner, repo, gitOpFetch, true
	case method == http.MethodPost && len(rest) == 1 && rest[0] == "git-receive-pack":
		return owner, repo, gitOpPush, true
	}
	return "", "", "", false
}

// validRepoSeg accepts a single owner/repo path segment: non-empty, drawn from
// the GitHub owner/repo alphabet [A-Za-z0-9._-], and free of "..". Restricting
// the charset is defense-in-depth for the allowlist — it rejects a surviving
// percent-escape (e.g. a smuggled "%2e%2e" or "%2f"), whitespace, and any other
// byte that could yield an ambiguous or normalized path, so the owner/repo we
// extract (and feed to the Authorize gate + the per-repo token-cache key) is
// always a clean, expected shape. ".." is excluded explicitly: it passes the
// charset (dots) but is no real repo name and enables traversal.
func validRepoSeg(s string) bool {
	if s == "" || strings.Contains(s, "..") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}
