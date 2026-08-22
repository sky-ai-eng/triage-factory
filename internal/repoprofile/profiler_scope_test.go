package repoprofile

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/repoevent"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// docScanServer serves the minimal GitHub API surface a doc-scan pass
// touches: a README (so the repo carries docs) and repo meta.
func docScanServer(t *testing.T) *httptest.Server {
	t.Helper()
	readmeBody := `{"content":"` + base64.StdEncoding.EncodeToString([]byte("# readme")) + `","encoding":"base64"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/contents/README.md"):
			_, _ = w.Write([]byte(readmeBody))
		case strings.Contains(r.URL.Path, "/contents/"):
			w.WriteHeader(http.StatusNotFound)
		default:
			_, _ = w.Write([]byte(`{"default_branch":"main","clone_url":"https://x/own/withdocs.git"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRunOrg_RecipientScopedFanOut is the delivery-scope acceptance test
// for the multi-mode shape: with SetRecipients wired, a repository_updated
// event reaches only the resolved user ids — a client whose teams do not
// track the repository receives no event for it, and nothing about the
// repo (slug, doc flags, profile text) crosses its socket. The resolver
// here is the seam the production wiring fills with
// TeamGitHubRepos.RepoUpdateRecipientsSystem, whose visibility semantics
// the dbtest conformance suite pins on both dialects; the hub's
// per-connection (org, user) filter is pinned in pkg/websocket. Together
// the three layers compose into the invariant.
func TestRunOrg_RecipientScopedFanOut(t *testing.T) {
	srv := docScanServer(t)

	hub := websocket.NewHub()
	tracking := dialHubTestClientAs(t, hub, "user-tracking", "org-1")
	outsider := dialHubTestClientAs(t, hub, "user-outsider", "org-1")
	waitForHubClients(t, hub, 2)

	repos := &batchRepositoryStore{names: []string{"own/withdocs"}}
	p := NewProfiler(fixedResolver{client: github.NewClient(srv.URL, "tok")}, nil, nil, repos, oneOrgStore{}, nil, nil, hub)
	var gotOwner, gotRepo string
	p.SetRecipients(func(_ context.Context, orgID, owner, repo string) ([]string, error) {
		gotOwner, gotRepo = owner, repo
		return []string{"user-tracking"}, nil
	})
	p.batchFn = func(context.Context, string, string, []repoWithDocs, agentproc.SecretsReader) ([]repoProfileResult, error) {
		return []repoProfileResult{{Repo: "own/withdocs", Profile: "a profile"}}, nil
	}

	if _, err := p.RunOrg(context.Background(), "org-1", true); err != nil {
		t.Fatalf("RunOrg: %v", err)
	}

	// The tracking user's client sees both emissions of the cycle — the
	// doc-scan flags, then the AI profile text — as sparse diffs of one
	// event type.
	first := decodeRepoEvent(t, tracking.expectMessage(t, 2*time.Second))
	if first.Type != repoevent.EventType || first.Data.Slug != "own/withdocs" || first.Data.HasReadme == nil || !*first.Data.HasReadme {
		t.Errorf("first event = %+v; want %s doc-flags diff for own/withdocs", first, repoevent.EventType)
	}
	if first.Data.ProfileText != nil {
		t.Errorf("doc-scan diff carries profile_text %q; sparse diffs must hold only the written fields", *first.Data.ProfileText)
	}
	second := decodeRepoEvent(t, tracking.expectMessage(t, 2*time.Second))
	if second.Type != repoevent.EventType || second.Data.ProfileText == nil || *second.Data.ProfileText != "a profile" {
		t.Errorf("second event = %+v; want profile_text diff", second)
	}
	if gotOwner != "own" || gotRepo != "withdocs" {
		t.Errorf("recipients resolved for (%q, %q); want (own, withdocs)", gotOwner, gotRepo)
	}

	// The same-org client outside the recipient set gets nothing at all.
	outsider.expectNoMessage(t, 300*time.Millisecond)
}

// TestRunOrg_LocalShape_OrgScopedBroadcastUnchanged pins the N=1 half of
// the invariant: without SetRecipients (local mode), the cycle's events
// broadcast org-scoped exactly as before the unification — every client
// in the org receives them, no per-user narrowing.
func TestRunOrg_LocalShape_OrgScopedBroadcastUnchanged(t *testing.T) {
	srv := docScanServer(t)

	hub := websocket.NewHub()
	a := dialHubTestClientAs(t, hub, "user-a", "org-1")
	b := dialHubTestClientAs(t, hub, "user-b", "org-1")
	waitForHubClients(t, hub, 2)

	repos := &batchRepositoryStore{names: []string{"own/withdocs"}}
	p := NewProfiler(fixedResolver{client: github.NewClient(srv.URL, "tok")}, nil, nil, repos, oneOrgStore{}, nil, nil, hub)
	p.batchFn = func(context.Context, string, string, []repoWithDocs, agentproc.SecretsReader) ([]repoProfileResult, error) {
		return []repoProfileResult{{Repo: "own/withdocs", Profile: "a profile"}}, nil
	}

	if _, err := p.RunOrg(context.Background(), "org-1", true); err != nil {
		t.Fatalf("RunOrg: %v", err)
	}

	for name, c := range map[string]*hubTestClient{"user-a": a, "user-b": b} {
		evt := decodeRepoEvent(t, c.expectMessage(t, 2*time.Second))
		if evt.Type != repoevent.EventType || evt.Data.Slug != "own/withdocs" {
			t.Errorf("%s: first event = %+v; want %s for own/withdocs", name, evt, repoevent.EventType)
		}
	}
}

// TestRunOrg_BatchFailureToast_ScopedPerUser pins the toast half of the
// delivery scope: a failed batch's warning names repo slugs, which are
// visibility-scoped data, so with SetRecipients wired each affected user
// gets a toast naming ONLY the repos they can see, and a user outside
// every repo's audience gets nothing — toast included.
func TestRunOrg_BatchFailureToast_ScopedPerUser(t *testing.T) {
	srv := docScanServer(t)

	hub := websocket.NewHub()
	x := dialHubTestClientAs(t, hub, "user-x", "org-1")
	y := dialHubTestClientAs(t, hub, "user-y", "org-1")
	z := dialHubTestClientAs(t, hub, "user-z", "org-1")
	waitForHubClients(t, hub, 3)

	repos := &batchRepositoryStore{names: []string{"own/alpha", "own/beta"}}
	p := NewProfiler(fixedResolver{client: github.NewClient(srv.URL, "tok")}, nil, nil, repos, oneOrgStore{}, nil, nil, hub)
	p.SetRecipients(func(_ context.Context, _ string, _, repo string) ([]string, error) {
		if repo == "alpha" {
			return []string{"user-x"}, nil
		}
		return []string{"user-x", "user-y"}, nil
	})
	p.batchFn = func(context.Context, string, string, []repoWithDocs, agentproc.SecretsReader) ([]repoProfileResult, error) {
		return nil, stubErr("simulated batch failure")
	}

	if _, err := p.RunOrg(context.Background(), "org-1", true); err != nil {
		t.Fatalf("RunOrg: %v", err)
	}

	// user-x: doc diffs for both repos, then a toast naming both.
	for _, want := range []string{"own/alpha", "own/beta"} {
		if evt := decodeRepoEvent(t, x.expectMessage(t, 2*time.Second)); evt.Data.Slug != want {
			t.Errorf("user-x event = %+v; want doc diff for %s", evt, want)
		}
	}
	if body := decodeToast(t, x.expectMessage(t, 2*time.Second)); body != "Profiling failed for own/alpha, own/beta — rows saved without AI summary" {
		t.Errorf("user-x toast body = %q; want both slugs", body)
	}

	// user-y: only beta's doc diff, and a toast that must not name alpha.
	if evt := decodeRepoEvent(t, y.expectMessage(t, 2*time.Second)); evt.Data.Slug != "own/beta" {
		t.Errorf("user-y event = %+v; want doc diff for own/beta", evt)
	}
	if body := decodeToast(t, y.expectMessage(t, 2*time.Second)); body != "Profiling failed for own/beta — rows saved without AI summary" {
		t.Errorf("user-y toast body = %q; must name own/beta only", body)
	}

	// user-z is outside every audience: no events, no toast.
	z.expectNoMessage(t, 300*time.Millisecond)
}

// TestRunOrg_BatchFailureToast_LocalOrgWide pins the N=1 half: without
// SetRecipients the failure toast stays a single org-wide broadcast
// naming the whole batch, exactly as before.
func TestRunOrg_BatchFailureToast_LocalOrgWide(t *testing.T) {
	srv := docScanServer(t)

	hub := websocket.NewHub()
	a := dialHubTestClientAs(t, hub, "user-a", "org-1")
	b := dialHubTestClientAs(t, hub, "user-b", "org-1")
	waitForHubClients(t, hub, 2)

	repos := &batchRepositoryStore{names: []string{"own/alpha", "own/beta"}}
	p := NewProfiler(fixedResolver{client: github.NewClient(srv.URL, "tok")}, nil, nil, repos, oneOrgStore{}, nil, nil, hub)
	p.batchFn = func(context.Context, string, string, []repoWithDocs, agentproc.SecretsReader) ([]repoProfileResult, error) {
		return nil, stubErr("simulated batch failure")
	}

	if _, err := p.RunOrg(context.Background(), "org-1", true); err != nil {
		t.Fatalf("RunOrg: %v", err)
	}

	for name, c := range map[string]*hubTestClient{"user-a": a, "user-b": b} {
		c.expectMessage(t, 2*time.Second) // doc diff own/alpha
		c.expectMessage(t, 2*time.Second) // doc diff own/beta
		if body := decodeToast(t, c.expectMessage(t, 2*time.Second)); body != "Profiling failed for own/alpha, own/beta — rows saved without AI summary" {
			t.Errorf("%s toast body = %q; want the whole batch named", name, body)
		}
	}
}

// repoEventFrame is the wire shape the scope tests decode — Type plus the
// repoevent.Update payload under data.
type repoEventFrame struct {
	Type string           `json:"type"`
	Data repoevent.Update `json:"data"`
}

func decodeRepoEvent(t *testing.T, raw []byte) repoEventFrame {
	t.Helper()
	var evt repoEventFrame
	if err := json.Unmarshal(raw, &evt); err != nil {
		t.Fatalf("unmarshal event %s: %v", raw, err)
	}
	if evt.Type != repoevent.EventType {
		t.Fatalf("event type = %q, want %q (frame: %s)", evt.Type, repoevent.EventType, raw)
	}
	return evt
}

// decodeToast asserts the frame is a toast and returns its body.
func decodeToast(t *testing.T, raw []byte) string {
	t.Helper()
	var evt struct {
		Type string `json:"type"`
		Data struct {
			Body string `json:"body"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &evt); err != nil {
		t.Fatalf("unmarshal event %s: %v", raw, err)
	}
	if evt.Type != "toast" {
		t.Fatalf("event type = %q, want toast (frame: %s)", evt.Type, raw)
	}
	return evt.Data.Body
}
