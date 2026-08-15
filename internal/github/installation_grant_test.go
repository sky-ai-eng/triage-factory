package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Completeness is not a detail for the grant mirror. A truncated answer written
// as if it were the whole grant makes every repository past the cut look
// revoked, and the difference between "the App cannot reach this" and "we
// stopped reading" is the difference between a security finding and a lie. The
// poller is unaffected either way — a shorter list just intersects to fewer
// repos this cycle — which is exactly why the bit has to be carried explicitly
// rather than inferred by the caller.
func TestListInstallationReposComplete_ReportsCompleteness(t *testing.T) {
	t.Run("WholeGrant", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"total_count": 2, "repositories": [
				{"full_name": "octo/repo"},
				{"full_name": "octo/other"}
			]}`))
		}))
		t.Cleanup(srv.Close)

		repos, complete, err := testClient(srv.URL).ListInstallationReposComplete(context.Background())
		if err != nil {
			t.Fatalf("ListInstallationReposComplete: %v", err)
		}
		if !complete {
			t.Error("complete = false for a listing that reached total_count")
		}
		if len(repos) != 2 {
			t.Errorf("got %d repos; want 2", len(repos))
		}
	})

	t.Run("EmptyGrant", func(t *testing.T) {
		// A grant of nothing is a legitimate answer, not a partial read — an
		// installation can be narrowed to no repositories at all. Reporting it
		// incomplete would make the mirror permanently unable to record the one
		// state that most deserves recording.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"total_count": 0, "repositories": []}`))
		}))
		t.Cleanup(srv.Close)

		repos, complete, err := testClient(srv.URL).ListInstallationReposComplete(context.Background())
		if err != nil {
			t.Fatalf("ListInstallationReposComplete: %v", err)
		}
		if !complete {
			t.Error("complete = false for an installation that genuinely grants nothing")
		}
		if len(repos) != 0 {
			t.Errorf("got %d repos; want 0", len(repos))
		}
	})

	t.Run("PagesRanOutBeforeTheAdvertisedCount", func(t *testing.T) {
		// GitHub says there are three and then hands back an empty page. Rare,
		// benign for the poller, and for the mirror precisely a partial fetch.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "1" {
				_, _ = w.Write([]byte(`{"total_count": 3, "repositories": [{"full_name": "octo/repo"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"total_count": 3, "repositories": []}`))
		}))
		t.Cleanup(srv.Close)

		repos, complete, err := testClient(srv.URL).ListInstallationReposComplete(context.Background())
		if err != nil {
			t.Fatalf("ListInstallationReposComplete: %v", err)
		}
		if complete {
			t.Error("complete = true though the pages never reached the advertised total_count")
		}
		if len(repos) != 1 {
			t.Errorf("got %d repos; want the 1 that was actually read", len(repos))
		}
	})

	t.Run("PageCapTruncates", func(t *testing.T) {
		// An enormous grant hits the page cap. The listing is real as far as it
		// goes and must be reported as partial, not as the whole grant.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			page := r.URL.Query().Get("page")
			_, _ = fmt.Fprintf(w, `{"total_count": 100000, "repositories": [{"full_name": "octo/repo-%s"}]}`, page)
		}))
		t.Cleanup(srv.Close)

		_, complete, err := testClient(srv.URL).ListInstallationReposComplete(context.Background())
		if err != nil {
			t.Fatalf("ListInstallationReposComplete: %v", err)
		}
		if complete {
			t.Error("complete = true for a listing cut short by the page cap")
		}
	})
}
