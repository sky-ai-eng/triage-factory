package server

import (
	"net/http"
	"net/url"
	"strings"

	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/reachability"
)

// reachabilityRequest is the URL-only body both reachability endpoints accept.
// There is no token field by design: reachability is the setup wizard's URL
// sub-step, deliberately split from the creds sub-step (auth.ValidateGitHub /
// POST /api/jira/connect), so an operator can confirm a host is routable from
// this deployment before entering any credential. These endpoints touch no
// org state — they only probe the network — so they are session-gated (via
// s.apiMutating) but not org-scoped.
type reachabilityRequest struct {
	URL string `json:"url"`
}

// handleGitHubReachability probes whether this deployment can reach the API
// host behind a user-entered GitHub base URL — one of the three free-entry
// host classes (github.com, a *.ghe.com data-residency tenant, or a typically
// private GHES host). No auth is sent; the derived API base (ghclient.APIBase)
// only needs to *answer*. Always 200 — the body's `reachable` flag is the
// verdict, mirroring handleGitHubPreflightSSH, so the client tells "host
// unreachable" apart from "the server errored". A malformed URL is the one
// 400: that's bad input, not an unreachable host.
//
// POST /api/github/reachability   body: {"url": "https://github.com"}
func (s *Server) handleGitHubReachability(w http.ResponseWriter, r *http.Request) {
	var req reachabilityRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	base, ok := normalizeReachabilityURL(req.URL)
	if !ok {
		badRequest(w, "url must be a valid http(s) URL")
		return
	}
	writeJSON(w, http.StatusOK, reachability.Probe(r.Context(), ghclient.APIBase(base)))
}

// handleJiraReachability is the Jira equivalent: it probes the same host a
// Jira client would use ({base}/rest/api/2/serverInfo), no auth, URL-only.
// Credentials still go through POST /api/jira/connect. Same always-200 /
// 400-on-malformed contract as the GitHub endpoint.
//
// POST /api/jira/reachability   body: {"url": "https://jira.example.com"}
func (s *Server) handleJiraReachability(w http.ResponseWriter, r *http.Request) {
	var req reachabilityRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	base, ok := normalizeReachabilityURL(req.URL)
	if !ok {
		badRequest(w, "url must be a valid http(s) URL")
		return
	}
	writeJSON(w, http.StatusOK, reachability.Probe(r.Context(), jira.ReachabilityURL(base)))
}

// normalizeReachabilityURL validates a user-entered base URL for a probe: it
// must parse, use http or https, and carry a host. Returns the
// trailing-slash-trimmed URL. A bad value is rejected at the handler (400) —
// the reachable/unreachable verdict is reserved for well-formed hosts, so a
// typo reads as "fix your URL", not "host down".
func normalizeReachabilityURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	return strings.TrimRight(raw, "/"), true
}
