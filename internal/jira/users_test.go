package jira

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

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
