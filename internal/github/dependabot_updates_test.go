package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// updatesRequest is the part of a ListDependabotUpdates request a test reads.
type updatesRequest struct {
	Path  string
	Query string
	Owner string
	Name  string
	After *string
}

// updatesServer answers each read with pages[i] for the i-th request, and
// records the requests.
func updatesServer(t *testing.T, pages ...string) (*[]updatesRequest, *Client) {
	t.Helper()
	var got []updatesRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		req := updatesRequest{Path: r.URL.Path, Query: body.Query}
		req.Owner, _ = body.Variables["owner"].(string)
		req.Name, _ = body.Variables["name"].(string)
		if after, ok := body.Variables["after"].(string); ok {
			req.After = &after
		}
		got = append(got, req)
		if len(got) > len(pages) {
			_, _ = w.Write([]byte(pages[len(pages)-1]))
			return
		}
		_, _ = w.Write([]byte(pages[len(got)-1]))
	}))
	t.Cleanup(srv.Close)
	return &got, NewClient(srv.URL, "tok")
}

func updatesPage(hasNext bool, cursor string, nodes ...string) string {
	c := "null"
	if cursor != "" {
		c = fmt.Sprintf("%q", cursor)
	}
	return fmt.Sprintf(`{"data":{"repository":{"vulnerabilityAlerts":{"pageInfo":{"hasNextPage":%t,"endCursor":%s},"nodes":[%s]}},"rateLimit":{"cost":1}}}`,
		hasNext, c, strings.Join(nodes, ","))
}

// TestClient_ListDependabotUpdates_MapsEachState: the four shapes GitHub gave
// dependabotUpdate on the probe repositories each map to their state, a pull
// request winning over an error beside it, and an alert with no number is
// skipped. The read is a POST to the GraphQL endpoint naming the repository.
func TestClient_ListDependabotUpdates_MapsEachState(t *testing.T) {
	reqs, client := updatesServer(t, updatesPage(false, "c1",
		`{"number":1,"dependabotUpdate":null}`,
		`{"number":2,"dependabotUpdate":{"pullRequest":null,"error":null}}`,
		`{"number":3,"dependabotUpdate":{"pullRequest":{"number":9,"url":"https://github.com/octo/api/pull/9"},"error":null}}`,
		`{"number":4,"dependabotUpdate":{"pullRequest":null,"error":{"errorType":"security_update_not_possible","title":"Dependabot cannot update braces to a non-vulnerable version"}}}`,
		`{"number":5,"dependabotUpdate":{"pullRequest":{"number":10,"url":"https://github.com/octo/api/pull/10"},"error":{"errorType":"x","title":"y"}}}`,
		`{"number":0,"dependabotUpdate":null}`,
		`null`,
	))

	got, err := client.ListDependabotUpdates(context.Background(), "octo", "api")
	if err != nil {
		t.Fatalf("ListDependabotUpdates: %v", err)
	}
	want := map[int]domain.DependabotUpdate{
		1: {State: domain.DependabotUpdateNone},
		2: {State: domain.DependabotUpdatePending},
		3: {State: domain.DependabotUpdatePullRequest, PullRequestNumber: 9, PullRequestURL: "https://github.com/octo/api/pull/9"},
		4: {State: domain.DependabotUpdateError, ErrorType: "security_update_not_possible", ErrorTitle: "Dependabot cannot update braces to a non-vulnerable version"},
		5: {State: domain.DependabotUpdatePullRequest, PullRequestNumber: 10, PullRequestURL: "https://github.com/octo/api/pull/10"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("updates = %+v, want %+v", got, want)
	}
	if len(*reqs) != 1 {
		t.Fatalf("requests = %d, want one page", len(*reqs))
	}
	r := (*reqs)[0]
	if !strings.HasSuffix(r.Path, "/graphql") || !strings.Contains(r.Query, "query DependabotUpdates") || r.Owner != "octo" || r.Name != "api" || r.After != nil {
		t.Errorf("request = %+v, want the DependabotUpdates query for octo/api with no cursor", r)
	}
}

// TestClient_ListDependabotUpdates_FollowsTheCursor: a repository with more
// than one page of open alerts is read page by page with the previous page's
// end cursor.
func TestClient_ListDependabotUpdates_FollowsTheCursor(t *testing.T) {
	reqs, client := updatesServer(t,
		updatesPage(true, "c1", `{"number":1,"dependabotUpdate":null}`),
		updatesPage(false, "c2", `{"number":2,"dependabotUpdate":null}`),
	)
	got, err := client.ListDependabotUpdates(context.Background(), "octo", "api")
	if err != nil || len(got) != 2 {
		t.Fatalf("ListDependabotUpdates = %v, %v; want both pages' alerts", got, err)
	}
	if len(*reqs) != 2 || (*reqs)[1].After == nil || *(*reqs)[1].After != "c1" {
		t.Errorf("requests = %+v, want the second sent after c1", *reqs)
	}
}

// TestClient_ListDependabotUpdates_AnUnusableAnswerFails: GitHub answering
// with no repository (it answers a repository it cannot resolve with null
// beside a NOT_FOUND error) or no alerts connection, a next page with no
// cursor, and pages past the cap each fail the whole read as unusable.
func TestClient_ListDependabotUpdates_AnUnusableAnswerFails(t *testing.T) {
	for name, pages := range map[string][]string{
		"no repository": {`{"data":{"repository":null,"rateLimit":{"cost":1}},"errors":[{"type":"NOT_FOUND","path":["repository"],"message":"Could not resolve to a Repository with the name 'octo/api'."}]}`},
		"no alerts":     {`{"data":{"repository":{"vulnerabilityAlerts":null}},"errors":[{"type":"FORBIDDEN","path":["repository","vulnerabilityAlerts"],"message":"Resource not accessible by integration"}]}`},
		"no cursor":     {updatesPage(true, "", `{"number":1,"dependabotUpdate":null}`)},
		"no last page":  {updatesPage(true, "c", `{"number":1,"dependabotUpdate":null}`)},
	} {
		t.Run(name, func(t *testing.T) {
			reqs, client := updatesServer(t, pages...)
			got, err := client.ListDependabotUpdates(context.Background(), "octo", "api")
			if !errors.Is(err, ErrDependabotListingUnusable) || got != nil {
				t.Errorf("ListDependabotUpdates = %v, %v; want an unusable read and no updates", got, err)
			}
			if name == "no last page" && len(*reqs) != maxDependabotAlertPages {
				t.Errorf("requests = %d, want the cap of %d", len(*reqs), maxDependabotAlertPages)
			}
		})
	}
}

// TestClient_ListDependabotUpdates_AFailureIsNotUnusable: a failure that says
// nothing about the repository is returned as itself, so the caller can tell
// it from an answer about the repository.
func TestClient_ListDependabotUpdates_AFailureIsNotUnusable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	t.Cleanup(srv.Close)
	_, err := NewClient(srv.URL, "tok").ListDependabotUpdates(context.Background(), "octo", "api")
	var he *HTTPError
	if err == nil || errors.Is(err, ErrDependabotListingUnusable) || !errors.As(err, &he) || he.StatusCode != http.StatusUnauthorized {
		t.Errorf("err = %v, want the 401", err)
	}
}

// TestClient_ListDependabotUpdates_AnErroredAlertIsNoAnswer: GitHub can answer
// with data beside an error on one alert's field, which it nulls. That alert
// is left out rather than read as an update Dependabot has not made; the
// others on the page are kept.
func TestClient_ListDependabotUpdates_AnErroredAlertIsNoAnswer(t *testing.T) {
	_, client := updatesServer(t, `{"data":{"repository":{"vulnerabilityAlerts":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[
		{"number":1,"dependabotUpdate":null},
		{"number":2,"dependabotUpdate":{"pullRequest":null,"error":null}},
		{"number":3,"dependabotUpdate":null}
	]}}},"errors":[
		{"type":"FORBIDDEN","path":["repository","vulnerabilityAlerts","nodes",1,"dependabotUpdate","pullRequest"],"message":"Resource not accessible by integration"},
		{"type":"FORBIDDEN","path":["repository","vulnerabilityAlerts","nodes",2,"dependabotUpdate"],"message":"Resource not accessible by integration"}
	]}`)
	got, err := client.ListDependabotUpdates(context.Background(), "octo", "api")
	if err != nil {
		t.Fatalf("ListDependabotUpdates: %v", err)
	}
	if want := map[int]domain.DependabotUpdate{1: {State: domain.DependabotUpdateNone}}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("updates = %+v, want only the alert no error names", got)
	}
}
