package jira

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestSearchAssignableUsers_SaysWhenItCut pins that the search asks for Jira's
// whole window, since Jira drops unassignable users only after it applies
// maxResults, and cuts the answer itself, reporting the cut: 51 assignable
// users come back as 50 and more, 50 as 50 and no more.
func TestSearchAssignableUsers_SaysWhenItCut(t *testing.T) {
	for _, tc := range []struct {
		served int
		more   bool
	}{{MaxUserSearchResults + 1, true}, {MaxUserSearchResults, false}} {
		var (
			mu    sync.Mutex
			query string
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			query = r.URL.RawQuery
			mu.Unlock()
			users := make([]string, tc.served)
			for i := range users {
				users[i] = fmt.Sprintf(`{"key":"JIRAUSER%d","name":"u%d","displayName":"User %d","active":true}`, i, i, i)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "["+strings.Join(users, ",")+"]")
		}))

		users, more, err := NewClient(DataCenterPAT(srv.URL, "tok")).SearchAssignableUsers(context.Background(), "PROJ-1", "user")
		srv.Close()
		if err != nil {
			t.Fatalf("served %d: %v", tc.served, err)
		}
		if len(users) != MaxUserSearchResults || more != tc.more {
			t.Errorf("served %d: got %d users, more=%v; want %d, more=%v", tc.served, len(users), more, MaxUserSearchResults, tc.more)
		}
		mu.Lock()
		if !strings.Contains(query, "maxResults=1000") {
			t.Errorf("query %q does not ask for Jira's whole window", query)
		}
		mu.Unlock()
	}
}

// TestAssignTo_RefusesAUserReadWithoutTheWriteID pins the guard in front of
// the assignee write: an empty id there does not name the user, and a null
// one clears the assignee, so a user Jira answers for without the field the
// deployment's write takes is refused and nothing is written.
func TestAssignTo_RefusesAUserReadWithoutTheWriteID(t *testing.T) {
	cases := map[string]struct {
		cfg  func(url string) Config
		user string
	}{
		// A Data Center user with a key and no username.
		"data center": {func(url string) Config { return DataCenterPAT(url, "tok") }, `{"key":"JIRAUSER1","displayName":"Ana","active":true}`},
		// A Cloud user without an accountId.
		"cloud": {func(url string) Config { return CloudAPIToken(url, "a@b.test", "tok") }, `{"key":"ana","name":"ana","displayName":"Ana","active":true}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var (
				mu     sync.Mutex
				writes int
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					mu.Lock()
					writes++
					mu.Unlock()
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.user)
			}))
			t.Cleanup(srv.Close)

			_, err := NewClient(tc.cfg(srv.URL)).AssignTo(context.Background(), "PROJ-1", "JIRAUSER1")
			if err == nil || !strings.Contains(err.Error(), "without the id its assignee write takes") {
				t.Errorf("err = %v, want the refusal", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if writes != 0 {
				t.Errorf("the refused assign still wrote %d time(s)", writes)
			}
		})
	}
}
