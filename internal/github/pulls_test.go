package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// servePRFixture returns a handler that responds to the four URLs
// GetPR hits. The PR body comes from prJSON; reviews and comments
// endpoints all return empty arrays. Anything else fails the test —
// surfacing accidental new GetPR call sites that would silently 404
// with the rest of the unit tests still passing.
func servePRFixture(t *testing.T, prJSON string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/42") ||
			strings.HasSuffix(r.URL.Path, "/pulls/7") ||
			strings.HasSuffix(r.URL.Path, "/pulls/99"):
			_, _ = w.Write([]byte(prJSON))
		case strings.Contains(r.URL.Path, "/pulls/") && (strings.HasSuffix(r.URL.Path, "/reviews") || strings.HasSuffix(r.URL.Path, "/comments")):
			_, _ = w.Write([]byte("[]"))
		case strings.Contains(r.URL.Path, "/issues/") && strings.HasSuffix(r.URL.Path, "/comments"):
			_, _ = w.Write([]byte("[]"))
		default:
			t.Errorf("unexpected URL: %s", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
}

// TestGetPR_ForkPR_ParsesHeadAndBaseCloneURLs locks down the parsing
// of base.repo.clone_url, head.repo.clone_url, and the SSH variants
// (base.repo.ssh_url, head.repo.ssh_url). setupGitHub depends
// on the upstream URL coming from base.repo.clone_url (anything else
// would point the bare's origin at a fork) and on head.repo.clone_url
// for fork-tracking configuration. The SSH equivalents are picked when
// GitHubConfig.CloneProtocol == "ssh". If the GitHub API ever moves
// these fields or the parser regresses, this test catches it before
// every PR delegation starts pushing to the wrong place.
func TestGetPR_ForkPR_ParsesHeadAndBaseCloneURLs(t *testing.T) {
	prJSON := `{
		"number": 42,
		"title": "Fork PR",
		"state": "open",
		"head": {
			"ref": "feature-branch",
			"sha": "abc123",
			"repo": {
				"clone_url": "https://github.com/contributor/forked-repo.git",
				"ssh_url":   "git@github.com:contributor/forked-repo.git"
			}
		},
		"base": {
			"ref": "main",
			"repo": {
				"clone_url": "https://github.com/upstream-owner/upstream-repo.git",
				"ssh_url":   "git@github.com:upstream-owner/upstream-repo.git"
			}
		}
	}`
	srv := httptest.NewServer(servePRFixture(t, prJSON))
	t.Cleanup(srv.Close)

	pr, err := clientAgainst(srv.URL).GetPR("upstream-owner", "upstream-repo", 42, false)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if pr.CloneURL != "https://github.com/contributor/forked-repo.git" {
		t.Errorf("CloneURL (head fork URL) = %q, want %q", pr.CloneURL, "https://github.com/contributor/forked-repo.git")
	}
	if pr.BaseCloneURL != "https://github.com/upstream-owner/upstream-repo.git" {
		t.Errorf("BaseCloneURL (upstream URL) = %q, want %q", pr.BaseCloneURL, "https://github.com/upstream-owner/upstream-repo.git")
	}
	if pr.SSHURL != "git@github.com:contributor/forked-repo.git" {
		t.Errorf("SSHURL (head fork SSH URL) = %q, want %q", pr.SSHURL, "git@github.com:contributor/forked-repo.git")
	}
	if pr.BaseSSHURL != "git@github.com:upstream-owner/upstream-repo.git" {
		t.Errorf("BaseSSHURL (upstream SSH URL) = %q, want %q", pr.BaseSSHURL, "git@github.com:upstream-owner/upstream-repo.git")
	}
	if pr.HeadRef != "feature-branch" {
		t.Errorf("HeadRef = %q, want %q", pr.HeadRef, "feature-branch")
	}
	if pr.BaseRef != "main" {
		t.Errorf("BaseRef = %q, want %q", pr.BaseRef, "main")
	}
}

// TestGetPR_OwnRepoPR_HeadAndBaseEqual verifies that when head.repo
// and base.repo point at the same repo, both clone URLs come back
// identical. The spawner uses this equality to decide whether to
// skip the fork-tracking setup; if the parser ever fails to populate
// one of them, the spawner would treat an own-repo PR as a fork
// (or vice versa) and configure pushes incorrectly.
func TestGetPR_OwnRepoPR_HeadAndBaseEqual(t *testing.T) {
	prJSON := `{
		"number": 7,
		"title": "Own PR",
		"state": "open",
		"head": {
			"ref": "my-feature",
			"sha": "def456",
			"repo": {
				"clone_url": "https://github.com/me/myrepo.git",
				"ssh_url":   "git@github.com:me/myrepo.git"
			}
		},
		"base": {
			"ref": "main",
			"repo": {
				"clone_url": "https://github.com/me/myrepo.git",
				"ssh_url":   "git@github.com:me/myrepo.git"
			}
		}
	}`
	srv := httptest.NewServer(servePRFixture(t, prJSON))
	t.Cleanup(srv.Close)

	pr, err := clientAgainst(srv.URL).GetPR("me", "myrepo", 7, false)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if pr.CloneURL == "" || pr.BaseCloneURL == "" {
		t.Fatalf("expected both URLs populated; got CloneURL=%q BaseCloneURL=%q", pr.CloneURL, pr.BaseCloneURL)
	}
	if pr.CloneURL != pr.BaseCloneURL {
		t.Errorf("own-repo PR: head and base clone URLs should be equal; got CloneURL=%q BaseCloneURL=%q", pr.CloneURL, pr.BaseCloneURL)
	}
	if pr.SSHURL == "" || pr.BaseSSHURL == "" {
		t.Fatalf("expected both SSH URLs populated; got SSHURL=%q BaseSSHURL=%q", pr.SSHURL, pr.BaseSSHURL)
	}
	if pr.SSHURL != pr.BaseSSHURL {
		t.Errorf("own-repo PR: head and base SSH URLs should be equal; got SSHURL=%q BaseSSHURL=%q", pr.SSHURL, pr.BaseSSHURL)
	}
}

// TestGetPR_DeletedFork_BaseStillPopulated covers the GitHub edge
// case where head.repo is null because the contributor's fork was
// deleted. The parser must leave CloneURL empty (not panic on the
// null) AND still populate BaseCloneURL and HeadRef so deleted-fork
// PRs can still be recognized and handled using the base repository
// metadata, including creating a read-only worktree when needed.
func TestGetPR_DeletedFork_BaseStillPopulated(t *testing.T) {
	prJSON := `{
		"number": 99,
		"title": "Deleted-fork PR",
		"state": "closed",
		"head": {
			"ref": "deleted-branch",
			"sha": "fff999",
			"repo": null
		},
		"base": {
			"ref": "main",
			"repo": {
				"clone_url": "https://github.com/me/myrepo.git",
				"ssh_url":   "git@github.com:me/myrepo.git"
			}
		}
	}`
	srv := httptest.NewServer(servePRFixture(t, prJSON))
	t.Cleanup(srv.Close)

	pr, err := clientAgainst(srv.URL).GetPR("me", "myrepo", 99, false)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if pr.CloneURL != "" {
		t.Errorf("CloneURL should be empty when head.repo is null; got %q", pr.CloneURL)
	}
	if pr.SSHURL != "" {
		t.Errorf("SSHURL should be empty when head.repo is null; got %q", pr.SSHURL)
	}
	if pr.BaseCloneURL != "https://github.com/me/myrepo.git" {
		t.Errorf("BaseCloneURL = %q, want %q (must survive deleted-fork)", pr.BaseCloneURL, "https://github.com/me/myrepo.git")
	}
	if pr.BaseSSHURL != "git@github.com:me/myrepo.git" {
		t.Errorf("BaseSSHURL = %q, want %q (must survive deleted-fork)", pr.BaseSSHURL, "git@github.com:me/myrepo.git")
	}
	if pr.HeadRef != "deleted-branch" {
		t.Errorf("HeadRef = %q, want %q (head.ref still parseable when repo is null)", pr.HeadRef, "deleted-branch")
	}
}

func makePRFilesList(count int, prefix string) []map[string]any {
	files := make([]map[string]any, count)
	for i := range files {
		files[i] = map[string]any{
			"filename":  fmt.Sprintf("%s_file_%d.go", prefix, i),
			"status":    "modified",
			"additions": 1,
			"deletions": 1,
			"patch":     "@@ -1,1 +1,1 @@\n+new\n",
		}
	}
	return files
}

func TestGetPRFiles_SinglePage(t *testing.T) {
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		data, _ := json.Marshal(makePRFilesList(50, "p1"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	got, err := c.GetPRFiles("owner", "repo", 1)
	if err != nil {
		t.Fatalf("GetPRFiles: %v", err)
	}
	if len(got) != 50 {
		t.Errorf("expected 50 files, got %d", len(got))
	}
	if callCount != 1 {
		t.Errorf("expected 1 API call, got %d", callCount)
	}
}

func TestGetPRFiles_MultiPage(t *testing.T) {
	// page 1: 100, page 2: 100, page 3: 30 → 230 total, 3 calls
	pageHits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page == "" {
			page = "1"
		}
		pageHits[page]++

		var count int
		switch page {
		case "1":
			count = 100
		case "2":
			count = 100
		case "3":
			count = 30
		default:
			t.Errorf("unexpected page %s requested", page)
			http.Error(w, "unexpected page", http.StatusBadRequest)
			return
		}

		data, _ := json.Marshal(makePRFilesList(count, "p"+page))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	got, err := c.GetPRFiles("owner", "repo", 1)
	if err != nil {
		t.Fatalf("GetPRFiles multi-page: %v", err)
	}
	if len(got) != 230 {
		t.Errorf("expected 230 files, got %d", len(got))
	}
	for _, pg := range []string{"1", "2", "3"} {
		if pageHits[pg] != 1 {
			t.Errorf("expected 1 hit for page %s, got %d", pg, pageHits[pg])
		}
	}
}

func TestGetPRFiles_CapAt1000(t *testing.T) {
	// Every page returns 100 files; should stop after 10 pages (1000 total).
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		data, _ := json.Marshal(makePRFilesList(100, fmt.Sprintf("call%d", callCount)))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	got, err := c.GetPRFiles("owner", "repo", 1)
	if err != nil {
		t.Fatalf("GetPRFiles cap: %v", err)
	}
	if len(got) != MaxPRFiles {
		t.Errorf("expected %d files (cap), got %d", MaxPRFiles, len(got))
	}
	if callCount != MaxPRFiles/100 {
		t.Errorf("expected %d API calls, got %d", MaxPRFiles/100, callCount)
	}
}

func TestGetPRFiles_ErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	_, err := c.GetPRFiles("owner", "repo", 1)
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
}

func TestGetPRFiles_SecondPageError(t *testing.T) {
	// First page succeeds, second page fails — error should propagate.
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			data, _ := json.Marshal(makePRFilesList(100, "p1"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)
			return
		}
		http.Error(w, `{"message":"rate limited"}`, http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	_, err := c.GetPRFiles("owner", "repo", 1)
	if err == nil {
		t.Fatal("expected error when second page fails, got nil")
	}
}

// TestCreatePR_HappyPath asserts the request shape and that we parse
// number + html_url out of GitHub's response. This is the canonical
// path the pending-PR submit handler walks on user approval.
func TestCreatePR_HappyPath(t *testing.T) {
	var (
		gotMethod, gotPath string
		gotBody            map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number": 42, "html_url": "https://github.com/owner/repo/pull/42", "node_id": "PR_node42"}`))
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	number, url, nodeID, err := c.CreatePR("owner", "repo", "feature/SKY-1", "main", "Add idempotency", "Body text", false)
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if number != 42 {
		t.Errorf("number = %d, want 42", number)
	}
	if url != "https://github.com/owner/repo/pull/42" {
		t.Errorf("url = %q, want canonical github.com path", url)
	}
	if nodeID != "PR_node42" {
		t.Errorf("nodeID = %q, want PR_node42 (the durable GraphQL handle)", nodeID)
	}
	if gotMethod != "POST" {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/repos/owner/repo/pulls" {
		t.Errorf("path = %q, want /repos/owner/repo/pulls", gotPath)
	}
	for _, want := range []struct{ key, val string }{
		{"title", "Add idempotency"},
		{"head", "feature/SKY-1"},
		{"base", "main"},
		{"body", "Body text"},
	} {
		if got, _ := gotBody[want.key].(string); got != want.val {
			t.Errorf("body[%q] = %q, want %q", want.key, got, want.val)
		}
	}
	if draft, _ := gotBody["draft"].(bool); draft {
		t.Errorf("draft = true, want false (not draft)")
	}
}

// TestCreatePR_DraftFlagPropagated confirms the draft boolean rides
// through to the request body verbatim. UI exposes the draft toggle
// at submit time; if this regressed, the toggle would silently no-op.
func TestCreatePR_DraftFlagPropagated(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number": 1, "html_url": "https://github.com/o/r/pull/1"}`))
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	if _, _, _, err := c.CreatePR("o", "r", "h", "main", "T", "B", true); err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if draft, _ := gotBody["draft"].(bool); !draft {
		t.Errorf("draft = false, want true (draft requested)")
	}
}

// TestCreatePR_422_BaseMissing pins the surfacing-the-message contract
// for the most common GitHub error: caller specified a base that
// doesn't exist on the upstream. The nested errors[].message must
// land in the returned error (rather than just the raw JSON blob)
// so the user sees the actionable reason in the submit-handler's 502.
func TestCreatePR_422_BaseMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"message":"base 'develop' is not a valid branch"}]}`))
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	_, _, _, err := c.CreatePR("o", "r", "h", "develop", "T", "B", false)
	if err == nil {
		t.Fatal("expected error for 422, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Validation Failed") {
		t.Errorf("expected error to surface 'Validation Failed', got %q", msg)
	}
	if !strings.Contains(msg, "base 'develop' is not a valid branch") {
		t.Errorf("expected error to surface nested message, got %q", msg)
	}
}

// TestCreatePR_422_FieldErr covers the other common 422 shape:
// errors[].field+code instead of errors[].message (e.g. invalid
// head ref). Field-level errors should still be readable.
func TestCreatePR_422_FieldErr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"invalid","field":"head"}]}`))
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	_, _, _, err := c.CreatePR("o", "r", "ghost-branch", "main", "T", "B", false)
	if err == nil {
		t.Fatal("expected error for 422, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "head") || !strings.Contains(msg, "invalid") {
		t.Errorf("expected error to mention invalid head field, got %q", msg)
	}
}

// TestUpdatePR_HappyPath asserts UpdatePR issues a PATCH to the PR endpoint
// with title and body sent verbatim (a whole-field replace). This is the edit
// path the B·2 PR rework drives when an approved preview's title/body change.
func TestUpdatePR_HappyPath(t *testing.T) {
	var (
		gotMethod, gotPath string
		gotBody            map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number": 42}`))
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	if err := c.UpdatePR("owner", "repo", 42, "New title", "New body"); err != nil {
		t.Fatalf("UpdatePR: %v", err)
	}
	if gotMethod != "PATCH" {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if gotPath != "/repos/owner/repo/pulls/42" {
		t.Errorf("path = %q, want /repos/owner/repo/pulls/42", gotPath)
	}
	if got, _ := gotBody["title"].(string); got != "New title" {
		t.Errorf("body[title] = %q, want %q", got, "New title")
	}
	if got, _ := gotBody["body"].(string); got != "New body" {
		t.Errorf("body[body] = %q, want %q", got, "New body")
	}
}

// TestUpdatePR_422 confirms a validation failure is lifted to a readable
// message (same liftValidationErr path as CreatePR) rather than the raw JSON.
func TestUpdatePR_422(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"message":"title cannot be blank"}]}`))
	}))
	t.Cleanup(srv.Close)

	err := clientAgainst(srv.URL).UpdatePR("o", "r", 1, "", "body")
	if err == nil {
		t.Fatal("expected error for 422, got nil")
	}
	if !strings.Contains(err.Error(), "title cannot be blank") {
		t.Errorf("expected lifted validation message, got %q", err.Error())
	}
}

// TestClosePR_HappyPath asserts ClosePR issues a PATCH to the PR endpoint with
// state=closed — the abandon path's "close the draft, keep the branch" action.
func TestClosePR_HappyPath(t *testing.T) {
	var (
		gotMethod, gotPath string
		gotBody            map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number": 42, "state": "closed"}`))
	}))
	t.Cleanup(srv.Close)

	c := clientAgainst(srv.URL)
	if err := c.ClosePR("owner", "repo", 42); err != nil {
		t.Fatalf("ClosePR: %v", err)
	}
	if gotMethod != "PATCH" {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if gotPath != "/repos/owner/repo/pulls/42" {
		t.Errorf("path = %q, want /repos/owner/repo/pulls/42", gotPath)
	}
	if got, _ := gotBody["state"].(string); got != "closed" {
		t.Errorf("body[state] = %q, want %q", got, "closed")
	}
}

// TestFilterDiffByFile_ExactMatch verifies the file filter matches the new-side
// path exactly and never via substring, so a request for "foo.go" can't
// accidentally capture a different file whose path merely contains it.
func TestFilterDiffByFile_ExactMatch(t *testing.T) {
	diff := "diff --git a/foo.go b/foo.go\n@@ -1 +1 @@\n-a\n+b\n" +
		"diff --git a/lib/b/foo.go b/lib/b/foo.go\n@@ -1 +1 @@\n-c\n+d\n"

	got := filterDiffByFile(diff, "foo.go")
	if !strings.Contains(got, "a/foo.go b/foo.go") || !strings.Contains(got, "+b") {
		t.Errorf("expected the foo.go section; got:\n%s", got)
	}
	if strings.Contains(got, "lib/b/foo.go") || strings.Contains(got, "+d") {
		t.Errorf("substring match leaked the lib/b/foo.go section; got:\n%s", got)
	}

	// The nested file is reachable by its exact path.
	nested := filterDiffByFile(diff, "lib/b/foo.go")
	if !strings.Contains(nested, "a/lib/b/foo.go b/lib/b/foo.go") || !strings.Contains(nested, "+d") {
		t.Errorf("expected the lib/b/foo.go section by exact path; got:\n%s", nested)
	}
	if strings.Contains(nested, "+b") {
		t.Errorf("nested lookup leaked the top-level foo.go section; got:\n%s", nested)
	}

	// A path not in the diff yields nothing.
	if out := filterDiffByFile(diff, "missing.go"); out != "" {
		t.Errorf("expected empty result for absent file; got:\n%s", out)
	}
}
