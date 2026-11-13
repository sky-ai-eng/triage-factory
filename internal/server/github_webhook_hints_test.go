package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

const (
	// hintRepoID is GitHub's id for the repository the hint tests track.
	hintRepoID = "9001"
	hintSHA    = "0123456789abcdef0123456789abcdef01234567"
)

// trackHintRepo registers GitHub repository id on github.com and has the
// local team track it, returning its registry id.
func trackHintRepo(t *testing.T, s *Server, id, name string) string {
	t.Helper()
	ctx := context.Background()
	stores := sqlitestore.New(s.db)
	row, err := stores.Repos.FindOrCreateByExternalID(ctx, runmode.LocalDefaultOrgID,
		domain.RepoRef{Host: "https://github.com", Owner: "octo", Repo: name, ExternalID: id})
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	tracked, err := stores.TeamGitHubRepos.ListForTeamSystem(ctx, runmode.LocalDefaultTeamID, "https://github.com")
	if err != nil {
		t.Fatalf("list tracked: %v", err)
	}
	ids := []string{row.ID}
	for _, r := range tracked {
		ids = append(ids, r.RepositoryID)
	}
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, "https://github.com", ids); err != nil {
		t.Fatalf("track %s: %v", name, err)
	}
	return row.ID
}

// refreshMarks reads every pending mark of the local org.
func refreshMarks(t *testing.T, s *Server) []domain.GitHubRefreshMark {
	t.Helper()
	got, err := sqlitestore.New(s.db).GitHubRefreshMarks.ListSystem(context.Background(), runmode.LocalDefaultOrgID, 100)
	if err != nil {
		t.Fatalf("list refresh marks: %v", err)
	}
	return got
}

// markTargets is the set of (repository, target) pairs the marks name.
func markTargets(marks []domain.GitHubRefreshMark) map[[2]string]domain.GitHubRefreshTarget {
	out := map[[2]string]domain.GitHubRefreshTarget{}
	for _, m := range marks {
		out[[2]string{m.RepositoryID, string(m.Target.Kind) + ":" + m.Target.Value}] = m.Target
	}
	return out
}

// captureNudges records every refresh-mark nudge the server fires.
func captureNudges(s *Server) func() []string {
	var mu sync.Mutex
	var got []string
	s.SetOnGitHubRefreshMarked(func(orgID string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, orgID)
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// TestGitHubWebhook_RefreshMarks covers each content event TF's App
// subscribes to: the delivery writes a mark for every target it names in a
// tracked repository, and nudges the holder once.
func TestGitHubWebhook_RefreshMarks(t *testing.T) {
	const repo = `"repository":{"id":9001},"installation":{"id":4242}`
	for _, tc := range []struct {
		event string
		body  string
		want  []domain.GitHubRefreshTarget
	}{
		{"pull_request", `{"action":"synchronize","number":7,"pull_request":{"number":7},` + repo + `}`,
			[]domain.GitHubRefreshTarget{domain.PullRequestRefreshTarget(7)}},
		{"pull_request_review", `{"action":"submitted","pull_request":{"number":7},` + repo + `}`,
			[]domain.GitHubRefreshTarget{domain.PullRequestRefreshTarget(7)}},
		{"pull_request_review_comment", `{"action":"created","pull_request":{"number":7},` + repo + `}`,
			[]domain.GitHubRefreshTarget{domain.PullRequestRefreshTarget(7)}},
		{"issue_comment", `{"action":"created","issue":{"number":12,"pull_request":{"url":"https://api.github.com/repos/octo/api/pulls/12"}},` + repo + `}`,
			[]domain.GitHubRefreshTarget{domain.PullRequestRefreshTarget(12)}},
		// A listed pull request based in another repository is numbered there,
		// so only the delivery's own repository's pull requests are marked; a
		// pull request listed twice is one mark.
		{"check_run", `{"action":"completed","check_run":{"head_sha":"` + hintSHA + `","pull_requests":[` +
			`{"number":7,"base":{"repo":{"id":9001}}},{"number":8,"base":{"repo":{"id":9001}}},` +
			`{"number":7,"base":{"repo":{"id":9001}}},{"number":3,"base":{"repo":{"id":5555}}}]},` + repo + `}`,
			[]domain.GitHubRefreshTarget{domain.HeadSHARefreshTarget(hintSHA), domain.PullRequestRefreshTarget(7), domain.PullRequestRefreshTarget(8)}},
		// A fork's pull request is not listed; the head SHA is the only mark.
		{"check_suite", `{"action":"completed","check_suite":{"head_sha":"` + hintSHA + `","pull_requests":[]},` + repo + `}`,
			[]domain.GitHubRefreshTarget{domain.HeadSHARefreshTarget(hintSHA)}},
		{"push", `{"ref":"refs/heads/feature/x",` + repo + `}`,
			[]domain.GitHubRefreshTarget{domain.BranchRefreshTarget("feature/x")}},
	} {
		t.Run(tc.event, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			s := newTestServer(t)
			seedWebhookApp(t, s)
			repoRow := trackHintRepo(t, s, hintRepoID, "api")
			nudges := captureNudges(s)

			body := []byte(tc.body)
			if rec := postWebhook(s, tc.event, sign(body), body); rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
			}

			marks := refreshMarks(t, s)
			got := markTargets(marks)
			if len(marks) != len(tc.want) {
				t.Errorf("marks = %+v, want %d", marks, len(tc.want))
			}
			for _, want := range tc.want {
				if _, ok := got[[2]string{repoRow, string(want.Kind) + ":" + want.Value}]; !ok {
					t.Errorf("no mark for %+v on the tracked repository; marks = %+v", want, marks)
				}
			}
			if n := nudges(); len(n) != 1 || n[0] != runmode.LocalDefaultOrgID {
				t.Errorf("nudges = %v, want one for the org", n)
			}
		})
	}
}

// TestGitHubWebhook_RefreshMarks_NamesNothing: a content delivery that names
// nothing to refresh — a comment on an issue, a tag push — is recorded and
// acknowledged, and writes no mark and sends no nudge.
func TestGitHubWebhook_RefreshMarks_NamesNothing(t *testing.T) {
	for _, tc := range []struct{ name, event, body string }{
		{"comment on an issue", "issue_comment", `{"action":"created","issue":{"number":12},"repository":{"id":9001}}`},
		{"comment on an issue, null pull_request", "issue_comment", `{"action":"created","issue":{"number":12,"pull_request":null},"repository":{"id":9001}}`},
		{"tag push", "push", `{"ref":"refs/tags/v1.0.0","repository":{"id":9001}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, runmode.ModeLocal)
			s := newTestServer(t)
			seedWebhookApp(t, s)
			trackHintRepo(t, s, hintRepoID, "api")
			nudges := captureNudges(s)

			body := []byte(tc.body)
			if rec := postWebhookDelivery(s, tc.event, sign(body), body, "gh-delivery-nothing"); rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
			}
			if marks := refreshMarks(t, s); len(marks) != 0 {
				t.Errorf("marks = %+v, want none", marks)
			}
			if n := nudges(); len(n) != 0 {
				t.Errorf("nudges = %v, want none", n)
			}
			if n := deliveryRows(t, s); n != 1 {
				t.Errorf("delivery rows = %d, want the delivery recorded", n)
			}
		})
	}
}

// TestGitHubWebhook_DependabotAlertIsRecordedOnly: the App subscribes to
// dependabot_alert, and a delivery of it in a tracked repository is recorded
// and acknowledged and does nothing else yet: no mark and no nudge.
func TestGitHubWebhook_DependabotAlertIsRecordedOnly(t *testing.T) {
	if hintedEvent("dependabot_alert") {
		t.Fatal("dependabot_alert is parsed as a refresh hint, which names no alert target yet")
	}
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)
	trackHintRepo(t, s, hintRepoID, "api")
	nudges := captureNudges(s)

	body := []byte(`{"action":"created","alert":{"number":3,"state":"open",` +
		`"dependency":{"package":{"ecosystem":"npm","name":"lodash"},"manifest_path":"package-lock.json"}},` +
		`"repository":{"id":9001},"installation":{"id":4242}}`)
	if rec := postWebhookDelivery(s, "dependabot_alert", sign(body), body, "gh-delivery-dependabot"); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if marks := refreshMarks(t, s); len(marks) != 0 {
		t.Errorf("marks = %+v, want none", marks)
	}
	if n := nudges(); len(n) != 0 {
		t.Errorf("nudges = %v, want none", n)
	}
	if n := deliveryRows(t, s); n != 1 {
		t.Errorf("delivery rows = %d, want the delivery recorded", n)
	}
}

// TestGitHubWebhook_RefreshMarks_UntrackedRepository: a delivery for a
// repository no team tracks, or one TF holds no row for, is recorded and
// acknowledged, writes no mark and sends no nudge, and creates no registry row.
func TestGitHubWebhook_RefreshMarks_UntrackedRepository(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)
	trackHintRepo(t, s, hintRepoID, "api")
	if _, err := sqlitestore.New(s.db).Repos.FindOrCreateByExternalID(context.Background(), runmode.LocalDefaultOrgID,
		domain.RepoRef{Host: "https://github.com", Owner: "octo", Repo: "untracked", ExternalID: "9002"}); err != nil {
		t.Fatalf("register untracked: %v", err)
	}
	nudges := captureNudges(s)
	before := registryRowCount(t, s)

	for _, id := range []string{"9002", "9003"} {
		body := []byte(`{"action":"opened","pull_request":{"number":7},"repository":{"id":` + id + `}}`)
		if rec := postWebhookDelivery(s, "pull_request", sign(body), body, "gh-delivery-"+id); rec.Code != http.StatusNoContent {
			t.Fatalf("repository %s: status = %d, want 204; body=%s", id, rec.Code, rec.Body.String())
		}
	}
	if marks := refreshMarks(t, s); len(marks) != 0 {
		t.Errorf("marks = %+v, want none", marks)
	}
	if n := nudges(); len(n) != 0 {
		t.Errorf("nudges = %v, want none", n)
	}
	if n := deliveryRows(t, s); n != 2 {
		t.Errorf("delivery rows = %d, want both deliveries recorded", n)
	}
	if after := registryRowCount(t, s); after != before {
		t.Errorf("registry rows = %d after the deliveries, want %d — a delivery creates no repository", after, before)
	}
}

func registryRowCount(t *testing.T, s *Server) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM repositories`).Scan(&n); err != nil {
		t.Fatalf("count repositories: %v", err)
	}
	return n
}

// TestGitHubWebhook_RefreshMarks_Redelivery: a redelivered content delivery
// is a duplicate: its mark is not written again and no second nudge is sent.
func TestGitHubWebhook_RefreshMarks_Redelivery(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)
	trackHintRepo(t, s, hintRepoID, "api")
	nudges := captureNudges(s)

	body := []byte(`{"action":"opened","pull_request":{"number":7},"repository":{"id":9001}}`)
	for range 2 {
		if rec := postWebhookDelivery(s, "pull_request", sign(body), body, "gh-delivery-twice"); rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
		}
	}
	marks := refreshMarks(t, s)
	if len(marks) != 1 || marks[0].Revision != 1 {
		t.Errorf("marks = %+v, want one mark at revision 1", marks)
	}
	if n := nudges(); len(n) != 1 {
		t.Errorf("nudges = %v, want one", n)
	}

	// A distinct delivery for the same pull request lands on the same mark.
	if rec := postWebhookDelivery(s, "pull_request", sign(body), body, "gh-delivery-again"); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if marks := refreshMarks(t, s); len(marks) != 1 || marks[0].Revision != 2 {
		t.Errorf("marks = %+v, want the one mark at revision 2", marks)
	}
}

type failingMarks struct{ db.GitHubRefreshMarkStore }

func (failingMarks) MarkSystem(context.Context, string, string, domain.GitHubRefreshTarget) (domain.GitHubRefreshMark, error) {
	return domain.GitHubRefreshMark{}, errors.New("mark write failed")
}

// TestGitHubWebhook_RefreshMarks_FailedMarkLeavesDeliveryUnrecorded: when the
// mark write fails, the delivery's record rolls back with it, so the receiver
// answers 5xx and a redelivery applies the delivery.
func TestGitHubWebhook_RefreshMarks_FailedMarkLeavesDeliveryUnrecorded(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	var failing bool
	s := newTestServerWithTx(t, func(tx db.TxRunner) db.TxRunner {
		return switchableTx{TxRunner: tx, failing: &failing}
	})
	seedWebhookApp(t, s)
	trackHintRepo(t, s, hintRepoID, "api")
	nudges := captureNudges(s)

	body := []byte(`{"action":"opened","pull_request":{"number":7},"repository":{"id":9001}}`)
	failing = true
	if rec := postWebhookDelivery(s, "pull_request", sign(body), body, "gh-delivery-retry"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d with the mark write failing, want 500", rec.Code)
	}
	if n := deliveryRows(t, s); n != 0 {
		t.Errorf("delivery rows = %d after a failed mark write, want 0", n)
	}
	if marks := refreshMarks(t, s); len(marks) != 0 {
		t.Errorf("marks = %+v after a failed mark write, want none", marks)
	}
	if n := nudges(); len(n) != 0 {
		t.Errorf("nudges = %v after a failed mark write, want none", n)
	}

	failing = false
	if rec := postWebhookDelivery(s, "pull_request", sign(body), body, "gh-delivery-retry"); rec.Code != http.StatusNoContent {
		t.Fatalf("redelivery status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if marks := refreshMarks(t, s); len(marks) != 1 || marks[0].Target != domain.PullRequestRefreshTarget(7) {
		t.Errorf("marks = %+v after the redelivery, want pull request 7", marks)
	}
	if n := nudges(); len(n) != 1 {
		t.Errorf("nudges = %v after the redelivery, want one", n)
	}
}

// switchableTx hands every system transaction a refresh-mark store whose
// writes fail while *failing is set, so the transaction rolls back.
type switchableTx struct {
	db.TxRunner
	failing *bool
}

func (s switchableTx) SystemWithTx(ctx context.Context, orgID string, fn func(db.SystemTxStores) error) error {
	return s.TxRunner.SystemWithTx(ctx, orgID, func(tx db.SystemTxStores) error {
		if *s.failing {
			tx.GitHubRefreshMarks = failingMarks{}
		}
		return fn(tx)
	})
}

// failingRepos fails the read a delivery's repository is resolved through.
type failingRepos struct{ db.RepositoryStore }

func (failingRepos) GetTrackedByExternalIDSystem(context.Context, string, string, string) (*domain.Repository, error) {
	return nil, errors.New("registry unreadable")
}

// TestGitHubWebhook_RefreshMarks_UnreadableRegistryLeavesDeliveryUnrecorded:
// the repository is resolved ahead of the record, so a failed read answers
// 5xx with nothing recorded.
func TestGitHubWebhook_RefreshMarks_UnreadableRegistryLeavesDeliveryUnrecorded(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)
	trackHintRepo(t, s, hintRepoID, "api")
	s.repos = failingRepos{s.repos}

	body := []byte(`{"action":"opened","pull_request":{"number":7},"repository":{"id":9001}}`)
	if rec := postWebhook(s, "pull_request", sign(body), body); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if n := deliveryRows(t, s); n != 0 {
		t.Errorf("delivery rows = %d, want 0", n)
	}
}

// TestGitHubWebhook_RefreshMarks_MalformedStaysBadRequest: a content delivery
// missing what its hint is read from is refused ahead of the dedup gate, so it
// is 400 on every attempt and records nothing.
func TestGitHubWebhook_RefreshMarks_MalformedStaysBadRequest(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)
	trackHintRepo(t, s, hintRepoID, "api")

	for _, tc := range []struct{ name, event, body string }{
		{"unparseable", "pull_request", `{"action":"opened","pull_request":`},
		{"no repository", "pull_request", `{"action":"opened","pull_request":{"number":7}}`},
		{"no pull request", "pull_request_review", `{"action":"submitted","repository":{"id":9001}}`},
		{"no pull request number", "pull_request", `{"action":"opened","pull_request":{},"repository":{"id":9001}}`},
		{"no issue", "issue_comment", `{"action":"created","repository":{"id":9001}}`},
		{"comment on a pull request with no number", "issue_comment", `{"action":"created","issue":{"pull_request":{}},"repository":{"id":9001}}`},
		{"no check run", "check_run", `{"action":"completed","repository":{"id":9001}}`},
		{"short head SHA", "check_suite", `{"action":"completed","check_suite":{"head_sha":"0123abc"},"repository":{"id":9001}}`},
		{"listed pull request with no number", "check_run", `{"action":"completed","check_run":{"head_sha":"` + hintSHA + `","pull_requests":[{}]},"repository":{"id":9001}}`},
		{"no ref", "push", `{"repository":{"id":9001}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			for attempt := 1; attempt <= 2; attempt++ {
				if rec := postWebhookDelivery(s, tc.event, sign(body), body, "gh-delivery-"+tc.name); rec.Code != http.StatusBadRequest {
					t.Fatalf("attempt %d: status = %d, want 400", attempt, rec.Code)
				}
			}
		})
	}
	if n := deliveryRows(t, s); n != 0 {
		t.Errorf("delivery rows = %d, want 0", n)
	}
	if marks := refreshMarks(t, s); len(marks) != 0 {
		t.Errorf("marks = %+v, want none", marks)
	}
}

// TestDeploymentWebhook_RefreshMarks: the deployment App's receiver writes
// marks under the org the installation is bound to, resolving the repository
// on the deployment's GitHub.
func TestDeploymentWebhook_RefreshMarks(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	s := newTestServer(t)
	seedDeploymentWebhook(t, s)
	bindInstallation(t, s, "4242", "https://github.com")
	repoRow := trackHintRepo(t, s, hintRepoID, "api")
	nudges := captureNudges(s)

	body := []byte(`{"action":"completed","check_suite":{"head_sha":"` + hintSHA + `","pull_requests":[{"number":7,"base":{"repo":{"id":9001}}}]},"repository":{"id":9001},"installation":{"id":4242}}`)
	if rec := postDeploymentWebhook(s, "check_suite", signDeployment(body), body); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	got := markTargets(refreshMarks(t, s))
	for _, want := range []domain.GitHubRefreshTarget{domain.HeadSHARefreshTarget(hintSHA), domain.PullRequestRefreshTarget(7)} {
		if _, ok := got[[2]string{repoRow, string(want.Kind) + ":" + want.Value}]; !ok {
			t.Errorf("no mark for %+v; marks = %+v", want, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("marks = %+v, want 2", got)
	}
	if n := nudges(); len(n) != 1 || n[0] != runmode.LocalDefaultOrgID {
		t.Errorf("nudges = %v, want one for the bound org", n)
	}

	// An unbound installation names no org, so nothing is marked for anyone.
	unbound := []byte(`{"action":"opened","pull_request":{"number":8},"repository":{"id":9001},"installation":{"id":5151}}`)
	if rec := postDeploymentWebhook(s, "pull_request", signDeployment(unbound), unbound); rec.Code != http.StatusNoContent {
		t.Fatalf("unbound status = %d, want 204", rec.Code)
	}
	if got := refreshMarks(t, s); len(got) != 2 {
		t.Errorf("marks = %+v after an unbound delivery, want the two from before", got)
	}
}
