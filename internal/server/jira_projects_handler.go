package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// POST /api/jira/projects/list — the Jira project picker's option list.
//
// The GitHub sibling (POST /api/github/repos/list) reads a local mirror; this
// one proxies Jira live, and the difference is a property of the two estates
// rather than a preference. Reachability on GitHub is computed across
// credential classes over estates that run to thousands behind tight rate
// limits, and the write gate consumes the mirror locally. Jira has one org
// credential and a catalog of dozens that arrives in one call, and its write
// gate asks Jira live too (jira_project_gate.go) — so nothing would read a
// mirror that a live call doesn't serve, and there is nothing for one to earn.
//
// Proxying is what makes this a proxy list: total_count is null, because
// counting the upstream would mean walking every remaining page on the first
// request, and next_page_token wraps the upstream position. See
// httpx.WriteProxyList.
//
// The credential is the org's Jira service credential — the same one the
// poller reads with. A read of the org's own catalog does not need the
// caller's personal Jira identity, and requiring one would make the picker
// unusable for exactly the members who have not bound theirs yet.
// --------------------------------------------------------------------

// jiraProjectListRequest is the body: the picker's search box plus the
// standard paging pair.
type jiraProjectListRequest struct {
	// Q is matched against the project key and the project name, case
	// insensitively. Empty matches everything. Cloud applies it upstream;
	// Data Center, whose catalog endpoint takes no filter, applies it to the
	// catalog in hand — one contract, two implementations (jira.ListProjects).
	Q string `json:"q"`
	httpx.PageRequest
}

// jiraProjectListFilterKey is the canonicalized filter set the page token is
// fingerprinted against, so a token minted for one search cannot address
// another's results. Canonicalized lowercase-and-trimmed because the match
// itself is case-insensitive on both deployments: two spellings of one query
// are one query, and a token minted under either addresses the same page.
type jiraProjectListFilterKey struct {
	Q string `json:"q"`
}

// jiraProjectJSON is one picker option. Key and name and nothing else: a
// project object from either deployment carries avatars, lead, category and a
// self link, none of which the picker renders — and a proxy list that echoes
// whatever the upstream sent is how a provider's shape becomes our contract.
type jiraProjectJSON struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

func (s *Server) handleJiraProjectsList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req jiraProjectListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	q := strings.TrimSpace(req.Q)
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest,
		httpx.FilterFingerprint(jiraProjectListFilterKey{Q: strings.ToLower(q)}), 0)
	startAt := jiraStartAtFromCursor(&v, page.Cursor)
	if page.CountOnly {
		// page_size: 0 asks for total_count under the same filters, and this
		// list has no total to give — a proxy list's total is null by
		// contract. Answering an empty page with a null total would look like
		// a count of nothing, so the ask is refused with the reason named.
		v.OutOfRange("page_size", "page_size must be between 1 and 200; this list is proxied from Jira, which reports no total, so the count-only read (page_size: 0) has no answer here")
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	// Credentials read through the app pool inside WithTx so the org_secrets
	// read runs under the caller's claims — the same door GET
	// /api/jira/statuses uses for the same credential.
	var creds auth.Credentials
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		creds, lerr = integrations.Load(r.Context(), tx.Secrets, orgID)
		if lerr != nil {
			return fmt.Errorf("load integration credentials: %w", lerr)
		}
		return nil
	}); err != nil {
		internalError(w, "jira/projects", err)
		return
	}
	cfg, ok := integrations.JiraSystemConfig(creds)
	if !ok {
		writeNotConfigured(w, "Jira is not connected for this workspace")
		return
	}

	upstream, err := jira.NewClient(cfg).ListProjects(r.Context(), q, startAt, page.Limit)
	if err != nil {
		serverLog.Warn("list jira projects failed", "org", orgID, "error", err)
		httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{
			Reason:  httpx.ReasonUpstreamUnavailable,
			Message: "failed to fetch projects from Jira" + httpx.LocalDetail(err),
		})
		return
	}
	items := make([]jiraProjectJSON, 0, len(upstream.Projects))
	for _, p := range upstream.Projects {
		items = append(items, jiraProjectJSON{Key: p.Key, Name: p.Name})
	}
	next := ""
	if upstream.NextStartAt > 0 {
		next = strconv.Itoa(upstream.NextStartAt)
	}
	httpx.WriteProxyList(w, page, items, next)
}

// jiraStartAtFromCursor decodes the offset the proxy page token carries. An
// empty cursor is the first page. Anything else must be a non-negative integer
// — the token is opaque and only this server mints one, so a value that isn't
// is a tampered or stale token, not a position.
func jiraStartAtFromCursor(v *httpx.Validation, cursor string) int {
	if cursor == "" {
		return 0
	}
	n, err := strconv.Atoi(cursor)
	if err != nil || n < 0 {
		v.Add(httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidParam,
			Message: "page_token is not a valid page token",
			Field:   "page_token",
		})
		return 0
	}
	return n
}
