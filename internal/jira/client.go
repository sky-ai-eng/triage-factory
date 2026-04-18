package jira

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Client wraps the Jira REST API v2.
type Client struct {
	baseURL    string
	pat        string
	http       *http.Client
	cachedSelf *currentUserResponse // lazily populated by currentUser()
}

func NewClient(baseURL, pat string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		pat:     pat,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Status represents a Jira workflow status.
type Status struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ProjectStatuses returns all unique statuses available in a project,
// deduplicated across issue types.
func (c *Client) ProjectStatuses(projectKey string) ([]Status, error) {
	url := fmt.Sprintf("%s/rest/api/2/project/%s/statuses", c.baseURL, projectKey)
	body, err := c.get(url)
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
func (c *Client) AssignToSelf(issueKey string) error {
	url := fmt.Sprintf("%s/rest/api/2/issue/%s/assignee", c.baseURL, issueKey)
	// Setting name to "-1" assigns to the current user in Jira Server/DC.
	// For Jira Cloud, we need accountId. We'll try the myself endpoint first.
	myself, err := c.currentUser()
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

	return c.put(url, payload)
}

// Unassign removes the assignee from an issue.
func (c *Client) Unassign(issueKey string) error {
	url := fmt.Sprintf("%s/rest/api/2/issue/%s/assignee", c.baseURL, issueKey)
	// Detect Cloud vs Server the same way AssignToSelf does.
	myself, err := c.currentUser()
	if err != nil {
		return fmt.Errorf("get current user: %w", err)
	}
	if myself.AccountID != "" {
		// Jira Cloud: null accountId clears assignee
		return c.put(url, map[string]*string{"accountId": nil})
	}
	// Jira Server/DC: null name clears assignee
	return c.put(url, map[string]*string{"name": nil})
}

// TransitionTo transitions an issue to the target status name.
// It finds the appropriate transition by matching the target status name.
func (c *Client) TransitionTo(issueKey, targetStatusName string) error {
	transitions, err := c.getTransitions(issueKey)
	if err != nil {
		return err
	}

	for _, t := range transitions {
		if strings.EqualFold(t.To.Name, targetStatusName) {
			return c.doTransition(issueKey, t.ID)
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
	StatusName     string // current workflow status
}

// GetClaimState fetches the current assignee and status of an issue and
// checks whether the assignee is the authenticated user. Returns nil on
// any error — callers treat failure as "unknown, proceed normally".
func (c *Client) GetClaimState(issueKey string) *ClaimState {
	// Fetch only assignee + status to minimize payload. The ?fields param
	// works identically on Cloud and Server/DC (v2 REST API).
	url := fmt.Sprintf("%s/rest/api/2/issue/%s?fields=assignee,status", c.baseURL, issueKey)
	body, err := c.get(url)
	if err != nil {
		log.Printf("[jira] claim guard: failed to fetch %s: %v", issueKey, err)
		return nil
	}
	var issue Issue
	if err := json.Unmarshal(body, &issue); err != nil {
		log.Printf("[jira] claim guard: failed to parse %s: %v", issueKey, err)
		return nil
	}

	myself, err := c.currentUser()
	if err != nil {
		log.Printf("[jira] claim guard: failed to get current user: %v", err)
		return nil
	}

	state := &ClaimState{}
	if issue.Fields.Status != nil {
		state.StatusName = issue.Fields.Status.Name
	}
	if issue.Fields.Assignee != nil {
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
func (c *Client) ListPriorities() ([]Priority, error) {
	body, err := c.get(fmt.Sprintf("%s/rest/api/2/priority", c.baseURL))
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
func (c *Client) GetIssue(issueKey string) (*Issue, error) {
	url := fmt.Sprintf("%s/rest/api/2/issue/%s", c.baseURL, issueKey)
	body, err := c.get(url)
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
func (c *Client) GetChildIssues(parentKey string) ([]Issue, error) {
	seen := map[string]bool{}
	var result []Issue

	// Query 1: direct parent relationship (Cloud + Server/DC subtasks)
	issues, err := c.SearchIssues(fmt.Sprintf("parent = %s ORDER BY created ASC", parentKey), nil, 100)
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
	epicField, err := c.epicLinkField()
	if err != nil {
		log.Printf("[jira] warning: epic link field discovery failed: %v", err)
	} else if epicField != "" {
		epicIssues, err := c.SearchIssues(fmt.Sprintf("cf[%s] = %s ORDER BY created ASC", extractFieldID(epicField), parentKey), nil, 100)
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
var DefaultSearchFields = []string{"summary", "description", "status", "issuetype", "priority", "assignee", "parent", "labels"}

// SearchIssues runs a JQL query and returns matching issues.
// If fields is nil, DefaultSearchFields is used. Pass []string{"*all"} for everything.
func (c *Client) SearchIssues(jql string, fields []string, maxResults int) ([]Issue, error) {
	if fields == nil {
		fields = DefaultSearchFields
	}
	if maxResults <= 0 {
		maxResults = 100
	}

	url := fmt.Sprintf("%s/rest/api/2/search", c.baseURL)
	payload := map[string]any{
		"jql":        jql,
		"maxResults": maxResults,
		"fields":     fields,
	}
	respBody, err := c.postJSON(url, payload)
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
func (c *Client) AddComment(issueKey, body string) error {
	url := fmt.Sprintf("%s/rest/api/2/issue/%s/comment", c.baseURL, issueKey)
	return c.post(url, map[string]string{"body": body})
}

// GetTransitions returns the available workflow transitions for an issue.
func (c *Client) GetTransitions(issueKey string) ([]Transition, error) {
	return c.getTransitions(issueKey)
}

// ListIssueTypes returns the issue types available in a project.
func (c *Client) ListIssueTypes(projectKey string) ([]IssueType, error) {
	url := fmt.Sprintf("%s/rest/api/2/project/%s", c.baseURL, projectKey)
	body, err := c.get(url)
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
func (c *Client) CreateIssue(projectKey, issueType, summary, description, parentKey, priority string) (string, error) {
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
	createURL := fmt.Sprintf("%s/rest/api/2/issue", c.baseURL)
	respBody, err := c.postJSON(createURL, payload)

	// If parent field failed on Server/DC, retry with Epic Link
	if err != nil && parentKey != "" {
		if strings.Contains(err.Error(), "gh.epic.error") || strings.Contains(err.Error(), "parent") {
			delete(fields, "parent")
			epicField, epicErr := c.epicLinkField()
			if epicErr == nil && epicField != "" {
				fields[epicField] = parentKey
				payload = map[string]any{"fields": fields}
				respBody, err = c.postJSON(createURL, payload)
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
func (c *Client) SetPriority(issueKey, priority string) error {
	url := fmt.Sprintf("%s/rest/api/2/issue/%s", c.baseURL, issueKey)
	return c.put(url, map[string]any{"fields": map[string]any{
		"priority": map[string]string{"name": priority},
	}})
}

// SetParent links an existing issue under a parent.
// Tries fields.parent first (works for Cloud + Server/DC subtasks).
// Falls back to Epic Link custom field on Server/DC if parent is an Epic.
func (c *Client) SetParent(issueKey, parentKey string) error {
	url := fmt.Sprintf("%s/rest/api/2/issue/%s", c.baseURL, issueKey)

	// Try native parent field first
	err := c.put(url, map[string]any{"fields": map[string]any{
		"parent": map[string]string{"key": parentKey},
	}})
	if err == nil {
		return nil
	}

	// Fall back to Epic Link if the parent field failed
	if strings.Contains(err.Error(), "gh.epic.error") || strings.Contains(err.Error(), "parent") {
		epicField, epicErr := c.epicLinkField()
		if epicErr == nil && epicField != "" {
			return c.put(url, map[string]any{"fields": map[string]any{
				epicField: parentKey,
			}})
		}
	}

	return err
}

// epicLinkField discovers the custom field ID for Epic Link on Server/DC.
// It looks for the field with schema type "com.pyxis.greenhopper.jira:gh-epic-link".
// Returns empty string (not an error) if not found.
func (c *Client) epicLinkField() (string, error) {
	body, err := c.get(fmt.Sprintf("%s/rest/api/2/field", c.baseURL))
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

func (c *Client) currentUser() (*currentUserResponse, error) {
	if c.cachedSelf != nil {
		return c.cachedSelf, nil
	}
	body, err := c.get(fmt.Sprintf("%s/rest/api/2/myself", c.baseURL))
	if err != nil {
		return nil, err
	}
	var user currentUserResponse
	if err := json.Unmarshal(body, &user); err != nil {
		return nil, fmt.Errorf("parse myself: %w", err)
	}
	c.cachedSelf = &user
	return &user, nil
}

func (c *Client) getTransitions(issueKey string) ([]Transition, error) {
	url := fmt.Sprintf("%s/rest/api/2/issue/%s/transitions", c.baseURL, issueKey)
	body, err := c.get(url)
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

func (c *Client) doTransition(issueKey, transitionID string) error {
	url := fmt.Sprintf("%s/rest/api/2/issue/%s/transitions", c.baseURL, issueKey)
	payload := map[string]any{
		"transition": map[string]string{"id": transitionID},
	}
	return c.post(url, payload)
}

func (c *Client) get(url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
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

func (c *Client) put(url string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("PUT", url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
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

func (c *Client) postJSON(url string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
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

func (c *Client) post(url string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
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
