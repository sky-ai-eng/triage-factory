package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRefreshGitHub_PollCompleteSentinel_OnlyOnFullWrap is TFAC-571's decision
// on what the poll-completion sentinel means for a partial cycle: it fires
// only once discovery has covered every repo passed in (a full wrap), never
// when ErrRateLimited cuts the fan-out short partway through. Downstream
// subscribers (scorer, profiler, reconciler, poll-tracker) all
// key off system:poll:completed to kick a pass over the org — firing it on a
// partial cold-start cycle would have them churn on incomplete data every
// time a large tracked set can't finish within one rate budget.
func TestRefreshGitHub_PollCompleteSentinel_OnlyOnFullWrap(t *testing.T) {
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "1")

	repos := []string{"octo/ok1", "octo/limited"}
	resetAt := time.Now().Add(time.Hour) // beyond maxRateLimitWait — immediate ErrRateLimited, no retry-sleep

	var allow bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			// Phase-2 RefreshPRs stub: report the node inaccessible so the
			// refresh is a no-op and the seeded discovery snapshot stands —
			// this test only cares about the poll-completed sentinel, not
			// what Phase 3 diffs.
			_, _ = w.Write([]byte(`{"data":{"nodes":[null]}}`))
		case strings.Contains(r.URL.Path, "/limited/") && !allow:
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", resetAt.Unix()))
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for 127.0.0.1."}`))
		case strings.Contains(r.URL.Path, "/ok1/"):
			// A real open PR so ListActiveSystem has something to gate
			// EmitPollComplete on (it's a no-op when there are zero active
			// entities, regardless of wrap completeness).
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(openPRBody))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	if err := stores.Repos.SetConfigured(ctx, org, repos); err != nil {
		t.Fatalf("SetConfigured: %v", err)
	}

	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)

	// Rate-limited (partial) cycle: octo/ok1 can seed an entity, but the poll-completed
	// sentinel must stay silent until a later cycle fully wraps the repo list.
	if _, _, err := tr.RefreshGitHub(ctx, ghclient.NewClient(srv.URL, "tok"), "", repos, nil); err == nil {
		t.Fatal("RefreshGitHub error = nil; want ErrRateLimited")
	}
	for _, evt := range pub.events {
		if evt.EventType == domain.EventSystemPollCompleted {
			t.Fatalf("poll-completed sentinel fired on a rate-limited (partial) cycle; want it suppressed until a full wrap")
		}
	}

	// "Rate limit window resets" — both repos now list successfully, so this
	// cycle fully wraps the list. A fresh client models what the poller
	// actually does each cycle (resolver.ClientFor mints a new *ghclient.Client
	// per call — see internal/github/resolver.go) — reusing the first
	// client's rate-limit memory would keep its pre-flight budget check
	// (awaitBudget) refusing requests until the real reset time passed.
	allow = true
	if _, _, err := tr.RefreshGitHub(ctx, ghclient.NewClient(srv.URL, "tok"), "", repos, nil); err != nil {
		t.Fatalf("RefreshGitHub (full wrap): %v", err)
	}
	var found bool
	for _, evt := range pub.events {
		if evt.EventType != domain.EventSystemPollCompleted {
			continue
		}
		found = true
		var meta struct {
			Source string `json:"source"`
		}
		if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
			t.Fatalf("parse poll-completed metadata: %v", err)
		}
		if meta.Source != "github" {
			t.Errorf("poll-completed source = %q; want %q", meta.Source, "github")
		}
	}
	if !found {
		t.Fatal("poll-completed sentinel did not fire on a fully-wrapped cycle")
	}
}
