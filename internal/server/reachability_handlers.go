package server

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/reachability"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
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
// private GHES host). No auth is sent; the derived API base (ghbase.APIBase)
// only needs to *answer*. Always 200 — the body's `reachable` flag is the
// verdict, mirroring handleGitHubPreflightSSH, so the client tells "host
// unreachable" apart from "the server errored". A malformed URL is the one
// 400: that's bad input, not an unreachable host.
//
// POST /api/github/reachability   body: {"url": "https://github.com"}
func handleGitHubReachability(w http.ResponseWriter, r *http.Request) {
	var req reachabilityRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	base, ok := normalizeBaseURL(req.URL)
	if !ok {
		invalidBaseURLField(w, "url")
		return
	}
	writeJSON(w, http.StatusOK, reachability.Probe(r.Context(), ghbase.APIBase(base)))
}

// handleJiraReachability is the Jira equivalent: it probes the same host a
// Jira client would use ({base}/rest/api/2/serverInfo), no auth, URL-only.
// Credentials still go through POST /api/jira/connect. Same always-200 /
// 400-on-malformed contract as the GitHub endpoint.
//
// POST /api/jira/reachability   body: {"url": "https://jira.example.com"}
func handleJiraReachability(w http.ResponseWriter, r *http.Request) {
	var req reachabilityRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	base, ok := normalizeBaseURL(req.URL)
	if !ok {
		invalidBaseURLField(w, "url")
		return
	}
	writeJSON(w, http.StatusOK, reachability.Probe(r.Context(), jira.ReachabilityURL(base)))
}

// invalidBaseURLField writes the rejection every base-URL door gives, so the
// probe and the settings write that persists the probed value name one rule.
func invalidBaseURLField(w http.ResponseWriter, field string) {
	httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
		Reason:  httpx.ReasonInvalidField,
		Message: field + " must be a valid http(s) URL with no credentials, query, or fragment",
		Field:   field,
	})
}

// normalizeBaseURL validates a user-entered GitHub/Jira base URL and returns
// its canonical form (scheme://host[/path], trailing slash trimmed). One
// function for the probe and for the settings write that persists the same
// value, so a URL the probe accepts is exactly the one the column can hold. A
// base URL must parse, use http or https, and carry a host — and must NOT
// carry credentials (userinfo), a query, or a fragment. Those forms are
// rejected as bad input (a 400 at the handler) rather than flowing into a
// malformed derived probe URL — e.g. without this, "https://host?x=1" would
// become "https://host?x=1/api/v3" and surface a confusing 200 + unreachable
// instead of "fix your URL". A path is preserved because Jira Data Center can
// live under a context path. For a GitHub base a path is unusual — GHES mounts
// the API at the host root, so it flows into APIBase's /api/v3 suffix — but the
// reachability verdict is host-level (the probe only asks whether that host
// answers), so an odd path can't produce a wrong reachable/unreachable result.
func normalizeBaseURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", false
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), true
}
