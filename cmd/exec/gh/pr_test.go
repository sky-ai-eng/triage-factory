package gh

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// jsonPRFiles encodes a slice of file maps as the response body GitHub would return
// for the PR files endpoint. Each map should have at minimum "filename" and "patch".
func jsonPRFiles(t *testing.T, files []map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(files)
	if err != nil {
		t.Fatalf("marshal PR files: %v", err)
	}
	return data
}

// for the three endpoints persistPRDiff touches: the PR JSON object
// (GET /pulls/N), the raw diff (same path, diff Accept header), and the file
// list (/pulls/N/files). Reviews/comments sub-fetches GetPR makes are stubbed
// with empty arrays so they don't error.
type prDiffBackend struct {
	prJSON      []byte
	diffBody    string
	diffStatus  int // 0 → 200
	filesBody   []byte
	filesStatus int // 0 → 200
}

func newPRDiffServer(t *testing.T, b prDiffBackend) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/files"):
			if b.filesStatus != 0 {
				w.WriteHeader(b.filesStatus)
			}
			_, _ = w.Write(b.filesBody)
		case strings.Contains(r.Header.Get("Accept"), "diff"):
			if b.diffStatus != 0 {
				w.WriteHeader(b.diffStatus)
			}
			_, _ = w.Write([]byte(b.diffBody))
		case strings.HasSuffix(r.URL.Path, "/reviews"), strings.HasSuffix(r.URL.Path, "/comments"):
			_, _ = w.Write([]byte(`[]`))
		default:
			_, _ = w.Write(b.prJSON)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeBaseSHA is a stand-in base.sha for the API-path tests, which don't have a
// local checkout and never resolve the base commit. Distinct from any head sha
// so it can't be confused with one. The local-checkout tests pass gitInit's real
// base SHA instead, so resolveDiffBase takes its primary (recorded-base) path.
const fakeBaseSHA = "ba5eba5eba5eba5eba5eba5eba5eba5eba5eba5e"

// prJSON builds the GET /pulls/N body with the fields persistPRDiff reads. base
// carries both ref and sha, mirroring a real GitHub PR object — the recorded
// base.sha is the diff base the worktree path frames against.
func prJSON(t *testing.T, sha, baseRef, baseSHA string, additions, deletions, changedFiles int) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"number":        42,
		"additions":     additions,
		"deletions":     deletions,
		"changed_files": changedFiles,
		"head":          map[string]any{"sha": sha, "ref": "feature"},
		"base":          map[string]any{"ref": baseRef, "sha": baseSHA},
	})
	if err != nil {
		t.Fatalf("marshal PR json: %v", err)
	}
	return data
}

func readManifestFromDisk(t *testing.T, dir string) diffManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest.json: %v", err)
	}
	var m diffManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal manifest.json: %v", err)
	}
	return m
}

// TestPersistPRDiff_WritesFullDiffAndManifest is the happy path: the diff is
// fetched verbatim, written to a per-PR dir, and the manifest reflects the
// PR metadata + per-file rows.
func TestPersistPRDiff_WritesFullDiffAndManifest(t *testing.T) {
	const sha = "abcdef0123456789abcdef"
	diff := "diff --git a/foo.go b/foo.go\n@@ -1,2 +1,2 @@\n context\n-old\n+new\n"
	files := jsonPRFiles(t, []map[string]any{
		{"filename": "foo.go", "status": "modified", "additions": 1, "deletions": 1, "patch": "@@ -1,2 +1,2 @@\n context\n-old\n+new\n"},
	})
	srv := newPRDiffServer(t, prDiffBackend{
		prJSON:    prJSON(t, sha, "main", fakeBaseSHA, 1, 1, 1),
		diffBody:  diff,
		filesBody: files,
	})

	cwd := t.TempDir()
	client := ghclient.NewClient(srv.URL, "test-token")
	m, err := persistPRDiff(context.Background(), client, localCheckout{}, cwd, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("persistPRDiff: %v", err)
	}

	wantDir := filepath.Join(cwd, "_tfac", "pr-diffs", "owner__repo__42")
	if m.Dir != wantDir {
		t.Errorf("Dir = %q, want %q", m.Dir, wantDir)
	}
	if m.HeadSHA != sha || m.BaseRef != "main" || m.ChangedFiles != 1 || m.Additions != 1 || m.Deletions != 1 {
		t.Errorf("manifest metadata mismatch: %+v", m)
	}
	if m.Truncated {
		t.Error("Truncated should be false on the verbatim path")
	}
	got, err := os.ReadFile(m.FullDiffPath)
	if err != nil {
		t.Fatalf("read full.diff: %v", err)
	}
	if string(got) != diff {
		t.Errorf("full.diff = %q, want %q", got, diff)
	}
	if len(m.Files) != 1 || m.Files[0].Path != "foo.go" || m.Files[0].Binary {
		t.Errorf("manifest files mismatch: %+v", m.Files)
	}
	// manifest.json on disk round-trips to the returned value.
	if disk := readManifestFromDisk(t, m.Dir); disk.HeadSHA != m.HeadSHA || len(disk.Files) != len(m.Files) {
		t.Errorf("on-disk manifest mismatch: %+v", disk)
	}
}

// TestPersistPRDiff_406Reassembles verifies the HTTP-406 fallback: the diff is
// reconstructed from per-file patches and flagged truncated.
func TestPersistPRDiff_406Reassembles(t *testing.T) {
	const sha = "feedface00112233"
	files := jsonPRFiles(t, []map[string]any{
		{"filename": "foo.go", "status": "added", "additions": 2, "deletions": 0, "patch": "@@ -0,0 +1,2 @@\n+line1\n+line2"},
	})
	srv := newPRDiffServer(t, prDiffBackend{
		prJSON:     prJSON(t, sha, "main", fakeBaseSHA, 2, 0, 1),
		diffBody:   `{"message":"diff too large"}`,
		diffStatus: http.StatusNotAcceptable,
		filesBody:  files,
	})

	cwd := t.TempDir()
	client := ghclient.NewClient(srv.URL, "test-token")
	m, err := persistPRDiff(context.Background(), client, localCheckout{}, cwd, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("persistPRDiff: %v", err)
	}
	if !m.Truncated {
		t.Error("Truncated should be true on the 406 fallback path")
	}
	got, err := os.ReadFile(m.FullDiffPath)
	if err != nil {
		t.Fatalf("read full.diff: %v", err)
	}
	for _, want := range []string{"diff --git a/foo.go b/foo.go", "--- /dev/null", "+++ b/foo.go", "+line1"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("reassembled diff missing %q; got:\n%s", want, got)
		}
	}
}

// TestPersistPRDiff_BinaryAndRename verifies binary detection and that a
// rename carries previous_filename without being flagged binary.
func TestPersistPRDiff_BinaryAndRename(t *testing.T) {
	const sha = "0011223344556677"
	files := jsonPRFiles(t, []map[string]any{
		{"filename": "image.png", "status": "added", "additions": 0, "deletions": 0},                               // no patch + zero counts → binary
		{"filename": "new.go", "status": "renamed", "previous_filename": "old.go", "additions": 0, "deletions": 0}, // no patch but a rename
		{"filename": "huge.go", "status": "modified", "additions": 5000, "deletions": 10},                          // no patch but real counts → oversized text, NOT binary
		{"filename": "main.go", "status": "modified", "additions": 1, "deletions": 0, "patch": "@@ -1 +1,2 @@\n a\n+b"},
	})
	srv := newPRDiffServer(t, prDiffBackend{
		prJSON:    prJSON(t, sha, "main", fakeBaseSHA, 1, 0, 4),
		diffBody:  "diff --git a/main.go b/main.go\n@@ -1 +1,2 @@\n a\n+b\n",
		filesBody: files,
	})

	cwd := t.TempDir()
	client := ghclient.NewClient(srv.URL, "test-token")
	m, err := persistPRDiff(context.Background(), client, localCheckout{}, cwd, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("persistPRDiff: %v", err)
	}
	byPath := map[string]fileSummary{}
	for _, f := range m.Files {
		byPath[f.Path] = f
	}
	if !byPath["image.png"].Binary {
		t.Error("image.png should be flagged binary")
	}
	if byPath["new.go"].Binary {
		t.Error("renamed file should NOT be flagged binary")
	}
	if byPath["new.go"].PreviousFilename != "old.go" {
		t.Errorf("rename previous_filename = %q, want old.go", byPath["new.go"].PreviousFilename)
	}
	if byPath["main.go"].Binary {
		t.Error("text file should not be binary")
	}
	if byPath["huge.go"].Binary {
		t.Error("oversized text file (empty patch but non-zero counts) should NOT be flagged binary")
	}
}

// TestPersistPRDiff_MissingHeadSHATolerated confirms a PR with no head SHA no
// longer hard-fails (the dir is keyed by PR number, not SHA): the capture is
// written and head_sha is simply empty in the manifest.
func TestPersistPRDiff_MissingHeadSHATolerated(t *testing.T) {
	srv := newPRDiffServer(t, prDiffBackend{
		prJSON:    prJSON(t, "", "main", fakeBaseSHA, 0, 0, 0),
		diffBody:  "diff --git a/foo.go b/foo.go\n@@ -1 +1,2 @@\n a\n+b\n",
		filesBody: jsonPRFiles(t, []map[string]any{{"filename": "foo.go", "status": "modified", "additions": 1, "deletions": 0, "patch": "@@ -1 +1,2 @@\n a\n+b"}}),
	})
	cwd := t.TempDir()
	client := ghclient.NewClient(srv.URL, "test-token")
	m, err := persistPRDiff(context.Background(), client, localCheckout{}, cwd, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("persistPRDiff with empty head SHA: %v", err)
	}
	if m.HeadSHA != "" {
		t.Errorf("HeadSHA = %q, want empty", m.HeadSHA)
	}
	if _, err := os.Stat(m.FullDiffPath); err != nil {
		t.Errorf("expected full.diff written: %v", err)
	}
}

// TestPersistPRDiff_ReDiff verifies the number-keying contract: re-diffing the
// same PR overwrites the capture in place (same dir, stale files gone,
// content/head_sha refreshed) — there is no per-SHA proliferation.
func TestPersistPRDiff_ReDiff(t *testing.T) {
	cwd := t.TempDir()
	files := jsonPRFiles(t, []map[string]any{
		{"filename": "foo.go", "status": "modified", "additions": 1, "deletions": 0, "patch": "@@ -1 +1,2 @@\n a\n+b"},
	})
	srv1 := newPRDiffServer(t, prDiffBackend{
		prJSON:    prJSON(t, "aaaaaaaaaaaa1111", "main", fakeBaseSHA, 1, 0, 1),
		diffBody:  "diff --git a/foo.go b/foo.go\n@@ -1 +1,2 @@\n a\n+b\n",
		filesBody: files,
	})
	m1, err := persistPRDiff(context.Background(), ghclient.NewClient(srv1.URL, "test-token"), localCheckout{}, cwd, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("first persistPRDiff: %v", err)
	}
	// Drop a stale file into the dir; the re-diff must clobber it.
	stale := filepath.Join(m1.Dir, "stale.txt")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatalf("write stale file: %v", err)
	}

	// Branch moved (new head SHA + new content). Same PR → same dir, overwritten.
	srv2 := newPRDiffServer(t, prDiffBackend{
		prJSON:    prJSON(t, "bbbbbbbbbbbb2222", "main", fakeBaseSHA, 1, 0, 1),
		diffBody:  "diff --git a/foo.go b/foo.go\n@@ -1 +1,2 @@\n a\n+c\n",
		filesBody: files,
	})
	m2, err := persistPRDiff(context.Background(), ghclient.NewClient(srv2.URL, "test-token"), localCheckout{}, cwd, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("re-diff: %v", err)
	}
	if m1.Dir != m2.Dir {
		t.Fatalf("same PR should map to the same dir; got %q then %q", m1.Dir, m2.Dir)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale file should have been clobbered on re-diff (stat err: %v)", err)
	}
	if m2.HeadSHA != "bbbbbbbbbbbb2222" {
		t.Errorf("manifest head_sha = %q, want the refreshed SHA", m2.HeadSHA)
	}
	got, err := os.ReadFile(m2.FullDiffPath)
	if err != nil {
		t.Fatalf("read full.diff: %v", err)
	}
	if !strings.Contains(string(got), "+c") {
		t.Errorf("full.diff should hold the refreshed content; got:\n%s", got)
	}
}

// TestPersistPRDiff_RejectsSymlinkedScratch confirms the shared symlink guard
// fires for the pr-diffs path too: a symlinked _tfac component is refused.
func TestPersistPRDiff_RejectsSymlinkedScratch(t *testing.T) {
	cwd := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(cwd, "_tfac")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	srv := newPRDiffServer(t, prDiffBackend{
		prJSON:    prJSON(t, "abcdef0123456789", "main", fakeBaseSHA, 1, 0, 1),
		diffBody:  "diff --git a/foo.go b/foo.go\n@@ -1 +1,2 @@\n a\n+b\n",
		filesBody: jsonPRFiles(t, []map[string]any{{"filename": "foo.go", "status": "modified", "patch": "@@ -1 +1,2 @@\n a\n+b"}}),
	})
	client := ghclient.NewClient(srv.URL, "test-token")
	_, err := persistPRDiff(context.Background(), client, localCheckout{}, cwd, "owner", "repo", 42)
	if err == nil {
		t.Fatal("expected symlink rejection, got nil")
	}
	if !strings.Contains(err.Error(), "symlinked path component") {
		t.Errorf("error should mention symlink, got: %v", err)
	}
}

// TestBuildPRFilesResult verifies the slim `pr files` envelope: no PR-level
// totals (sizing is pr view's job), the patch is never carried, binary/rename
// metadata is populated, and an empty file list yields an empty (non-null)
// files array.
func TestBuildPRFilesResult(t *testing.T) {
	files := []ghclient.PRFile{
		{Filename: "main.go", Status: "modified", Additions: 12, Deletions: 3, Patch: "@@ -1 +1,2 @@\n a\n+b"},
		{Filename: "image.png", Status: "added", Additions: 0, Deletions: 0},                                             // binary
		{Filename: "new.go", Status: "renamed", PreviousFilename: "old.go", Additions: 1, Deletions: 1, Patch: "@@..@@"}, // rename w/ edits
	}
	got := buildPRFilesResult(files)

	if got.Truncated {
		t.Error("a 3-file PR should not be flagged truncated")
	}
	byPath := map[string]fileSummary{}
	for _, f := range got.Files {
		byPath[f.Path] = f
	}
	if byPath["main.go"].Additions != 12 || byPath["main.go"].Deletions != 3 {
		t.Errorf("per-file counts wrong: %+v", byPath["main.go"])
	}
	if !byPath["image.png"].Binary {
		t.Error("image.png should be flagged binary")
	}
	if byPath["new.go"].PreviousFilename != "old.go" || byPath["new.go"].Binary {
		t.Errorf("rename row wrong: %+v", byPath["new.go"])
	}

	// No patch content, and no PR-level total field (changed_files), may
	// survive into the output. additions/deletions legitimately appear
	// per-file, so they're not banned — only the top-level rollup is.
	blob, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, banned := range []string{"@@", "\"patch\"", "changed_files"} {
		if strings.Contains(string(blob), banned) {
			t.Errorf("pr files output must not contain %q; got:\n%s", banned, blob)
		}
	}

	// Empty file list → empty, non-null array (so JSON is [] not null).
	empty := buildPRFilesResult(nil)
	if empty.Files == nil {
		t.Error("Files should be an empty slice, not nil")
	}
	if empty.Truncated {
		t.Error("empty file list should not be flagged truncated")
	}
}

// TestBuildPRFilesResult_Truncated checks the cap-detection flag: a listing at
// the GetPRFiles cap is flagged truncated so the agent falls back to pr diff.
func TestBuildPRFilesResult_Truncated(t *testing.T) {
	files := make([]ghclient.PRFile, ghclient.MaxPRFiles)
	for i := range files {
		files[i] = ghclient.PRFile{Filename: fmt.Sprintf("f%d.go", i), Status: "modified"}
	}
	if got := buildPRFilesResult(files); !got.Truncated {
		t.Errorf("a listing of MaxPRFiles (%d) should be flagged truncated", ghclient.MaxPRFiles)
	}
}

// TestStripClaudeCodeCitation pins the rules for trimming Claude
// Code's auto-citation off PR bodies before they hit the queue.
// The TF footer (added at submit time) is the prominent
// attribution; letting Claude Code's citation through would crowd
// it out and double-bill the PR.
func TestStripClaudeCodeCitation(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "typical Claude Code PR body",
			in:   "Summary of changes.\n\n## Test plan\n- run tests\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n",
			want: "Summary of changes.\n\n## Test plan\n- run tests",
		},
		{
			name: "no citation: passthrough",
			in:   "Summary of changes.\n\n## Test plan\n- run tests\n",
			want: "Summary of changes.\n\n## Test plan\n- run tests\n",
		},
		{
			name: "citation alone",
			in:   "🤖 Generated with [Claude Code](https://claude.com/claude-code)",
			want: "",
		},
		{
			name: "trailing whitespace before citation",
			in:   "Body.\n\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n\n\n",
			want: "Body.",
		},
		{
			name: "mid-body citation: leave alone",
			in:   "Citing 🤖 Generated with [Claude Code](https://claude.com/claude-code) in context.\n\nFinal sentence.",
			want: "Citing 🤖 Generated with [Claude Code](https://claude.com/claude-code) in context.\n\nFinal sentence.",
		},
		{
			name: "citation without leading emoji",
			in:   "Body.\n\nGenerated with [Claude Code](https://claude.com/claude-code)",
			want: "Body.",
		},
		{
			name: "empty body",
			in:   "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripClaudeCodeCitation(tc.in)
			if got != tc.want {
				t.Errorf("stripClaudeCodeCitation:\n--- got ---\n%q\n--- want ---\n%q", got, tc.want)
			}
		})
	}
}

// stubThreadGHAPI captures the owner/repo GetCommentThread is called with. It
// embeds ghAPI (nil) so it satisfies the interface; prThreadView only ever calls
// GetCommentThread, so no other method is reached.
type stubThreadGHAPI struct {
	ghAPI
	gotOwner, gotRepo string
}

func (s *stubThreadGHAPI) GetCommentThread(_ context.Context, owner, repo string, commentID, _ int) (*ghclient.CommentThread, error) {
	s.gotOwner, s.gotRepo = owner, repo
	return &ghclient.CommentThread{ID: commentID}, nil
}

// touchCapturingHost captures the RecordReadTouch call prThreadView makes. It
// embeds agenthost.Client (nil) so it satisfies the interface; only
// RecordReadTouch is exercised.
type touchCapturingHost struct {
	agenthost.Client
	calls                 int
	provider, target, url string
}

func (h *touchCapturingHost) RecordReadTouch(_ context.Context, provider, target, url string) {
	h.calls++
	h.provider, h.target, h.url = provider, target, url
}

// TestPrThreadView_HonorsRepoFlag pins that `gh pr thread-view` resolves the
// repo from an explicit --repo (not the cwd/env fallback) for BOTH the read and
// the touch it records — a regression guard on the args-slicing that used to
// hide the flag from resolveRepo.
func TestPrThreadView_HonorsRepoFlag(t *testing.T) {
	api := &stubThreadGHAPI{}
	host := &touchCapturingHost{}

	// <pr_number> <comment_id> --repo octo/repo — the flag trails the positionals.
	prThreadView(context.Background(), api, host, []string{"42", "999", "--repo", "octo/repo"})

	if api.gotOwner != "octo" || api.gotRepo != "repo" {
		t.Errorf("GetCommentThread saw %q/%q, want octo/repo (an explicit --repo must be honored)", api.gotOwner, api.gotRepo)
	}
	if host.calls != 1 {
		t.Fatalf("RecordReadTouch called %d time(s), want 1", host.calls)
	}
	if host.provider != domain.ArtifactProviderGitHub || host.target != "octo/repo#42" {
		t.Errorf("touch = (%q, %q), want (github, octo/repo#42)", host.provider, host.target)
	}
}
