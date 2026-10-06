package linear

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// reply is one scripted response.
type reply struct {
	status int
	header map[string]string
	body   string
}

// recordedRequest is what the stub saw of one request.
type recordedRequest struct {
	Auth      string
	UserAgent string
	Path      string
	Query     string
	Variables map[string]json.RawMessage
}

// stub is a GraphQL server answering each request with respond(n, req), n
// counting from 0.
type stub struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []recordedRequest
}

func newStub(t *testing.T, respond func(n int, req recordedRequest) reply) *stub {
	t.Helper()
	s := &stub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Query     string                     `json:"query"`
			Variables map[string]json.RawMessage `json:"variables"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		req := recordedRequest{
			Auth:      r.Header.Get("Authorization"),
			UserAgent: r.Header.Get("User-Agent"),
			Path:      r.URL.Path,
			Query:     body.Query,
			Variables: body.Variables,
		}
		s.mu.Lock()
		n := len(s.got)
		s.got = append(s.got, req)
		s.mu.Unlock()

		rep := respond(n, req)
		for k, v := range rep.header {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		if rep.status == 0 {
			rep.status = http.StatusOK
		}
		w.WriteHeader(rep.status)
		_, _ = w.Write([]byte(rep.body))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// scripted answers the nth request with replies[n], repeating the last.
func scripted(replies ...reply) func(int, recordedRequest) reply {
	return func(n int, _ recordedRequest) reply {
		if n < len(replies) {
			return replies[n]
		}
		return replies[len(replies)-1]
	}
}

func (s *stub) requests() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedRequest(nil), s.got...)
}

func (s *stub) client() *Client {
	cfg := APIKey("lin_api_test")
	cfg.Endpoint = s.srv.URL
	return NewClient(cfg)
}

// shortWaits shrinks the backoff so retry tests do not sleep for real, and
// restores the rate-limit cap a test may lower.
func shortWaits(t *testing.T) {
	t.Helper()
	SetRetryBackoffForTest(t, time.Millisecond)
	prevCap := rateLimitWaitCap
	t.Cleanup(func() { rateLimitWaitCap = prevCap })
}

const viewerOK = `{"data":{"viewer":{"id":"u1","name":"Ada","displayName":"ada","email":"ada@example.com","active":true,"isMe":true}}}`

func rateLimited(reset time.Time) reply {
	return reply{
		status: http.StatusBadRequest,
		header: map[string]string{
			"X-RateLimit-Requests-Remaining": "0",
			"X-RateLimit-Requests-Reset":     strconv.FormatInt(reset.UnixMilli(), 10),
		},
		body: `{"errors":[{"message":"Rate limit exceeded","extensions":{"code":"RATELIMITED"}}]}`,
	}
}

func TestRateLimited_WaitsForResetThenRetries(t *testing.T) {
	shortWaits(t)
	s := newStub(t, scripted(rateLimited(time.Now().Add(150*time.Millisecond)), reply{body: viewerOK}))
	ctx, tally := upstream.WithTally(context.Background())

	start := time.Now()
	u, err := s.client().Viewer(ctx)
	if err != nil {
		t.Fatalf("Viewer: %v", err)
	}
	if u.ID != "u1" {
		t.Errorf("viewer id = %q, want u1", u.ID)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("retried after %v, want the wait until the reset", elapsed)
	}
	if n := len(s.requests()); n != 2 {
		t.Errorf("attempts = %d, want 2", n)
	}
	if tally.Count(upstream.RateLimited) != 1 || tally.Count(upstream.OK) != 1 {
		t.Errorf("tally: %d rate_limited, %d ok; want 1, 1", tally.Count(upstream.RateLimited), tally.Count(upstream.OK))
	}
}

// TestRateLimited_ResetPastTheBudgetReturnsAtOnce: a reset further off than
// the request's wait budget is not slept on. The caller gets the reset time
// straight away, and the request is not sent again.
func TestRateLimited_ResetPastTheBudgetReturnsAtOnce(t *testing.T) {
	shortWaits(t)
	reset := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	s := newStub(t, scripted(rateLimited(reset), reply{body: viewerOK}))

	start := time.Now()
	_, err := s.client().Viewer(context.Background())
	var rle *RateLimitError
	if !errors.As(err, &rle) || !rle.Reset.Equal(reset) {
		t.Fatalf("err = %v, want a *RateLimitError carrying reset %v", err, reset)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("returned after %v, want at once", elapsed)
	}
	if n := len(s.requests()); n != 1 {
		t.Errorf("attempts = %d, want 1", n)
	}
}

// TestRateLimited_BudgetIsPerRequestNotPerWait: two waits that each fit the
// budget but together exceed it stop at the second, so one request cannot
// block for several budgets.
func TestRateLimited_BudgetIsPerRequestNotPerWait(t *testing.T) {
	shortWaits(t)
	rateLimitWaitCap = 150 * time.Millisecond
	s := newStub(t, func(int, recordedRequest) reply {
		return rateLimited(time.Now().Add(100 * time.Millisecond))
	})

	_, err := s.client().Viewer(context.Background())
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if n := len(s.requests()); n != 2 {
		t.Errorf("attempts = %d, want 2: one wait fits the budget, a second does not", n)
	}
}

func TestRateLimited_WaitEndsWithContext(t *testing.T) {
	shortWaits(t)
	s := newStub(t, scripted(rateLimited(time.Now().Add(20*time.Second))))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := s.client().Viewer(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("returned after %v, want as soon as the context ended", elapsed)
	}
	if n := len(s.requests()); n != 1 {
		t.Errorf("attempts = %d, want 1", n)
	}
}

func TestRateLimited_GivesUpAfterThreeAttempts(t *testing.T) {
	shortWaits(t)
	reset := time.Now().Add(5 * time.Millisecond).Truncate(time.Millisecond)
	cases := map[string]func(*Client) error{
		"query":    func(c *Client) error { _, err := c.Viewer(context.Background()); return err },
		"mutation": func(c *Client) error { return c.UnassignIssue(context.Background(), "TFAC-1") },
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStub(t, scripted(rateLimited(reset)))
			err := call(s.client())
			if !errors.Is(err, ErrRateLimited) {
				t.Fatalf("err = %v, want ErrRateLimited", err)
			}
			var rle *RateLimitError
			if !errors.As(err, &rle) || !rle.Reset.Equal(reset) {
				t.Errorf("err = %#v, want a *RateLimitError carrying reset %v", err, reset)
			}
			if class, _ := upstream.ClassOf(err); class != upstream.RateLimited {
				t.Errorf("class = %q, want rate_limited", class)
			}
			// A rate-limited request was refused before Linear acted, so a
			// mutation is retried as a query is.
			if n := len(s.requests()); n != maxAttempts {
				t.Errorf("attempts = %d, want %d", n, maxAttempts)
			}
		})
	}
}

func TestRateLimitReset_LaterOfTheTwoWindows(t *testing.T) {
	early, late := time.UnixMilli(1_900_000_000_000), time.UnixMilli(1_900_000_060_000)
	h := http.Header{}
	h.Set("X-RateLimit-Requests-Reset", strconv.FormatInt(early.UnixMilli(), 10))
	h.Set("X-RateLimit-Complexity-Reset", strconv.FormatInt(late.UnixMilli(), 10))
	if got := rateLimitReset(h); !got.Equal(late) {
		t.Errorf("reset = %v, want the later complexity reset %v", got, late)
	}
	h.Del("X-RateLimit-Complexity-Reset")
	if got := rateLimitReset(h); !got.Equal(early) {
		t.Errorf("reset = %v, want the request reset %v", got, early)
	}
	if got := rateLimitReset(http.Header{}); !got.IsZero() {
		t.Errorf("reset with no headers = %v, want zero", got)
	}
}

func TestServerError_QueryRetried(t *testing.T) {
	shortWaits(t)
	s := newStub(t, scripted(reply{status: http.StatusServiceUnavailable, body: "<html>down</html>"}, reply{body: viewerOK}))
	ctx, tally := upstream.WithTally(context.Background())

	if _, err := s.client().WithOrg("org-1").Viewer(ctx); err != nil {
		t.Fatalf("Viewer: %v", err)
	}
	if n := len(s.requests()); n != 2 {
		t.Errorf("attempts = %d, want 2", n)
	}
	if tally.Count(upstream.Transient) != 1 || tally.Count(upstream.OK) != 1 {
		t.Errorf("tally: %d transient, %d ok; want 1, 1", tally.Count(upstream.Transient), tally.Count(upstream.OK))
	}
}

func TestServerError_QueryGivesUpAfterThreeAttempts(t *testing.T) {
	shortWaits(t)
	s := newStub(t, scripted(reply{status: http.StatusBadGateway, body: "<html>bad gateway</html>"}))

	_, err := s.client().Viewer(context.Background())
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusBadGateway || se.Class != upstream.Transient {
		t.Fatalf("err = %v, want a transient 502 *StatusError", err)
	}
	if strings.Contains(err.Error(), "<html>") {
		t.Errorf("message carries the body: %q", err.Error())
	}
	if n := len(s.requests()); n != maxAttempts {
		t.Errorf("attempts = %d, want %d", n, maxAttempts)
	}
}

func TestServerError_MutationNotRetried(t *testing.T) {
	shortWaits(t)
	s := newStub(t, scripted(reply{status: http.StatusInternalServerError, body: `{}`}))

	err := s.client().TransitionIssue(context.Background(), "TFAC-1", "state-1")
	var se *StatusError
	if !errors.As(err, &se) || se.Class != upstream.Transient {
		t.Fatalf("err = %v, want a transient *StatusError", err)
	}
	if n := len(s.requests()); n != 1 {
		t.Errorf("attempts = %d, want 1: the write may have applied", n)
	}
}

func TestUnauthorized_NotRetried(t *testing.T) {
	shortWaits(t)
	cases := map[string]reply{
		"http 401":             {status: http.StatusUnauthorized, body: `{"message":"bad key"}`},
		"authentication error": {status: http.StatusBadRequest, body: `{"errors":[{"message":"Authentication required, not authenticated","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`},
		"forbidden on 200":     {body: `{"data":null,"errors":[{"message":"Forbidden","extensions":{"code":"FORBIDDEN"}}]}`},
	}
	for name, rep := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStub(t, scripted(rep))
			ctx, tally := upstream.WithTally(context.Background())
			_, err := s.client().Viewer(ctx)
			if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("err = %v, want ErrUnauthorized", err)
			}
			if class, _ := upstream.ClassOf(err); class != upstream.Auth {
				t.Errorf("class = %q, want auth", class)
			}
			if n := len(s.requests()); n != 1 {
				t.Errorf("attempts = %d, want 1", n)
			}
			if tally.Count(upstream.Auth) != 1 {
				t.Errorf("tally: %d auth, want 1", tally.Count(upstream.Auth))
			}
		})
	}
}

func TestDataWithErrors_IsAnError(t *testing.T) {
	s := newStub(t, scripted(reply{body: `{"data":{"viewer":{"id":"u1"}},"errors":[{"message":"field failed","path":["viewer","email"],"extensions":{"code":"INTERNAL_ERROR"}}]}`}))

	u, err := s.client().Viewer(context.Background())
	var gerr *GraphQLError
	if !errors.As(err, &gerr) {
		t.Fatalf("err = %v, want a *GraphQLError", err)
	}
	if gerr.Code != "INTERNAL_ERROR" || !reflect.DeepEqual(gerr.Path, []string{"viewer", "email"}) {
		t.Errorf("GraphQLError = %+v", gerr)
	}
	if gerr.UpstreamClass() != upstream.Rejected {
		t.Errorf("class = %q, want rejected", gerr.UpstreamClass())
	}
	if u.ID != "" {
		t.Errorf("viewer = %+v, want nothing read from a response with errors", u)
	}
}

func TestGetIssue_NotFound(t *testing.T) {
	cases := map[string]reply{
		"entity not found code": {status: http.StatusBadRequest, body: `{"errors":[{"message":"Could not find it","extensions":{"code":"ENTITY_NOT_FOUND"}}]}`},
		"entity not found text": {status: http.StatusBadRequest, body: `{"errors":[{"message":"Entity not found: Issue","extensions":{"code":"INVALID_INPUT"}}]}`},
		"null issue":            {body: `{"data":{"issue":null}}`},
	}
	for name, rep := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStub(t, scripted(rep))
			issue, err := s.client().GetIssue(context.Background(), "TFAC-404")
			if issue != nil {
				t.Errorf("issue = %+v, want nil", issue)
			}
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
			var te *upstream.TransportError
			if errors.As(err, &te) {
				t.Errorf("err = %v, want no transport error", err)
			}
			if n := len(s.requests()); n != 1 {
				t.Errorf("attempts = %d, want 1", n)
			}
		})
	}
}

const issueJSON = `{
  "id": "uuid-1", "identifier": "TFAC-86", "title": "Linear", "description": "body",
  "url": "https://linear.app/sky/issue/TFAC-86", "priority": 2, "priorityLabel": "High",
  "createdAt": "2026-09-18T15:00:00.000Z", "updatedAt": "2026-09-19T15:00:00.000Z",
  "completedAt": null, "canceledAt": null, "archivedAt": null, "trashed": null,
  "state": {"id": "s2", "name": "In Progress", "type": "started", "position": 2},
  "assignee": {"id": "u1", "name": "Ada", "displayName": "ada", "email": "ada@example.com", "active": true},
  "creator": null,
  "parent": {"id": "uuid-0", "identifier": "TFAC-1"},
  "team": {"id": "team-1", "key": "TFAC", "name": "Triage Factory", "private": false},
  "labels": {"nodes": [{"name": "zeta"}, {"name": "alpha"}]},
  "comments": {"nodes": [{"id": "c9", "createdAt": "2026-09-19T14:00:00.000Z"}]},
  "children": {"nodes": [{"id": "uuid-2", "identifier": "TFAC-87", "state": {"id": "s3", "name": "Done", "type": "completed", "position": 3}}]}
}`

func TestGetIssue_DecodesTheFragment(t *testing.T) {
	s := newStub(t, scripted(reply{body: `{"data":{"issue":` + issueJSON + `}}`}))

	issue, err := s.client().GetIssue(context.Background(), "TFAC-86")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	want := &Issue{
		ID: "uuid-1", Identifier: "TFAC-86", Title: "Linear", Description: "body",
		URL: "https://linear.app/sky/issue/TFAC-86", Priority: 2, PriorityLabel: "High",
		State:     WorkflowState{ID: "s2", Name: "In Progress", Type: "started", Position: 2},
		Assignee:  &User{ID: "u1", Name: "Ada", DisplayName: "ada", Email: "ada@example.com", Active: true},
		Parent:    &IssueRef{ID: "uuid-0", Identifier: "TFAC-1"},
		Team:      Team{ID: "team-1", Key: "TFAC", Name: "Triage Factory"},
		Labels:    []string{"alpha", "zeta"},
		CreatedAt: "2026-09-18T15:00:00.000Z", UpdatedAt: "2026-09-19T15:00:00.000Z",
		LastComment: &CommentRef{ID: "c9", CreatedAt: "2026-09-19T14:00:00.000Z"},
		Children: []ChildIssue{{ID: "uuid-2", Identifier: "TFAC-87",
			State: WorkflowState{ID: "s3", Name: "Done", Type: "completed", Position: 3}}},
	}
	if !reflect.DeepEqual(issue, want) {
		t.Errorf("issue =\n%+v\nwant\n%+v", issue, want)
	}
	req := s.requests()[0]
	if string(req.Variables["id"]) != `"TFAC-86"` {
		t.Errorf("id variable = %s, want the identifier", req.Variables["id"])
	}
	if !strings.Contains(req.Query, "fragment IssueFields on Issue") {
		t.Errorf("query does not carry the IssueFields fragment:\n%s", req.Query)
	}
}

func TestListWorkflowStates_FollowsCursorToTheEnd(t *testing.T) {
	s := newStub(t, func(n int, req recordedRequest) reply {
		switch string(req.Variables["after"]) {
		case "null":
			return reply{body: `{"data":{"workflowStates":{"nodes":[{"id":"s3","name":"Done","type":"completed","position":3},{"id":"s1","name":"Todo","type":"unstarted","position":1}],"pageInfo":{"hasNextPage":true,"endCursor":"c1"}}}}`}
		case `"c1"`:
			return reply{body: `{"data":{"workflowStates":{"nodes":[{"id":"s2","name":"Doing","type":"started","position":2}],"pageInfo":{"hasNextPage":false,"endCursor":"c2"}}}}`}
		}
		t.Errorf("request %d with unexpected cursor %s", n, req.Variables["after"])
		return reply{status: http.StatusBadRequest, body: `{}`}
	})

	states, err := s.client().ListWorkflowStates(context.Background(), "team-1")
	if err != nil {
		t.Fatalf("ListWorkflowStates: %v", err)
	}
	var ids []string
	for _, st := range states {
		ids = append(ids, st.ID)
	}
	if !reflect.DeepEqual(ids, []string{"s1", "s2", "s3"}) {
		t.Errorf("states = %v, want both pages ordered by position", ids)
	}
	reqs := s.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if string(reqs[0].Variables["teamID"]) != `"team-1"` || string(reqs[0].Variables["first"]) != "100" {
		t.Errorf("variables = %v", reqs[0].Variables)
	}
}

func TestListTeams_FilterOnlyWithAQuery(t *testing.T) {
	page := reply{body: `{"data":{"teams":{"nodes":[{"id":"t1","key":"TFAC","name":"Triage Factory","private":true}],"pageInfo":{"hasNextPage":true,"endCursor":"c1"}}}}`}
	s := newStub(t, scripted(page))
	c := s.client()

	got, err := c.ListTeams(context.Background(), "", "", 50)
	if err != nil {
		t.Fatalf("ListTeams: %v", err)
	}
	want := TeamPage{Items: []Team{{ID: "t1", Key: "TFAC", Name: "Triage Factory", Private: true}}, EndCursor: "c1", HasNextPage: true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("page = %+v, want %+v", got, want)
	}
	if _, err := c.ListTeams(context.Background(), " tri ", "c1", 10); err != nil {
		t.Fatalf("ListTeams: %v", err)
	}
	reqs := s.requests()
	if f, ok := reqs[0].Variables["filter"]; ok {
		t.Errorf("empty query sent filter %s, want none", f)
	}
	assertJSON(t, "filter", reqs[1].Variables["filter"],
		`{"or":[{"key":{"containsIgnoreCase":"tri"}},{"name":{"containsIgnoreCase":"tri"}}]}`)
	if string(reqs[1].Variables["after"]) != `"c1"` || string(reqs[1].Variables["first"]) != "10" {
		t.Errorf("variables = %v", reqs[1].Variables)
	}

	for _, first := range []int{0, 51} {
		if _, err := c.ListTeams(context.Background(), "", "", first); err == nil {
			t.Errorf("ListTeams(first=%d) succeeded, want a range error", first)
		}
	}
	if n := len(s.requests()); n != 2 {
		t.Errorf("requests = %d, want the invalid pages refused before sending", n)
	}
}

func TestSearchIssues_RendersTheFilter(t *testing.T) {
	cases := []struct {
		name   string
		filter IssueFilter
		want   string
	}{
		{"empty", IssueFilter{}, `{}`},
		{"team", IssueFilter{TeamID: "t1"}, `{"team":{"id":{"eq":"t1"}}}`},
		{"states in", IssueFilter{StateIDsIn: []string{"s1", "s2"}}, `{"state":{"id":{"in":["s1","s2"]}}}`},
		{"states not in", IssueFilter{StateIDsNotIn: []string{"s3"}}, `{"state":{"id":{"nin":["s3"]}}}`},
		{"states in and not in", IssueFilter{StateIDsIn: []string{"s1"}, StateIDsNotIn: []string{"s3"}}, `{"state":{"id":{"in":["s1"],"nin":["s3"]}}}`},
		{"unassigned", IssueFilter{Unassigned: true}, `{"assignee":{"null":true}}`},
		{"assignee", IssueFilter{AssigneeID: "u1"}, `{"assignee":{"id":{"eq":"u1"}}}`},
		{
			"everything",
			IssueFilter{TeamID: "t1", StateIDsIn: []string{"s1"}, StateIDsNotIn: []string{"s3"}, AssigneeID: "u1"},
			`{"team":{"id":{"eq":"t1"}},"state":{"id":{"in":["s1"],"nin":["s3"]}},"assignee":{"id":{"eq":"u1"}}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t, scripted(reply{body: `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`}))
			if _, err := s.client().SearchIssues(context.Background(), tc.filter, ""); err != nil {
				t.Fatalf("SearchIssues: %v", err)
			}
			req := s.requests()[0]
			assertJSON(t, "filter", req.Variables["filter"], tc.want)
			if string(req.Variables["first"]) != "50" || string(req.Variables["after"]) != "null" {
				t.Errorf("variables = %v", req.Variables)
			}
		})
	}
}

func TestSearchIssues_RefusedFilters(t *testing.T) {
	s := newStub(t, scripted(reply{body: `{}`}))
	c := s.client()

	if _, err := c.SearchIssues(context.Background(), IssueFilter{Unassigned: true, AssigneeID: "u1"}, ""); err == nil {
		t.Error("Unassigned with AssigneeID succeeded, want an error")
	}
	// An empty set of states admits no issue; it must not be dropped and
	// widen the search to every state.
	page, err := c.SearchIssues(context.Background(), IssueFilter{TeamID: "t1", StateIDsIn: []string{}}, "")
	if err != nil || len(page.Items) != 0 || page.HasNextPage {
		t.Errorf("empty StateIDsIn = (%+v, %v), want an empty page", page, err)
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("requests = %d, want none sent", n)
	}
}

func TestUpdateIssue_RendersTheInput(t *testing.T) {
	s := newStub(t, scripted(reply{body: `{"data":{"issueUpdate":{"success":true,"issue":` + issueJSON + `}}}`}))
	c := s.client()
	title, parent, priority := "New title", "", 1

	err := c.UpdateIssue(context.Background(), "TFAC-86", UpdateIssueFields{
		Title: &title, ParentID: &parent, Priority: &priority,
		AddLabelIDs: []string{"l1"}, RemoveLabelIDs: []string{"l2"},
	})
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	if err := c.UnassignIssue(context.Background(), "TFAC-86"); err != nil {
		t.Fatalf("UnassignIssue: %v", err)
	}
	reqs := s.requests()
	assertJSON(t, "input", reqs[0].Variables["input"],
		`{"title":"New title","parentId":null,"priority":1,"addedLabelIds":["l1"],"removedLabelIds":["l2"]}`)
	assertJSON(t, "input", reqs[1].Variables["input"], `{"assigneeId":null}`)
	if string(reqs[0].Variables["id"]) != `"TFAC-86"` {
		t.Errorf("id = %s", reqs[0].Variables["id"])
	}
}

func TestMutations_SuccessFalseIsAnError(t *testing.T) {
	s := newStub(t, scripted(
		reply{body: `{"data":{"issueUpdate":{"success":false,"issue":null}}}`},
		reply{body: `{"data":{"issueCreate":{"success":false,"issue":null}}}`},
	))
	c := s.client()

	if err := c.TransitionIssue(context.Background(), "TFAC-1", "s1"); err == nil || !strings.Contains(err.Error(), "issueUpdate") {
		t.Errorf("TransitionIssue err = %v, want one naming issueUpdate", err)
	}
	if _, err := c.CreateIssue(context.Background(), CreateIssueInput{TeamID: "t1", Title: "x"}); err == nil || !strings.Contains(err.Error(), "issueCreate") {
		t.Errorf("CreateIssue err = %v, want one naming issueCreate", err)
	}
}

func TestClientSideValidation_SendsNothing(t *testing.T) {
	s := newStub(t, scripted(reply{body: `{}`}))
	c := s.client()
	ctx := context.Background()
	bad := 5

	for name, err := range map[string]error{
		"priority above 4":       c.SetPriority(ctx, "TFAC-1", 5),
		"priority below 0":       c.SetPriority(ctx, "TFAC-1", -1),
		"update priority":        c.UpdateIssue(ctx, "TFAC-1", UpdateIssueFields{Priority: &bad}),
		"empty update":           c.UpdateIssue(ctx, "TFAC-1", UpdateIssueFields{}),
		"create without team":    func() error { _, err := c.CreateIssue(ctx, CreateIssueInput{Title: "x"}); return err }(),
		"create without title":   func() error { _, err := c.CreateIssue(ctx, CreateIssueInput{TeamID: "t1"}); return err }(),
		"assign to nobody":       c.AssignIssue(ctx, "TFAC-1", ""),
		"empty comment":          func() error { _, err := c.AddComment(ctx, "TFAC-1", "  "); return err }(),
		"too many issue ids":     func() error { _, err := c.GetIssues(ctx, make([]string, 51)); return err }(),
		"transition to no state": c.TransitionIssue(ctx, "TFAC-1", ""),
	} {
		if err == nil {
			t.Errorf("%s: succeeded, want an error", name)
		}
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("requests = %d, want none sent", n)
	}
}

func TestAddComment_ReturnsTheCommentID(t *testing.T) {
	s := newStub(t, scripted(reply{body: `{"data":{"commentCreate":{"success":true,"comment":{"id":"c1"}}}}`}))

	id, err := s.client().AddComment(context.Background(), "TFAC-1", "hello")
	if err != nil || id != "c1" {
		t.Fatalf("AddComment = (%q, %v), want c1", id, err)
	}
	req := s.requests()[0]
	assertJSON(t, "input", req.Variables["input"], `{"issueId":"TFAC-1","body":"hello"}`)
	if strings.Contains(req.Query, "createAsUser") {
		t.Error("commentCreate sets createAsUser, which TF never uses")
	}
}

func TestConfigs_AuthorizationAndEndpoint(t *testing.T) {
	s := newStub(t, scripted(reply{body: viewerOK}))
	proxy := ProxyPlaceholder(s.srv.URL+"/", "placeholder-1")
	cases := []struct {
		name     string
		cfg      Config
		wantAuth string
		wantPath string
	}{
		{"api key", APIKey("lin_api_abc"), "lin_api_abc", "/"},
		{"bearer", Bearer("oauth-token"), "Bearer oauth-token", "/"},
		{"proxy placeholder", proxy, "Bearer placeholder-1", "/graphql"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(s.requests())
			cfg := tc.cfg
			if cfg.Endpoint == DefaultEndpoint {
				cfg.Endpoint = s.srv.URL + "/"
			}
			if _, err := NewClient(cfg).Viewer(context.Background()); err != nil {
				t.Fatalf("Viewer: %v", err)
			}
			req := s.requests()[before]
			if req.Auth != tc.wantAuth {
				t.Errorf("Authorization = %q, want %q", req.Auth, tc.wantAuth)
			}
			if req.Path != tc.wantPath {
				t.Errorf("path = %q, want %q", req.Path, tc.wantPath)
			}
			if !strings.HasPrefix(req.UserAgent, "triagefactory/") {
				t.Errorf("User-Agent = %q, want triagefactory/<version>", req.UserAgent)
			}
		})
	}
	if APIKey("k").Endpoint != DefaultEndpoint || Bearer("t").Endpoint != DefaultEndpoint {
		t.Error("APIKey and Bearer must target Linear's endpoint")
	}
	if _, err := NewClient(Config{Endpoint: s.srv.URL}).Viewer(context.Background()); err == nil {
		t.Error("a Config built without a constructor sent a request with no credential")
	}
}

// assertJSON compares a JSON value to want, ignoring key order and spacing.
func assertJSON(t *testing.T, name string, got json.RawMessage, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: %s is not JSON: %v", name, got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("%s: want %s is not JSON: %v", name, want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("%s = %s, want %s", name, got, want)
	}
}

func TestSetVersion_NamesTheReleaseInTheUserAgent(t *testing.T) {
	prev := userAgent
	t.Cleanup(func() { userAgent = prev })
	SetVersion("v1.16.0")
	s := newStub(t, scripted(reply{body: viewerOK}))

	if _, err := s.client().Viewer(context.Background()); err != nil {
		t.Fatalf("Viewer: %v", err)
	}
	if got := s.requests()[0].UserAgent; got != "triagefactory/v1.16.0" {
		t.Errorf("User-Agent = %q, want triagefactory/v1.16.0", got)
	}
}

// truncated answers with a 200 whose body breaks off before its declared
// length, so the client's read of it fails after Linear answered.
func truncated(w http.ResponseWriter) {
	w.Header().Set("Content-Length", "1000")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"data":{"vie`))
	w.(http.Flusher).Flush()
	panic(http.ErrAbortHandler)
}

func TestTruncatedBody_RetriedLikeATransportFailure(t *testing.T) {
	shortWaits(t)
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			truncated(w)
		}
		_, _ = w.Write([]byte(viewerOK))
	}))
	t.Cleanup(srv.Close)
	cfg := APIKey("k")
	cfg.Endpoint = srv.URL
	ctx, tally := upstream.WithTally(context.Background())

	u, err := NewClient(cfg).Viewer(ctx)
	if err != nil {
		t.Fatalf("Viewer: %v", err)
	}
	if u.ID != "u1" || calls != 2 {
		t.Errorf("viewer = %q after %d attempts, want u1 after 2", u.ID, calls)
	}
	if tally.Count(upstream.Transient) != 1 || tally.Count(upstream.OK) != 1 {
		t.Errorf("tally: %d transient, %d ok; want 1, 1", tally.Count(upstream.Transient), tally.Count(upstream.OK))
	}
}

func TestTruncatedBody_MutationNotRetried(t *testing.T) {
	shortWaits(t)
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		truncated(w)
	}))
	t.Cleanup(srv.Close)
	cfg := APIKey("k")
	cfg.Endpoint = srv.URL

	err := NewClient(cfg).TransitionIssue(context.Background(), "ENG-1", "s1")
	var te *upstream.TransportError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v, want a *upstream.TransportError", err)
	}
	if calls != 1 {
		t.Errorf("attempts = %d, want 1: the write may have applied", calls)
	}
}

func TestHTTP429_IsARateLimitError(t *testing.T) {
	shortWaits(t)
	reset := time.Now().Add(5 * time.Millisecond).Truncate(time.Millisecond)
	s := newStub(t, scripted(reply{
		status: http.StatusTooManyRequests,
		header: map[string]string{"X-RateLimit-Requests-Reset": strconv.FormatInt(reset.UnixMilli(), 10)},
		body:   "<html>slow down</html>",
	}))

	_, err := s.client().Viewer(context.Background())
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	var rle *RateLimitError
	if !errors.As(err, &rle) || !rle.Reset.Equal(reset) {
		t.Fatalf("err = %#v, want a *RateLimitError carrying reset %v", err, reset)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusTooManyRequests {
		t.Errorf("err = %v, want it to wrap the 429 *StatusError", err)
	}
	if n := len(s.requests()); n != maxAttempts {
		t.Errorf("attempts = %d, want %d", n, maxAttempts)
	}
}

func TestRateLimitReset_FallsBackToRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "30")
	got := rateLimitReset(h)
	if wait := time.Until(got); wait < 25*time.Second || wait > 30*time.Second {
		t.Errorf("reset is %v away, want about 30s from Retry-After", wait)
	}
	h.Set("X-RateLimit-Requests-Reset", "1900000000000")
	if got := rateLimitReset(h); !got.Equal(time.UnixMilli(1_900_000_000_000)) {
		t.Errorf("reset = %v, want Linear's own header over Retry-After", got)
	}
}

// TestGetIssue_CompletesTruncatedConnections: an issue whose labels or
// sub-issues run past the fragment's first page has each list fetched whole,
// by the issue's UUID, rather than handed back cut short.
func TestGetIssue_CompletesTruncatedConnections(t *testing.T) {
	const truncatedIssue = `{"data":{"issue":{"id":"uuid-1","identifier":"ENG-1","title":"Epic",
		"state":{"id":"s1","name":"Todo","type":"unstarted","position":1},
		"team":{"id":"t1","key":"ENG","name":"Eng"},
		"labels":{"nodes":[{"name":"b"}],"pageInfo":{"hasNextPage":true}},
		"comments":{"nodes":[]},
		"children":{"nodes":[{"id":"c1","identifier":"ENG-2","state":{"id":"s1"}}],"pageInfo":{"hasNextPage":true}}}}}`
	s := newStub(t, func(n int, req recordedRequest) reply {
		after := string(req.Variables["after"])
		switch {
		case strings.Contains(req.Query, "query Issue("):
			return reply{body: truncatedIssue}
		case strings.Contains(req.Query, "query IssueLabels"):
			return reply{body: `{"data":{"issue":{"labels":{"nodes":[{"name":"c"},{"name":"a"},{"name":"b"}],"pageInfo":{"hasNextPage":false,"endCursor":"l1"}}}}}`}
		case strings.Contains(req.Query, "query IssueChildren") && after == "null":
			return reply{body: `{"data":{"issue":{"children":{"nodes":[{"id":"c1","identifier":"ENG-2","state":{"id":"s1"}}],"pageInfo":{"hasNextPage":true,"endCursor":"k1"}}}}}`}
		case strings.Contains(req.Query, "query IssueChildren") && after == `"k1"`:
			return reply{body: `{"data":{"issue":{"children":{"nodes":[{"id":"c2","identifier":"ENG-3","state":{"id":"s2"}}],"pageInfo":{"hasNextPage":false,"endCursor":"k2"}}}}}`}
		}
		t.Errorf("unexpected request %d: %s", n, req.Query)
		return reply{status: http.StatusBadRequest, body: `{}`}
	})

	issue, err := s.client().GetIssue(context.Background(), "ENG-1")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if !reflect.DeepEqual(issue.Labels, []string{"a", "b", "c"}) {
		t.Errorf("labels = %v, want every label, sorted", issue.Labels)
	}
	var children []string
	for _, c := range issue.Children {
		children = append(children, c.Identifier)
	}
	if !reflect.DeepEqual(children, []string{"ENG-2", "ENG-3"}) {
		t.Errorf("children = %v, want both pages", children)
	}
	for _, req := range s.requests()[1:] {
		if string(req.Variables["id"]) != `"uuid-1"` || string(req.Variables["first"]) != "100" {
			t.Errorf("completion variables = %v, want the UUID and a page of 100", req.Variables)
		}
	}
	if n := len(s.requests()); n != 4 {
		t.Errorf("requests = %d, want the issue, one label page and two sub-issue pages", n)
	}
}

func TestSearchIssues_CompletesOnlyTruncatedIssues(t *testing.T) {
	s := newStub(t, func(_ int, req recordedRequest) reply {
		if strings.Contains(req.Query, "query IssueLabels") {
			return reply{body: `{"data":{"issue":{"labels":{"nodes":[{"name":"x"},{"name":"y"}],"pageInfo":{"hasNextPage":false}}}}}`}
		}
		return reply{body: `{"data":{"issues":{"nodes":[
			{"id":"u1","identifier":"ENG-1","labels":{"nodes":[{"name":"x"}],"pageInfo":{"hasNextPage":true}}},
			{"id":"u2","identifier":"ENG-2","labels":{"nodes":[{"name":"z"}],"pageInfo":{"hasNextPage":false}}}
		],"pageInfo":{"hasNextPage":false}}}}`}
	})

	page, err := s.client().SearchIssues(context.Background(), IssueFilter{TeamID: "t1"}, "")
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if !reflect.DeepEqual(page.Items[0].Labels, []string{"x", "y"}) || !reflect.DeepEqual(page.Items[1].Labels, []string{"z"}) {
		t.Errorf("labels = %v / %v", page.Items[0].Labels, page.Items[1].Labels)
	}
	if n := len(s.requests()); n != 2 {
		t.Errorf("requests = %d, want the search plus one completion", n)
	}
}

func TestListChildren_WalksOnlyTheSubIssues(t *testing.T) {
	s := newStub(t, func(_ int, req recordedRequest) reply {
		if !strings.Contains(req.Query, "query IssueChildren") {
			t.Errorf("ListChildren sent %q, want only IssueChildren", req.Query)
		}
		return reply{body: `{"data":{"issue":{"children":{"nodes":[{"id":"c1","identifier":"ENG-2","state":{"id":"s1","name":"Todo","type":"unstarted","position":1}}],"pageInfo":{"hasNextPage":false}}}}}`}
	})

	children, err := s.client().ListChildren(context.Background(), "ENG-1")
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	want := []ChildIssue{{ID: "c1", Identifier: "ENG-2", State: WorkflowState{ID: "s1", Name: "Todo", Type: "unstarted", Position: 1}}}
	if !reflect.DeepEqual(children, want) {
		t.Errorf("children = %+v, want %+v", children, want)
	}
	if string(s.requests()[0].Variables["id"]) != `"ENG-1"` {
		t.Errorf("id = %s, want the identifier as given", s.requests()[0].Variables["id"])
	}
}

func TestListChildren_NotFound(t *testing.T) {
	s := newStub(t, scripted(reply{body: `{"data":{"issue":null}}`}))
	if _, err := s.client().ListChildren(context.Background(), "ENG-404"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
