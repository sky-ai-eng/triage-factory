package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
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
)

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
	case AuthMethodCloudAPIToken:
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
// Data Center PATs today.
//
// Extension point — Cloud OAuth (SKY-347, out of scope here): a Cloud OAuth
// 3LO access token is also a bearer token, so bearerAuth is reused as-is for
// that flow. Only token acquisition/refresh and the gateway BaseURL (see
// Config.BaseURL) get added there; the scheme itself does not change.
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
	// BaseURL is the site/host URL as given; it is treated as opaque.
	//
	// Extension point — Cloud OAuth (SKY-347, out of scope here): for the
	// OAuth flow this will hold the api.atlassian.com/ex/jira/{cloud_id}
	// gateway URL rather than the raw site URL. No request-building code
	// here changes for that — the path stays {BaseURL}/rest/api/{version}/...
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

// CloudAPIToken builds a Config for an Atlassian Cloud API token: Basic auth
// (base64(email:token)), REST v3. No caller constructs this yet — Cloud
// onboarding (capturing the email + Cloud marker + storage) is a separate
// SKY-347 sub-ticket; this constructor just makes Cloud expressible in code.
func CloudAPIToken(baseURL, email, token string) Config {
	return Config{
		BaseURL:    baseURL,
		Deployment: DeploymentCloud,
		APIVersion: APIv3,
		auth:       basicAuth{email: email, token: token},
	}
}

// restURL joins an already-formatted literal path onto the versioned REST
// base: {BaseURL}/rest/api/{APIVersion}/{path}. A trailing slash on BaseURL
// (if any) is trimmed so the result never contains "//rest".
func (cfg Config) restURL(path string) string {
	return fmt.Sprintf("%s/rest/api/%s/%s", strings.TrimRight(cfg.BaseURL, "/"), cfg.APIVersion, path)
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
func NewClient(cfg Config) *Client {
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: 15 * time.Second},
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

// TransitionTo transitions an issue to the target status name.
// It finds the appropriate transition by matching the target status name.
func (c *Client) TransitionTo(ctx context.Context, issueKey, targetStatusName string) error {
	transitions, err := c.getTransitions(ctx, issueKey)
	if err != nil {
		return err
	}

	for _, t := range transitions {
		if strings.EqualFold(t.To.Name, targetStatusName) {
			return c.doTransition(ctx, issueKey, t.ID)
		}
	}

	available := make([]string, len(transitions))
	for i, t := range transitions {
		available[i] = t.To.Name
	}
	return fmt.Errorf("no transition to %q found (available: %s)", targetStatusName, strings.Join(available, ", "))
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
			log.Printf("[jira] claim guard: failed to fetch %s: %v", issueKey, err)
		}
		return nil
	}
	var issue Issue
	if err := json.Unmarshal(body, &issue); err != nil {
		log.Printf("[jira] claim guard: failed to parse %s: %v", issueKey, err)
		return nil
	}

	myself, err := c.currentUser(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("[jira] claim guard: failed to get current user: %v", err)
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
		if myself.AccountID != "" {
			state.AssignedToSelf = issue.Fields.Assignee.AccountID == myself.AccountID
		} else {
			state.AssignedToSelf = issue.Fields.Assignee.Name == myself.Name
		}
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
		log.Printf("[jira] warning: epic link field discovery failed: %v", err)
	} else if epicField != "" {
		epicIssues, err := c.SearchIssues(ctx, fmt.Sprintf("cf[%s] = %s ORDER BY created ASC", extractFieldID(epicField), parentKey), nil, 100)
		if err != nil {
			log.Printf("[jira] warning: epic link query failed for %s: %v", parentKey, err)
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

// SearchIssues runs a JQL query and returns matching issues.
// If fields is nil, DefaultSearchFields is used. Pass []string{"*all"} for everything.
func (c *Client) SearchIssues(ctx context.Context, jql string, fields []string, maxResults int) ([]Issue, error) {
	if fields == nil {
		fields = DefaultSearchFields
	}
	if maxResults <= 0 {
		maxResults = 100
	}

	url := c.apiURL("search")
	payload := map[string]any{
		"jql":        jql,
		"maxResults": maxResults,
		"fields":     fields,
	}
	respBody, err := c.postJSON(ctx, url, payload)
	if err != nil {
		return nil, err
	}

	var result struct {
		Issues []Issue `json:"issues"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse search results: %w", err)
	}
	return result.Issues, nil
}

// extractFieldID pulls the numeric ID from a custom field name like "customfield_10008".
func extractFieldID(field string) string {
	return strings.TrimPrefix(field, "customfield_")
}

// AddComment posts a comment on an issue.
func (c *Client) AddComment(ctx context.Context, issueKey, body string) error {
	url := c.apiURL("issue/%s/comment", issueKey)
	return c.post(ctx, url, map[string]string{"body": body})
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
		fields["description"] = description
	}
	if priority != "" {
		fields["priority"] = map[string]string{"name": priority}
	}

	if parentKey != "" {
		fields["parent"] = map[string]string{"key": parentKey}
	}

	payload := map[string]any{"fields": fields}
	createURL := c.apiURL("issue")
	respBody, err := c.postJSON(ctx, createURL, payload)

	// If parent field failed on Server/DC, retry with Epic Link
	if err != nil && parentKey != "" {
		if strings.Contains(err.Error(), "gh.epic.error") || strings.Contains(err.Error(), "parent") {
			delete(fields, "parent")
			epicField, epicErr := c.epicLinkField(ctx)
			if epicErr == nil && epicField != "" {
				fields[epicField] = parentKey
				payload = map[string]any{"fields": fields}
				respBody, err = c.postJSON(ctx, createURL, payload)
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
		fields["description"] = *f.Description
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
	Name      string `json:"name"`      // Jira Server/DC
	AccountID string `json:"accountId"` // Jira Cloud
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

func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	if err := c.cfg.authorize(req); err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d: %s", url, resp.StatusCode, string(body))
	}
	return body, nil
}

func (c *Client) put(ctx context.Context, url string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	if err := c.cfg.authorize(req); err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PUT %s returned %d: %s", url, resp.StatusCode, string(body))
	}
	return nil
}

func (c *Client) postJSON(ctx context.Context, url string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if err := c.cfg.authorize(req); err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response from POST %s: %w", url, err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("POST %s returned %d: %s", url, resp.StatusCode, string(body))
	}
	return body, nil
}

func (c *Client) post(ctx context.Context, url string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	if err := c.cfg.authorize(req); err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s returned %d: %s", url, resp.StatusCode, string(body))
	}
	return nil
}
