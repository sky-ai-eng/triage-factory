package github

import "strings"

// DefaultBaseURL is the public github.com web base. An org with an empty
// GitHubBaseURL setting (the common case) resolves to this.
const DefaultBaseURL = "https://github.com"

// ResolveBaseURL normalizes a per-org GitHub base URL into the user-facing
// web base NewClient expects. Empty (not configured) maps to github.com;
// a GHES base is returned trimmed of trailing slashes. This is the single
// canonical copy shared by the App-register flow (internal/server) and the
// credential resolver — keep new callers pointed here rather than
// re-deriving.
//
// internal/db keeps its own private copy (github_app_backfill.go) because
// importing internal/github there would form a cycle (the resolver in this
// package imports internal/db).
func ResolveBaseURL(orgBase string) string {
	if orgBase != "" {
		return strings.TrimRight(orgBase, "/")
	}
	return DefaultBaseURL
}

// APIBase derives the REST API base from a user-facing GitHub web base.
// github.com maps to api.github.com; a GHES host gets the /api/v3 suffix.
// Mirrors the derivation NewClient does internally so the App JWT mint
// endpoint and the client agree on where the API lives.
func APIBase(base string) string {
	base = strings.TrimRight(base, "/")
	if base == "" || base == DefaultBaseURL {
		return "https://api.github.com"
	}
	return base + "/api/v3"
}
