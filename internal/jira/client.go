package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/trace"
)

// Deployment identifies which Jira backend a Config targets. It selects the
// default REST API version and is carried for callers that branch on Cloud
// vs Data Center behavior (e.g. the /myself identity sniff in currentUser).
type Deployment string

const (
	// DeploymentCloud is Atlassian Cloud (*.atlassian.net), REST v3.
	DeploymentCloud Deployment = "cloud"
	// DeploymentDataCenter is self-hosted Jira Server / Data Center, REST v2.
	DeploymentDataCenter Deployment = "data_center"
)

// DeploymentForHost classifies a Jira base URL (or bare host) as Cloud or
// Data Center from its hostname shape: every Atlassian Cloud site is served
// from "<site>.atlassian.net", so a host under that domain is Cloud and any
// other origin is a self-hosted Server / Data Center instance. This is the
// shared deployment-detection primitive the onboarding handler, the per-user
// bind flows, and the system resolver branch on, so the Cloud-vs-DC decision
// is made exactly one way across the codebase.
//
// The input may be a full base URL ("https://acme.atlassian.net/") or a bare
// host ("acme.atlassian.net"); the hostname is extracted (dropping any
// scheme, port, or path) and lowercased before matching, so none of those
// affect the result. The suffix is matched with a leading dot so a look-alike
// like "atlassian.net.evil.example" is correctly Data Center, not Cloud.
func DeploymentForHost(host string) Deployment {
	h := strings.ToLower(strings.TrimSpace(host))
	if u, err := url.Parse(h); err == nil && u.Hostname() != "" {
		// Full URL (has a scheme): Hostname() already drops the port.
		h = u.Hostname()
	} else {
		// Bare host (no scheme): url.Parse won't populate Hostname(), so strip
		// any path then a trailing :port by hand — mirroring the frontend
		// helper, so "acme.atlassian.net:443" classifies as Cloud and the two
		// sides can't drift.
		if i := strings.IndexByte(h, '/'); i >= 0 {
			h = h[:i]
		}
		if i := strings.LastIndexByte(h, ':'); i >= 0 {
			h = h[:i]
		}
	}
	if h == "atlassian.net" || strings.HasSuffix(h, ".atlassian.net") {
		return DeploymentCloud
	}
	return DeploymentDataCenter
}

// APIVersion is the Jira REST API major-version segment that goes into
// "/rest/api/{version}/".
type APIVersion string

const (
	APIv2 APIVersion = "2"
	APIv3 APIVersion = "3"
)

// AuthMethod records which credential scheme an org's stored Jira service
// credential uses. It is persisted alongside the credential (under the
// jira_auth_method secret key) so the resolver can rebuild the right client
// — Basic/v3 for Cloud, Bearer/v2 for Data Center — without re-sniffing the
// host on every read.
type AuthMethod string

const (
	// AuthMethodDCPAT is a Data Center / Server personal access token
	// (Bearer auth, REST v2) — the historical default, and the method an org
	// onboarded before Cloud support carries implicitly (no stored marker).
	AuthMethodDCPAT AuthMethod = "dc_pat"
	// AuthMethodCloudAPIToken is an Atlassian Cloud API token: Basic auth over
	// email + token, REST v3.
	AuthMethodCloudAPIToken AuthMethod = "cloud_api_token"
	// AuthMethodCloudOAuth is an Atlassian Cloud OAuth 3LO credential: a
	// rotating refresh token is the durable per-user secret, and a short-lived
	// Bearer access token (REST v3, against the api.atlassian.com gateway) is
	// minted from it on demand. Unlike the static schemes above, the stored
	// credential carries no token usable directly — it holds the refresh token
	// + the resolved cloud_id (see UserCredential).
	AuthMethodCloudOAuth AuthMethod = "cloud_oauth"
)

// CloudGatewayBaseURL is the api.atlassian.com OAuth gateway base for a
// resolved Cloud site: a CloudOAuth client talks to
// "https://api.atlassian.com/ex/jira/{cloud_id}/rest/api/3/..." rather than the
// raw "<site>.atlassian.net" host (which only speaks Basic/API-token auth). The
// cloud_id is the site identifier from the accessible-resources lookup.
func CloudGatewayBaseURL(cloudID string) string {
	return "https://api.atlassian.com/ex/jira/" + cloudID
}

// DeploymentForMarker resolves the effective deployment from a stored
// auth-method marker and the canonical host. The marker is authoritative when
// set; an empty marker (a pre-Cloud org, which predates the marker) falls back
// to host-shape detection via DeploymentForHost — so a *.atlassian.net host
// still resolves Cloud and any other origin Data Center. In practice every
// genuine pre-Cloud org is non-*.atlassian.net (Cloud onboarding always writes
// a marker), so the fallback preserves the historical Data Center behavior
// without hard-coding it. Shared by the system resolver and the request-path
// config builders (internal/integrations) so the Cloud-vs-DC decision is made
// one way.
func DeploymentForMarker(method AuthMethod, host string) Deployment {
	switch method {
	case AuthMethodCloudAPIToken, AuthMethodCloudOAuth:
		return DeploymentCloud
	case AuthMethodDCPAT:
		return DeploymentDataCenter
	default:
		return DeploymentForHost(host)
	}
}

// authScheme renders the Authorization header onto a request. It is
// unexported by design: callers pick a named constructor (DataCenterPAT,
// CloudAPIToken) instead of assembling a scheme themselves.
type authScheme interface{ apply(*http.Request) }

// bearerAuth is the "Authorization: Bearer <token>" scheme. It authenticates
// both Data Center PATs (DataCenterPAT) and Cloud OAuth 3LO access tokens
// (CloudOAuth) — an OAuth access token is also a bearer token, so the scheme is
// shared verbatim; only token acquisition/refresh and the gateway BaseURL (see
// Config.BaseURL) differ between the two.
type bearerAuth struct{ token string }

func (a bearerAuth) apply(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+a.token)
}

// basicAuth is the "Authorization: Basic base64(email:token)" scheme used by
// Atlassian Cloud API tokens, which are not bearer tokens.
type basicAuth struct{ email, token string }

func (a basicAuth) apply(req *http.Request) {
	req.SetBasicAuth(a.email, a.token)
}

// Config describes how to reach and authenticate against a Jira backend.
// Build one with a named constructor (DataCenterPAT / CloudAPIToken); the
// auth scheme is unexported so it can only be set that way.
type Config struct {
	// BaseURL is the site/host URL as given; it is treated as opaque. For the
	// Cloud OAuth flow (CloudOAuth) it holds the
	// api.atlassian.com/ex/jira/{cloud_id} gateway URL rather than the raw site
	// URL; no request-building code changes for that — the path stays
	// {BaseURL}/rest/api/{version}/...
	BaseURL    string
	Deployment Deployment
	APIVersion APIVersion
	auth       authScheme
}

// DataCenterPAT builds a Config for a Jira Server / Data Center personal
// access token: Bearer auth, REST v2. This is the only backend wired today
// and reproduces the historical client behavior exactly.
func DataCenterPAT(baseURL, pat string) Config {
	return Config{
		BaseURL:    baseURL,
		Deployment: DeploymentDataCenter,
		APIVersion: APIv2,
		auth:       bearerAuth{token: pat},
	}
}

// ProxyPlaceholder builds a Config for a Jira client that talks to a per-run
// credential proxy (internal/apiproxy) rather than to Jira directly: baseURL is
// the proxy's address and placeholder is the per-run token the proxy swaps for
// the org's real Cloud-Basic / Data-Center-Bearer auth on the upstream hop, so
// the client — and the process holding it — never touches a durable Jira
// credential (Property B). The placeholder is presented as a Bearer, which the
// proxy authenticates regardless of the org's real auth scheme.
//
// deployment is the org's real backend, relayed non-secret from the sidecar
// that holds the credential, so this client picks the same REST version the
// non-proxy path would (v3 for Cloud, v2 for Data Center) and an agent's
// writes land in the same shape whichever half of the split runs them. An
// unrecognized value resolves Data Center / v2, which both backends serve.
func ProxyPlaceholder(baseURL, placeholder string, deployment Deployment) Config {
	if deployment != DeploymentCloud {
		deployment = DeploymentDataCenter
	}
	return Config{
		BaseURL:    baseURL,
		Deployment: deployment,
		APIVersion: apiVersionFor(deployment),
		auth:       bearerAuth{token: placeholder},
	}
}

// apiVersionFor is the one place the deployment → REST version mapping lives:
// Cloud speaks v3 (Atlassian Document Format for rich text), Data Center v2.
func apiVersionFor(d Deployment) APIVersion {
	if d == DeploymentCloud {
		return APIv3
	}
	return APIv2
}

// CloudAPIToken builds a Config for an Atlassian Cloud API token: Basic auth
// (base64(email:token)), REST v3. Built by the system resolver (ForSystem) and
// the request-path config builder (integrations.JiraSystemConfig) once an org
// has onboarded a Cloud credential, and by validation at connect time.
func CloudAPIToken(baseURL, email, token string) Config {
	return Config{
		BaseURL:    baseURL,
		Deployment: DeploymentCloud,
		APIVersion: APIv3,
		auth:       basicAuth{email: email, token: token},
	}
}

// CloudOAuth builds a Config for an Atlassian Cloud OAuth 3LO access token:
// Bearer auth, REST v3, against the api.atlassian.com gateway for the resolved
// site (cloud_id). The access token is short-lived and minted on demand from
// the stored refresh token (see internal/jiraoauth); this constructor just
// wraps whatever access token the caller minted. The bearer scheme is shared
// with DataCenterPAT — only the gateway BaseURL + v3 differ — so no request-
// building code changes for the OAuth path.
func CloudOAuth(cloudID, accessToken string) Config {
	return Config{
		BaseURL:    CloudGatewayBaseURL(cloudID),
		Deployment: DeploymentCloud,
		APIVersion: APIv3,
		auth:       bearerAuth{token: accessToken},
	}
}

// restURL joins an already-formatted literal path onto the versioned REST
// base: {BaseURL}/rest/api/{APIVersion}/{path}. A trailing slash on BaseURL
// (if any) is trimmed so the result never contains "//rest".
func (cfg Config) restURL(path string) string {
	return fmt.Sprintf("%s/rest/api/%s/%s", strings.TrimRight(cfg.BaseURL, "/"), cfg.APIVersion, path)
}

// cloudSearchURL is the Cloud enhanced-search endpoint,
// {BaseURL}/rest/api/3/search/jql.
//
// The version is pinned rather than read from APIVersion. v3 is the version
// Atlassian's own removal notice points migrating callers at, and it is what
// every Cloud Config already resolves to; whether a v2 alias of this endpoint
// exists is not something the published material settles, so nothing here
// depends on one. A hand-built Config that set APIVersion to v2 for a Cloud
// site would otherwise aim this at an endpoint that may not answer.
func (cfg Config) cloudSearchURL() string {
	return fmt.Sprintf("%s/rest/api/%s/search/jql", strings.TrimRight(cfg.BaseURL, "/"), APIv3)
}

// apiURL builds a versioned REST URL from a fmt format string + args, e.g.
// apiURL("issue/%s", key) → {BaseURL}/rest/api/{APIVersion}/issue/{key}.
func (cfg Config) apiURL(format string, args ...any) string {
	return cfg.restURL(fmt.Sprintf(format, args...))
}

// ReachabilityURL returns a no-auth probe URL on the same host a Client would
// use, for the setup wizard's URL-only reachability sub-step (split from the
// credential sub-step): {base}/rest/api/2/serverInfo. The endpoint exists on
// both Cloud and Data Center, and the probe only needs the host to *answer* —
// any HTTP status (including a 401 on a Cloud site that gates serverInfo
// behind auth) proves reachability — so the response is irrelevant. v2 is
// hard-wired (not cfg.APIVersion) because reachability is creds-free and so
// has no Config; v2 is served by every Jira backend. Kept here so the REST
// path shape stays owned by this package.
func ReachabilityURL(baseURL string) string {
	return fmt.Sprintf("%s/rest/api/2/serverInfo", strings.TrimRight(baseURL, "/"))
}

// richTextValue renders a rich-text field (a comment body, an issue
// description) in the shape this backend's REST version accepts. v2 takes the
// wiki-markup string as-is and parses it server-side; v3 takes only an
// Atlassian Document Format object and rejects a string outright, so the markup
// is converted here (see adf.go) rather than shipped flat.
//
// Empty renders as JSON null on v3 rather than as a document, because ADF has
// no empty document and an empty content array is a schema violation Jira
// rejects before it ever looks at the field. What an empty value then *means*
// is the field's business, not this function's: on a description it clears the
// field (as the v2 empty string does), while on a comment body Jira rejects it
// on both versions. Nothing here makes an empty comment a valid operation — it
// only keeps the resulting failure Jira's own.
func (cfg Config) richTextValue(s string) any {
	if cfg.APIVersion != APIv3 {
		return s
	}
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return wikiToADF(s)
}

// authorize applies the configured auth scheme to req. A Config built
// without a named constructor (DataCenterPAT / CloudAPIToken) has a nil
// scheme; because Config's other fields are exported and can be set by hand,
// surface that as a clear error rather than a nil-pointer panic.
func (cfg Config) authorize(req *http.Request) error {
	if cfg.auth == nil {
		return fmt.Errorf("jira: Config has no auth scheme - build it with DataCenterPAT or CloudAPIToken")
	}
	cfg.auth.apply(req)
	return nil
}

// NewAPIRequest builds an *http.Request against
// {BaseURL}/rest/api/{APIVersion}/{path} with this Config's Content-Type and
// Authorization header applied. body may be nil. It is the seam external
// validators (auth.ValidateJira) use to probe an endpoint with the same
// scheme + version a Client would, without reconstructing the (unexported)
// auth scheme.
func (cfg Config) NewAPIRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, cfg.restURL(path), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := cfg.authorize(req); err != nil {
		return nil, err
	}
	return req, nil
}

// Client wraps the Jira REST API. The API version and auth scheme are
// carried by cfg — see Config and the named constructors.
type Client struct {
	cfg     Config
	http    *http.Client
	selfMu  sync.RWMutex
	selfVal *currentUserResponse
}

// NewClient builds a Client from a Config. Construct the Config with a named
// constructor, e.g. NewClient(DataCenterPAT(url, pat)).
// Every Jira call in the process is built here — there is no
// caller-supplied-client constructor and no second place a *Client comes
// from — so instrumenting this one transport covers the whole surface.
func NewClient(cfg Config) *Client {
	return &Client{
		cfg:  cfg,
		http: telemetry.TracedHTTPClient(15*time.Second, "jira"),
	}
}

// apiURL builds a versioned REST URL for this client's backend, e.g.
// c.apiURL("issue/%s", key) → {BaseURL}/rest/api/{version}/issue/{key}.
func (c *Client) apiURL(format string, args ...any) string {
	return c.cfg.apiURL(format, args...)
}

// Status represents a Jira workflow status.
type Status struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ProjectStatuses returns all unique statuses available in a project,
// deduplicated across issue types.
func (c *Client) ProjectStatuses(ctx context.Context, projectKey string) ([]Status, error) {
	url := c.apiURL("project/%s/statuses", projectKey)
	body, err := c.get(ctx, url)
	if err != nil {
		return nil, err
	}

	// Response is an array of issue types, each with a statuses array.
	var issueTypes []struct {
		Statuses []Status `json:"statuses"`
	}
	if err := json.Unmarshal(body, &issueTypes); err != nil {
		return nil, fmt.Errorf("parse project statuses: %w", err)
	}

	seen := map[string]bool{}
	var result []Status
	for _, it := range issueTypes {
		for _, s := range it.Statuses {
			if !seen[s.Name] {
				seen[s.Name] = true
				result = append(result, s)
			}
		}
	}
	return result, nil
}

// AssignToSelf assigns the issue to the authenticated user (currentUser).
func (c *Client) AssignToSelf(ctx context.Context, issueKey string) error {
	url := c.apiURL("issue/%s/assignee", issueKey)
	// Setting name to "-1" assigns to the current user in Jira Server/DC.
	// For Jira Cloud, we need accountId. We'll try the myself endpoint first.
	myself, err := c.currentUser(ctx)
	if err != nil {
		return fmt.Errorf("get current user: %w", err)
	}

	payload := map[string]string{}
	if myself.AccountID != "" {
		// Jira Cloud
		payload["accountId"] = myself.AccountID
	} else {
		// Jira Server/DC
		payload["name"] = myself.Name
	}

	return c.put(ctx, url, payload)
}

// Unassign removes the assignee from an issue.
func (c *Client) Unassign(ctx context.Context, issueKey string) error {
	url := c.apiURL("issue/%s/assignee", issueKey)
	// Detect Cloud vs Server the same way AssignToSelf does.
	myself, err := c.currentUser(ctx)
	if err != nil {
		return fmt.Errorf("get current user: %w", err)
	}
	if myself.AccountID != "" {
		// Jira Cloud: null accountId clears assignee
		return c.put(ctx, url, map[string]*string{"accountId": nil})
	}
	// Jira Server/DC: null name clears assignee
	return c.put(ctx, url, map[string]*string{"name": nil})
}

// TransitionTo transitions an issue into the target status.
//
// The target is matched by ID when it carries one: a Jira workflow references
// the status entity, so an id keeps resolving across a rename while a name
// captured earlier goes stale. A target with no id — a status a caller named
// directly, or a rule armed before statuses were identified — falls back to
// the case-insensitive name match, which is what this always did.
func (c *Client) TransitionTo(ctx context.Context, issueKey string, target Status) error {
	transitions, err := c.getTransitions(ctx, issueKey)
	if err != nil {
		return err
	}

	for _, t := range transitions {
		if target.ID != "" {
			if t.To.ID == target.ID {
				return c.doTransition(ctx, issueKey, t.ID)
			}
			continue
		}
		if strings.EqualFold(t.To.Name, target.Name) {
			return c.doTransition(ctx, issueKey, t.ID)
		}
	}

	available := make([]string, len(transitions))
	for i, t := range transitions {
		available[i] = t.To.Name
	}
	wanted := target.Name
	if wanted == "" {
		wanted = "status id " + target.ID
	}
	return fmt.Errorf("no transition to %q found (available: %s)", wanted, strings.Join(available, ", "))
}

// ClaimState describes the assignee + status of a Jira issue, used by
// claim guards to skip redundant API mutations on multi-task entities.
type ClaimState struct {
	AssignedToSelf bool
	Unassigned     bool   // true when assignee is null (no one assigned)
	StatusName     string // current workflow status
}

// GetClaimState fetches the current assignee and status of an issue and
// checks whether the assignee is the authenticated user. Returns nil on
// any error — callers treat failure as "unknown, proceed normally".
func (c *Client) GetClaimState(ctx context.Context, issueKey string) *ClaimState {
	// Fetch only assignee + status to minimize payload. The ?fields param
	// works identically on Cloud and Server/DC.
	url := c.apiURL("issue/%s?fields=assignee,status", issueKey)
	body, err := c.get(ctx, url)
	if err != nil {
		// A cancelled/expired ctx (e.g. the requesting client disconnected)
		// is expected, not a failure worth logging — suppress the noise.
		if ctx.Err() == nil {
			jiraLog.Warn("claim guard: fetch issue failed", "issue", issueKey, "error", err)
		}
		return nil
	}
	var issue Issue
	if err := json.Unmarshal(body, &issue); err != nil {
		jiraLog.Warn("claim guard: parse issue failed", "issue", issueKey, "error", err)
		return nil
	}

	myself, err := c.currentUser(ctx)
	if err != nil {
		if ctx.Err() == nil {
			jiraLog.Warn("claim guard: get current user failed", "error", err)
		}
		return nil
	}

	state := &ClaimState{}
	if issue.Fields.Status != nil {
		state.StatusName = issue.Fields.Status.Name
	}
	if issue.Fields.Assignee == nil {
		state.Unassigned = true
	} else {
		// Compare via the shared stable-id precedence (accountId → key → name)
		// so "assigned to me" agrees with the identity the router and
		// user_jira_identities key on. Both operands come from the same live
		// instance here, but deriving them identically keeps this in lockstep
		// with StableID() and the snapshot path — no field can be preferred on
		// one side and not the other.
		self := StableUserID(myself.AccountID, myself.Key, myself.Name)
		assignee := StableUserID(issue.Fields.Assignee.AccountID, issue.Fields.Assignee.Key, issue.Fields.Assignee.Name)
		state.AssignedToSelf = self != "" && self == assignee
	}
	return state
}

// Issue represents core fields of a Jira issue.
type Issue struct {
	Key    string `json:"key"`
	Self   string `json:"self"`
	Fields struct {
		Summary     string          `json:"summary"`
		Description json.RawMessage `json:"description"`
		Status      *Status         `json:"status,omitempty"`
		IssueType   *struct {
			Name string `json:"name"`
		} `json:"issuetype,omitempty"`
		Priority *struct {
			Name string `json:"name"`
		} `json:"priority,omitempty"`
		Assignee *struct {
			DisplayName string `json:"displayName"`
			AccountID   string `json:"accountId"`
			Key         string `json:"key"`
			Name        string `json:"name"`
		} `json:"assignee,omitempty"`
		Parent *struct {
			Key string `json:"key"`
		} `json:"parent,omitempty"`
		Labels  []string `json:"labels,omitempty"`
		Comment *struct {
			Total int `json:"total"`
		} `json:"comment,omitempty"`
		Created string `json:"created,omitempty"`
		// Updated is Jira's last-modified timestamp on the issue. Bumped
		// every time any field changes (status, assignee, priority, comment
		// added). Used by the diff layer as a fallback source time for
		// events that don't have their own per-action timestamp on the
		// snapshot — better than "we noticed it now" without firing a
		// separate changelog API call.
		Updated string `json:"updated,omitempty"`
		// Subtasks inlined in the search response. Each element has its own
		// minimal fields block with a Status; we only need .Fields.Status.Name
		// to decide whether the subtask is still open. If Jira returns
		// partial fields (older Server versions), missing Status just means
		// we can't classify — treat as open to stay conservative.
		Subtasks []struct {
			Key    string `json:"key"`
			Fields struct {
				Status *Status `json:"status,omitempty"`
			} `json:"fields"`
		} `json:"subtasks,omitempty"`
	} `json:"fields"`
}

// IssueType represents a Jira issue type for a project.
type IssueType struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Subtask bool   `json:"subtask"`
	IconURL string `json:"iconUrl,omitempty"`
}

// Transition represents an available workflow transition.
type Transition struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	To   Status `json:"to"`
}

// Priority represents a Jira priority level.
type Priority struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListPriorities returns all priority levels configured on the instance.
func (c *Client) ListPriorities(ctx context.Context) ([]Priority, error) {
	body, err := c.get(ctx, c.apiURL("priority"))
	if err != nil {
		return nil, err
	}
	var priorities []Priority
	if err := json.Unmarshal(body, &priorities); err != nil {
		return nil, fmt.Errorf("parse priorities: %w", err)
	}
	return priorities, nil
}

// Project is one entry of the instance's project catalog: the key rules and
// JQL are written against, plus the display name a picker renders beside it.
// Nothing more — this catalog is read live for a picker, not mirrored for
// anything downstream to consume.
type Project struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// ProjectPage is one window of the catalog plus the offset the following
// window resumes from. NextStartAt is 0 when this was the last page, so a
// caller walks until it stops rather than asking for a window it already
// knows is empty.
type ProjectPage struct {
	Projects    []Project
	NextStartAt int
}

// maxProjectPageSize caps one ListProjects window. Cloud's catalog endpoint
// caps its own page at 50 and silently serves fewer than asked; the cap here
// is about the Data Center arm, which windows a whole catalog held in memory
// and would otherwise hand back however many rows a caller asked for.
const maxProjectPageSize = 200

// ListProjects returns one window of the projects this credential can see,
// narrowed by query — a case-insensitive match against the key and the name.
//
// The two deployments answer differently, and this is the one place that
// difference lives. Cloud has a paged, server-filtered catalog endpoint
// (/project/search, taking startAt + maxResults + query), so both the window
// and the filter go upstream. Data Center serves the whole catalog from
// /project in a single response with neither, so both are applied here to the
// catalog in hand. Callers get one signature, one page, and one resume offset
// either way.
func (c *Client) ListProjects(ctx context.Context, query string, startAt, maxResults int) (ProjectPage, error) {
	if startAt < 0 {
		startAt = 0
	}
	if maxResults <= 0 || maxResults > maxProjectPageSize {
		maxResults = maxProjectPageSize
	}
	if c.cfg.Deployment == DeploymentCloud {
		return c.listProjectsCloud(ctx, query, startAt, maxResults)
	}
	return c.listProjectsDataCenter(ctx, query, startAt, maxResults)
}

// listProjectsCloud reads one page of /project/search, which pages on startAt
// offsets and filters on a literal `query` matched against key and name.
func (c *Client) listProjectsCloud(ctx context.Context, query string, startAt, maxResults int) (ProjectPage, error) {
	params := url.Values{}
	params.Set("startAt", strconv.Itoa(startAt))
	params.Set("maxResults", strconv.Itoa(maxResults))
	if q := strings.TrimSpace(query); q != "" {
		params.Set("query", q)
	}
	body, err := c.get(ctx, c.apiURL("project/search")+"?"+params.Encode())
	if err != nil {
		return ProjectPage{}, err
	}
	var result struct {
		Values     []Project `json:"values"`
		IsLast     *bool     `json:"isLast"`
		Total      *int      `json:"total"`
		MaxResults int       `json:"maxResults"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return ProjectPage{}, fmt.Errorf("parse project search: %w", err)
	}
	// isLast is what this endpoint documents as its end-of-catalog signal, and
	// total settles the question outright when it is present instead. A response
	// carrying neither falls back to the short-page test, measured against the
	// window the server actually served rather than the one asked for: this
	// endpoint caps its own page and echoes that cap back in maxResults, so a
	// caller asking for more than the cap is handed a page that is short by the
	// ask while catalog remains. Either way an empty page ends the walk, so a
	// backend ignoring startAt can't loop.
	more := len(result.Values) > 0
	switch {
	case result.IsLast != nil:
		more = more && !*result.IsLast
	case result.Total != nil:
		more = more && startAt+len(result.Values) < *result.Total
	default:
		served := result.MaxResults
		if served <= 0 {
			served = maxResults
		}
		more = more && len(result.Values) >= served
	}
	page := ProjectPage{Projects: result.Values}
	if more {
		page.NextStartAt = startAt + len(result.Values)
	}
	return page, nil
}

// listProjectsDataCenter reads the whole catalog from /project — this backend
// serves it unpaged and unfiltered — then applies the caller's filter and
// window to what came back.
func (c *Client) listProjectsDataCenter(ctx context.Context, query string, startAt, maxResults int) (ProjectPage, error) {
	body, err := c.get(ctx, c.apiURL("project"))
	if err != nil {
		return ProjectPage{}, err
	}
	var catalog []Project
	if err := json.Unmarshal(body, &catalog); err != nil {
		return ProjectPage{}, fmt.Errorf("parse projects: %w", err)
	}
	needle := strings.ToLower(strings.TrimSpace(query))
	matched := make([]Project, 0, len(catalog))
	for _, p := range catalog {
		if needle == "" ||
			strings.Contains(strings.ToLower(p.Key), needle) ||
			strings.Contains(strings.ToLower(p.Name), needle) {
			matched = append(matched, p)
		}
	}
	if startAt >= len(matched) {
		return ProjectPage{Projects: []Project{}}, nil
	}
	end := min(startAt+maxResults, len(matched))
	page := ProjectPage{Projects: matched[startAt:end]}
	if end < len(matched) {
		page.NextStartAt = end
	}
	return page, nil
}

// GetIssue fetches a single issue by key.
func (c *Client) GetIssue(ctx context.Context, issueKey string) (*Issue, error) {
	url := c.apiURL("issue/%s", issueKey)
	body, err := c.get(ctx, url)
	if err != nil {
		return nil, err
	}
	var issue Issue
	if err := json.Unmarshal(body, &issue); err != nil {
		return nil, fmt.Errorf("parse issue: %w", err)
	}
	return &issue, nil
}

// GetChildIssues returns all child issues of a parent (subtasks + epic children).
// On Cloud, parent = KEY covers both. On Server/DC, we also query the Epic Link
// custom field. Results are deduplicated by key.
func (c *Client) GetChildIssues(ctx context.Context, parentKey string) ([]Issue, error) {
	seen := map[string]bool{}
	var result []Issue

	// Query 1: direct parent relationship (Cloud + Server/DC subtasks)
	issues, err := c.SearchIssues(ctx, fmt.Sprintf("parent = %s ORDER BY created ASC", parentKey), nil, 100)
	if err != nil {
		return nil, err
	}
	for _, issue := range issues {
		if !seen[issue.Key] {
			seen[issue.Key] = true
			result = append(result, issue)
		}
	}

	// Query 2: Epic Link (Server/DC epic children)
	epicField, err := c.epicLinkField(ctx)
	if err != nil {
		jiraLog.Warn("epic link field discovery failed", "error", err)
	} else if epicField != "" {
		epicIssues, err := c.SearchIssues(ctx, fmt.Sprintf("cf[%s] = %s ORDER BY created ASC", extractFieldID(epicField), parentKey), nil, 100)
		if err != nil {
			jiraLog.Warn("epic link query failed", "parent", parentKey, "error", err)
		} else {
			for _, issue := range epicIssues {
				if !seen[issue.Key] {
					seen[issue.Key] = true
					result = append(result, issue)
				}
			}
		}
	}

	return result, nil
}

// DefaultSearchFields is the default set of fields returned by SearchIssues.
var DefaultSearchFields = []string{"summary", "description", "status", "issuetype", "priority", "assignee", "parent", "labels", "created", "updated"}

// ExtractDescriptionText flattens a Jira issue description to plain text.
// Server/DC returns description as a JSON string; Cloud returns an ADF
// document ({type:"doc", content:[...]}). Returns empty string for null,
// missing, or unparseable input. Caller should truncate as needed — Jira
// descriptions can be arbitrarily large.
func ExtractDescriptionText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	// Try plain string first (Server/DC).
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	// Otherwise assume ADF — recursively collect every `text` field.
	var node map[string]any
	if err := json.Unmarshal(raw, &node); err != nil {
		return ""
	}
	var sb strings.Builder
	walkADF(node, &sb)
	return strings.TrimSpace(sb.String())
}

// walkADF recursively extracts text content from an ADF node. Paragraphs,
// list items, and headings get a trailing newline so the output is readable
// rather than one long run-on line.
func walkADF(node map[string]any, sb *strings.Builder) {
	if text, ok := node["text"].(string); ok {
		sb.WriteString(text)
	}
	if content, ok := node["content"].([]any); ok {
		for _, c := range content {
			if child, ok := c.(map[string]any); ok {
				walkADF(child, sb)
			}
		}
	}
	// Block-level nodes get a separator so output reads as multi-line prose.
	switch node["type"] {
	case "paragraph", "heading", "listItem", "codeBlock", "blockquote":
		sb.WriteString("\n")
	}
}

// maxSearchPages bounds the round trips one SearchIssues call will make.
// Both deployments' end-of-results signal comes from the server, so a backend
// that keeps handing back a page token — or one that ignores startAt and
// replays the same page — would otherwise spin here indefinitely. The budget
// sits far above what any caller needs: the largest ask in the codebase is one
// 100-key batch, which even a heavily clamped page size covers in a handful of
// pages.
const maxSearchPages = 50

// cloudSearchMaxPageSize is the ceiling Jira Cloud documents for a single
// enhanced-search page. Cloud clamps the requested page size server-side
// anyway — tighter the more fields are asked for — so this only keeps an
// oversized caller-supplied cap from being sent verbatim.
const cloudSearchMaxPageSize = 5000

// SearchIssues runs a JQL query and returns matching issues, following the
// backend's pagination until maxResults is satisfied or the results run out.
// If fields is nil, DefaultSearchFields is used. Pass []string{"*all"} for everything.
//
// The two deployments answer JQL at different endpoints. Atlassian removed
// /rest/api/{2,3}/search from Cloud — it answers 410 Gone — in favour of the
// enhanced /rest/api/3/search/jql, which pages on an opaque nextPageToken
// instead of startAt offsets and reports no total. Data Center follows its own
// lifecycle and still serves /search, so it keeps the request it has always
// been sent. Callers see one signature and one issue slice either way.
func (c *Client) SearchIssues(ctx context.Context, jql string, fields []string, maxResults int) ([]Issue, error) {
	if fields == nil {
		// More than a convenience on Cloud: the enhanced endpoint returns the
		// issue id alone when fields is absent, rather than a full issue, so
		// the default has to be filled in here and not left to the server.
		fields = DefaultSearchFields
	}
	if maxResults <= 0 {
		maxResults = 100
	}
	if c.cfg.Deployment == DeploymentCloud {
		return c.searchCloud(ctx, jql, fields, maxResults)
	}
	return c.searchDataCenter(ctx, jql, fields, maxResults)
}

// searchPaged drives the page walk both deployments share: it calls fetch for
// successive pages, stops as soon as the caller's cap is met or the backend
// reports nothing further, and trims to the cap. fetch receives the number of
// issues collected so far — the offset one deployment pages on, and what to
// subtract from the outstanding ask on both — and reports whether another page
// is worth asking for.
func (c *Client) searchPaged(ctx context.Context, jql string, maxResults int, fetch func(collected int) ([]Issue, bool, error)) ([]Issue, error) {
	var all []Issue
	for page := 0; page < maxSearchPages; page++ {
		issues, more, err := fetch(len(all))
		if err != nil {
			return nil, err
		}
		all = append(all, issues...)
		if !more || len(all) >= maxResults {
			if len(all) > maxResults {
				all = all[:maxResults]
			}
			return all, nil
		}
	}
	// Exhausting the budget means the backend was still claiming more results.
	// Say so — a silently short answer reads downstream as "the query matched
	// this much", which is how a paging bug turns into entities that quietly
	// stop moving.
	jiraLog.WarnContext(ctx, "jira search stopped at page budget",
		"pages", maxSearchPages, "collected", len(all), "requested", maxResults, "jql", jql)
	return all, nil
}

// searchCloud runs the JQL through the enhanced endpoint, walking
// nextPageToken until the caller's cap is met.
func (c *Client) searchCloud(ctx context.Context, jql string, fields []string, maxResults int) ([]Issue, error) {
	url := c.cfg.cloudSearchURL()
	var token string
	return c.searchPaged(ctx, jql, maxResults, func(collected int) ([]Issue, bool, error) {
		payload := map[string]any{
			"jql":        jql,
			"maxResults": min(maxResults-collected, cloudSearchMaxPageSize),
			"fields":     fields,
		}
		if token != "" {
			payload["nextPageToken"] = token
		}
		respBody, err := c.postJSON(ctx, url, payload, true)
		if err != nil {
			return nil, false, err
		}
		var result struct {
			Issues        []Issue `json:"issues"`
			NextPageToken string  `json:"nextPageToken"`
			IsLast        *bool   `json:"isLast"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, false, fmt.Errorf("parse search results: %w", err)
		}
		token = result.NextPageToken
		// The token is the load-bearing signal: its absence is what Atlassian
		// documents as "that was the last page", and isLast is not reliably
		// present on every response. An explicit isLast:true still ends the
		// walk, so a backend that sends both agrees with itself.
		return result.Issues, token != "" && (result.IsLast == nil || !*result.IsLast), nil
	})
}

// searchDataCenter runs the JQL through the classic endpoint, walking startAt
// offsets until the caller's cap is met.
func (c *Client) searchDataCenter(ctx context.Context, jql string, fields []string, maxResults int) ([]Issue, error) {
	url := c.apiURL("search")
	return c.searchPaged(ctx, jql, maxResults, func(collected int) ([]Issue, bool, error) {
		payload := map[string]any{
			"jql":        jql,
			"maxResults": maxResults - collected,
			"fields":     fields,
		}
		// Omitted on the first page, so the request Data Center has always
		// received is unchanged; an offset appears only once a second page is
		// actually needed.
		if collected > 0 {
			payload["startAt"] = collected
		}
		respBody, err := c.postJSON(ctx, url, payload, true)
		if err != nil {
			return nil, false, err
		}
		var result struct {
			Issues []Issue `json:"issues"`
			Total  int     `json:"total"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, false, fmt.Errorf("parse search results: %w", err)
		}
		// The reported total is what licenses another page: this endpoint
		// always carries one, so "the rows so far don't add up to the total"
		// is both the cheapest end-of-results test (no round trip spent
		// discovering an empty page) and the safe one. A response without a
		// total is treated as complete — the behaviour this deployment has
		// always had — rather than walked on an empty-page guess that a
		// backend ignoring startAt would turn into a loop.
		more := len(result.Issues) > 0 && collected+len(result.Issues) < result.Total
		return result.Issues, more, nil
	})
}

// extractFieldID pulls the numeric ID from a custom field name like "customfield_10008".
func extractFieldID(field string) string {
	return strings.TrimPrefix(field, "customfield_")
}

// AddComment posts a comment on an issue and returns the new comment's id.
// The id is best-effort: if the POST succeeds (2xx) but the response body
// can't be parsed, the comment still landed, so we return an empty id with no
// error rather than failing an action that already took effect. Callers that
// only care about success can ignore the id.
func (c *Client) AddComment(ctx context.Context, issueKey, body string) (string, error) {
	url := c.apiURL("issue/%s/comment", issueKey)
	respBody, err := c.postJSON(ctx, url, map[string]any{"body": c.cfg.richTextValue(body)}, false)
	if err != nil {
		return "", err
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", nil
	}
	return result.ID, nil
}

// GetTransitions returns the available workflow transitions for an issue.
func (c *Client) GetTransitions(ctx context.Context, issueKey string) ([]Transition, error) {
	return c.getTransitions(ctx, issueKey)
}

// ListIssueTypes returns the issue types available in a project.
func (c *Client) ListIssueTypes(ctx context.Context, projectKey string) ([]IssueType, error) {
	url := c.apiURL("project/%s", projectKey)
	body, err := c.get(ctx, url)
	if err != nil {
		return nil, err
	}
	var project struct {
		IssueTypes []IssueType `json:"issueTypes"`
	}
	if err := json.Unmarshal(body, &project); err != nil {
		return nil, fmt.Errorf("parse project: %w", err)
	}
	return project.IssueTypes, nil
}

// CreateIssue creates a new issue. parentKey and priority are optional (pass empty to skip).
func (c *Client) CreateIssue(ctx context.Context, projectKey, issueType, summary, description, parentKey, priority string) (string, error) {
	fields := map[string]any{
		"project":   map[string]string{"key": projectKey},
		"issuetype": map[string]string{"name": issueType},
		"summary":   summary,
	}
	if description != "" {
		fields["description"] = c.cfg.richTextValue(description)
	}
	if priority != "" {
		fields["priority"] = map[string]string{"name": priority}
	}

	if parentKey != "" {
		fields["parent"] = map[string]string{"key": parentKey}
	}

	payload := map[string]any{"fields": fields}
	createURL := c.apiURL("issue")
	respBody, err := c.postJSON(ctx, createURL, payload, false)

	// If parent field failed on Server/DC, retry with Epic Link
	if err != nil && parentKey != "" {
		if strings.Contains(err.Error(), "gh.epic.error") || strings.Contains(err.Error(), "parent") {
			delete(fields, "parent")
			epicField, epicErr := c.epicLinkField(ctx)
			if epicErr == nil && epicField != "" {
				fields[epicField] = parentKey
				payload = map[string]any{"fields": fields}
				respBody, err = c.postJSON(ctx, createURL, payload, false)
			}
		}
	}
	if err != nil {
		return "", err
	}

	var result struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse create response: %w", err)
	}
	return result.Key, nil
}

// SetPriority updates the priority of an issue.
func (c *Client) SetPriority(ctx context.Context, issueKey, priority string) error {
	url := c.apiURL("issue/%s", issueKey)
	return c.put(ctx, url, map[string]any{"fields": map[string]any{
		"priority": map[string]string{"name": priority},
	}})
}

// UpdateIssueFields describes the fields to mutate on an existing issue.
// Nil pointers mean "leave the field untouched"; an empty string means
// "set the field to empty" (Jira accepts this for description, less so
// for summary). AddLabels / RemoveLabels are processed via Jira's
// `update` operation so existing labels not mentioned are preserved.
type UpdateIssueFields struct {
	Summary      *string
	Description  *string
	Priority     *string
	IssueType    *string
	AddLabels    []string
	RemoveLabels []string
}

// IsEmpty reports whether the update would be a no-op.
func (u UpdateIssueFields) IsEmpty() bool {
	return u.Summary == nil &&
		u.Description == nil &&
		u.Priority == nil &&
		u.IssueType == nil &&
		len(u.AddLabels) == 0 &&
		len(u.RemoveLabels) == 0
}

// UpdateIssue mutates an existing issue. Only fields explicitly set on
// `fields` are touched; everything else is preserved. Returns an error
// if no fields were provided.
func (c *Client) UpdateIssue(ctx context.Context, issueKey string, f UpdateIssueFields) error {
	if f.IsEmpty() {
		return fmt.Errorf("no fields to update")
	}

	fields := map[string]any{}
	if f.Summary != nil {
		fields["summary"] = *f.Summary
	}
	if f.Description != nil {
		fields["description"] = c.cfg.richTextValue(*f.Description)
	}
	if f.Priority != nil {
		fields["priority"] = map[string]string{"name": *f.Priority}
	}
	if f.IssueType != nil {
		fields["issuetype"] = map[string]string{"name": *f.IssueType}
	}

	payload := map[string]any{}
	if len(fields) > 0 {
		payload["fields"] = fields
	}

	if len(f.AddLabels) > 0 || len(f.RemoveLabels) > 0 {
		ops := make([]map[string]string, 0, len(f.AddLabels)+len(f.RemoveLabels))
		for _, l := range f.AddLabels {
			ops = append(ops, map[string]string{"add": l})
		}
		for _, l := range f.RemoveLabels {
			ops = append(ops, map[string]string{"remove": l})
		}
		payload["update"] = map[string]any{"labels": ops}
	}

	url := c.apiURL("issue/%s", issueKey)
	return c.put(ctx, url, payload)
}

// SetParent links an existing issue under a parent.
// Tries fields.parent first (works for Cloud + Server/DC subtasks).
// Falls back to Epic Link custom field on Server/DC if parent is an Epic.
func (c *Client) SetParent(ctx context.Context, issueKey, parentKey string) error {
	url := c.apiURL("issue/%s", issueKey)

	// Try native parent field first
	err := c.put(ctx, url, map[string]any{"fields": map[string]any{
		"parent": map[string]string{"key": parentKey},
	}})
	if err == nil {
		return nil
	}

	// Fall back to Epic Link if the parent field failed
	if strings.Contains(err.Error(), "gh.epic.error") || strings.Contains(err.Error(), "parent") {
		epicField, epicErr := c.epicLinkField(ctx)
		if epicErr == nil && epicField != "" {
			return c.put(ctx, url, map[string]any{"fields": map[string]any{
				epicField: parentKey,
			}})
		}
	}

	return err
}

// epicLinkField discovers the custom field ID for Epic Link on Server/DC.
// It looks for the field with schema type "com.pyxis.greenhopper.jira:gh-epic-link".
// Returns empty string (not an error) if not found.
func (c *Client) epicLinkField(ctx context.Context) (string, error) {
	body, err := c.get(ctx, c.apiURL("field"))
	if err != nil {
		return "", err
	}

	var fields []struct {
		ID     string `json:"id"`
		Schema struct {
			Custom string `json:"custom"`
		} `json:"schema"`
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		return "", fmt.Errorf("parse fields: %w", err)
	}

	for _, f := range fields {
		if f.Schema.Custom == "com.pyxis.greenhopper.jira:gh-epic-link" {
			return f.ID, nil
		}
	}
	return "", nil
}

// --- internal helpers ---

type currentUserResponse struct {
	Name      string `json:"name"`      // Jira Server/DC username
	Key       string `json:"key"`       // Jira Server/DC internal user key
	AccountID string `json:"accountId"` // Jira Cloud
}

// StableUserID returns the deployment-appropriate stable identifier for a Jira
// user, with a single precedence shared by every surface that derives one:
// accountId (Jira Cloud) → key (Jira Server/DC internal user key, e.g.
// JIRAUSER12345) → name (the mutable username, often an email, last resort).
//
// This is the ONE source of truth for that precedence. auth.JiraUser.StableID()
// (what writes user_jira_identities.account_id at bind time), the snapshot
// derivation in internal/tracker (what writes events.assignee_account_id), and
// the claim guard below all route through it, so the value the router joins on
// (assignee-centric owner routing) can never drift from the identity it's
// matched against. A name-only fallback while the stored identity holds the key
// silently broke that join on Server/DC — events landed, no task was created.
func StableUserID(accountID, key, name string) string {
	switch {
	case accountID != "":
		return accountID
	case key != "":
		return key
	default:
		return name
	}
}

// currentUser resolves and caches the authenticated user's identity
// (accountId on Cloud, name on Server/DC). The result is cached for the
// client's lifetime on first success. Failures are deliberately NOT cached:
// because ctx now flows in, caching a transient error — especially a
// cancelled/expired ctx from one request — would otherwise poison identity
// resolution for every later caller. The next call simply retries.
func (c *Client) currentUser(ctx context.Context) (*currentUserResponse, error) {
	// Fast path: once cached, concurrent callers read under the read lock
	// and never contend on (or duplicate) the /myself fetch.
	c.selfMu.RLock()
	cached := c.selfVal
	c.selfMu.RUnlock()
	if cached != nil {
		return cached, nil
	}

	// Slow path: hold the write lock for the one-time fetch, re-checking
	// after acquiring it in case another caller populated the cache while we
	// waited. Holding it across the fetch also collapses a concurrent first-
	// call stampede into a single /myself round-trip.
	c.selfMu.Lock()
	defer c.selfMu.Unlock()
	if c.selfVal != nil {
		return c.selfVal, nil
	}
	body, err := c.get(ctx, c.apiURL("myself"))
	if err != nil {
		return nil, err
	}
	var user currentUserResponse
	if err := json.Unmarshal(body, &user); err != nil {
		return nil, fmt.Errorf("parse myself: %w", err)
	}
	c.selfVal = &user
	return c.selfVal, nil
}

func (c *Client) getTransitions(ctx context.Context, issueKey string) ([]Transition, error) {
	url := c.apiURL("issue/%s/transitions", issueKey)
	body, err := c.get(ctx, url)
	if err != nil {
		return nil, err
	}

	var result struct {
		Transitions []Transition `json:"transitions"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse transitions: %w", err)
	}
	return result.Transitions, nil
}

func (c *Client) doTransition(ctx context.Context, issueKey, transitionID string) error {
	url := c.apiURL("issue/%s/transitions", issueKey)
	payload := map[string]any{
		"transition": map[string]string{"id": transitionID},
	}
	return c.post(ctx, url, payload)
}

// doRequest issues method+url (with an optional JSON body) and returns the
// final response's status and fully-read body after any rate-limit-aware
// retries. It is the shared core behind get/put/postJSON/post.
//
// Each of those keeps its own status-to-error mapping, so the "returned <code>:
// <body>" strings a caller matches on (e.g. CreateIssue's parent/epic-link
// fallback) are unchanged. A failure with no response — request build,
// authorize, transport (after retries), body read, or a ctx cancellation during
// backoff — is returned as-is and picks up that one helper's wrapping prefix
// (request %s / PUT %s / POST %s), rather than the assorted per-call-site
// wording the inlined versions used (raw errors, "read response", ...); no
// caller inspects those.
//
// idempotent selects the retry policy (see retryableStatus): a GET retries a
// 429 or a 5xx; a mutation (PUT/POST) retries only a 429 — a throttled request
// was rejected, not processed, so replaying it can't double a side effect,
// whereas a 5xx mutation might have partially applied and is surfaced instead.
// The body is buffered as bytes so each attempt gets a fresh reader (an
// http.Request body isn't reusable across attempts). Every wait is ctx-aware,
// so the caller's deadline bounds total blocking regardless of the retry cap.
func (c *Client) doRequest(ctx context.Context, method, url string, body []byte, idempotent bool) (int, []byte, error) {
	for attempt := 1; ; attempt++ {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, reader)
		if err != nil {
			return 0, nil, err
		}
		if err := c.cfg.authorize(req); err != nil {
			return 0, nil, err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			// A transport error (reset, timeout) is retryable for an idempotent
			// request; a mutation might have reached Jira, so it is returned as-is.
			if idempotent && attempt <= maxRateLimitRetries && ctx.Err() == nil {
				if serr := awaitRetry(ctx, attempt, backoffDuration(attempt), "transport_error"); serr != nil {
					return 0, nil, serr
				}
				continue
			}
			return 0, nil, err
		}

		data, rerr := readAllClose(resp)
		if rerr != nil {
			return 0, nil, rerr
		}

		if !retryableStatus(resp.StatusCode, idempotent) || attempt > maxRateLimitRetries {
			return resp.StatusCode, data, nil
		}

		wait := rateLimitWait(resp.Header, attempt)
		if wait > maxRateLimitWait {
			// Retry-After is longer than we're willing to block for: surface the
			// throttled response so the caller turns it into its normal error
			// rather than pinning the goroutine (and, on the agent path, a run
			// slot) waiting it out.
			return resp.StatusCode, data, nil
		}
		if serr := awaitRetry(ctx, attempt, wait, "throttled"); serr != nil {
			return 0, nil, serr
		}
	}
}

// awaitRetry blocks out one backoff between attempts under its own span,
// the Jira twin of internal/github's. doRequest can sleep several times
// per logical call, so reason separates the two causes — a reset
// connection is a different problem from a 429.
func awaitRetry(ctx context.Context, attempt int, wait time.Duration, reason string) error {
	ctx, span := tracer.Start(ctx, "jira.retry.backoff",
		trace.WithAttributes(telemetry.Attempt(attempt), telemetry.Disposition(reason)))
	defer span.End()

	if err := sleepCtx(ctx, wait); err != nil {
		span.SetAttributes(telemetry.Outcome("cancelled"))
		return err
	}
	span.SetAttributes(telemetry.Outcome("waited"))
	return nil
}

// StatusError is a non-2xx Jira response, carrying the status alongside the
// message so callers can branch on *which* failure they got rather than
// pattern-matching a string. Its Error() text is the same line these paths
// have always produced.
//
// The distinction that motivates it: a 404 from the issue endpoint is Jira
// stating that an issue does not exist, which is a far stronger claim than an
// issue's absence from a JQL result set (equally consistent with an unindexed
// or archived issue, a permission change, or a moved key). Only the former is
// safe to act on destructively.
type StatusError struct {
	Method string
	URL    string
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s returned %d: %s", e.Method, e.URL, e.Status, e.Body)
}

// IsNotFound reports whether err is a Jira 404. Jira also answers 404 for an
// issue the credential may not see, which it does deliberately so existence
// isn't disclosed — so "not found" means "this credential cannot resolve this
// key", and a caller treating it as proof of deletion owes that distinction a
// thought.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusNotFound
}

func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	status, body, err := c.doRequest(ctx, "GET", url, nil, true)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", url, err)
	}
	if status != http.StatusOK {
		return nil, &StatusError{Method: "GET", URL: url, Status: status, Body: string(body)}
	}
	return body, nil
}

func (c *Client) put(ctx context.Context, url string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	status, body, err := c.doRequest(ctx, "PUT", url, data, false)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", url, err)
	}
	if status >= 300 {
		return fmt.Errorf("PUT %s returned %d: %s", url, status, string(body))
	}
	return nil
}

// postJSON issues a POST and returns the response body. idempotent selects the
// retry policy: a read-shaped POST (Jira's JQL /search, which mutates nothing)
// passes true so a transient 5xx is retried; a state-changing POST (add
// comment, create issue) passes false so only a 429 — provably not processed —
// is retried, never a 5xx that might have applied.
func (c *Client) postJSON(ctx context.Context, url string, payload any, idempotent bool) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	status, body, err := c.doRequest(ctx, "POST", url, data, idempotent)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	if status >= 300 {
		return nil, fmt.Errorf("POST %s returned %d: %s", url, status, string(body))
	}
	return body, nil
}

func (c *Client) post(ctx context.Context, url string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	status, body, err := c.doRequest(ctx, "POST", url, data, false)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	if status >= 300 {
		return fmt.Errorf("POST %s returned %d: %s", url, status, string(body))
	}
	return nil
}
