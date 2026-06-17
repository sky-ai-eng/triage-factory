package gh

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// newTestServer builds an httptest server whose handler dispatches on path suffix.
// diffHandler is called for requests that look like the diff endpoint (no /files suffix),
// filesHandler is called for requests to /files.
func newTestServer(t *testing.T, diffHandler, filesHandler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/files") {
			filesHandler(w, r)
		} else {
			diffHandler(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

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

// TestGetDiffShapes_NormalDiff verifies the happy path: the diff endpoint returns
// a valid unified diff and getDiffShapes parses it into both a file→commentable-lines
// map and a file→hunks map, fetched in a single round-trip.
func TestGetDiffShapes_NormalDiff(t *testing.T) {
	diffContent := "diff --git a/foo.go b/foo.go\n@@ -1,2 +1,2 @@\n context\n-old\n+new\n"

	srv := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(diffContent))
		},
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("files endpoint should not be called on a successful diff fetch")
			http.Error(w, "unexpected call", http.StatusInternalServerError)
		},
	)

	client := ghclient.NewClient(srv.URL, "test-token")
	result, hunks, err := getDiffShapes(client, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("getDiffShapes: %v", err)
	}
	if _, ok := result["foo.go"]; !ok {
		t.Errorf("expected foo.go in result, got keys: %v", keys(result))
	}
	if !result["foo.go"][1] || !result["foo.go"][2] {
		t.Errorf("expected lines 1 and 2 commentable for foo.go, got %v", result["foo.go"])
	}
	// Hunks are derived from the same diff fetch — verify they're populated.
	if got, want := hunks["foo.go"], []ghclient.Hunk{{NewStart: 1, NewEnd: 2}}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("expected foo.go hunks %v, got %v", want, got)
	}
}

// TestGetDiffShapes_406FallsBackToFiles verifies that when the diff endpoint
// returns HTTP 406, getDiffShapes falls back to GetPRFiles + DiffLinesFromPatches /
// DiffHunksFromPatches.
func TestGetDiffShapes_406FallsBackToFiles(t *testing.T) {
	filesPayload := jsonPRFiles(t, []map[string]any{
		{
			"filename":  "a.go",
			"status":    "modified",
			"additions": 1,
			"deletions": 1,
			"patch":     "@@ -1,2 +1,2 @@\n context\n-old\n+new\n",
		},
		{
			"filename":  "b.go",
			"status":    "added",
			"additions": 2,
			"deletions": 0,
			"patch":     "@@ -0,0 +1,2 @@\n+line1\n+line2\n",
		},
	})

	srv := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"diff too large"}`, http.StatusNotAcceptable)
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(filesPayload)
		},
	)

	client := ghclient.NewClient(srv.URL, "test-token")
	result, hunks, err := getDiffShapes(client, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("getDiffShapes with 406 fallback: %v", err)
	}

	// a.go: context line 1, added line 2
	if _, ok := result["a.go"]; !ok {
		t.Errorf("expected a.go in fallback result, got: %v", keys(result))
	}
	// Hunks come from the same fallback — verify both files have them.
	if got := hunks["a.go"]; len(got) != 1 || got[0].NewStart != 1 || got[0].NewEnd != 2 {
		t.Errorf("expected a.go hunk [1-2], got %v", got)
	}
	if got := hunks["b.go"]; len(got) != 1 || got[0].NewStart != 1 || got[0].NewEnd != 2 {
		t.Errorf("expected b.go hunk [1-2], got %v", got)
	}
	if !result["a.go"][1] || !result["a.go"][2] {
		t.Errorf("expected lines 1,2 commentable for a.go, got %v", result["a.go"])
	}

	// b.go: two added lines
	if _, ok := result["b.go"]; !ok {
		t.Errorf("expected b.go in fallback result, got: %v", keys(result))
	}
	if !result["b.go"][1] || !result["b.go"][2] {
		t.Errorf("expected lines 1,2 commentable for b.go, got %v", result["b.go"])
	}
}

// TestGetDiffShapes_406EmptyFileList verifies the fallback works even when the
// files endpoint returns an empty list (e.g., all files are binary-only).
func TestGetDiffShapes_406EmptyFileList(t *testing.T) {
	srv := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"diff too large"}`, http.StatusNotAcceptable)
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		},
	)

	client := ghclient.NewClient(srv.URL, "test-token")
	result, hunks, err := getDiffShapes(client, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("getDiffShapes with 406 + empty files: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty result for empty file list, got: %v", result)
	}
	if len(hunks) != 0 {
		t.Errorf("expected empty hunks for empty file list, got: %v", hunks)
	}
}

// TestGetDiffShapes_406FilesEndpointFails verifies that when the diff endpoint
// returns 406 AND the files fallback also fails, the files error is returned.
func TestGetDiffShapes_406FilesEndpointFails(t *testing.T) {
	srv := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"diff too large"}`, http.StatusNotAcceptable)
		},
		func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"internal server error"}`, http.StatusInternalServerError)
		},
	)

	client := ghclient.NewClient(srv.URL, "test-token")
	_, _, err := getDiffShapes(client, "owner", "repo", 42)
	if err == nil {
		t.Fatal("expected error when 406 and files endpoint also fails, got nil")
	}
}

// TestGetDiffShapes_OtherErrorPropagates verifies that non-406 errors from the
// diff endpoint are NOT silently swallowed — the fallback must NOT be triggered.
func TestGetDiffShapes_OtherErrorPropagates(t *testing.T) {
	filesCallCount := 0
	srv := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		},
		func(w http.ResponseWriter, r *http.Request) {
			filesCallCount++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		},
	)

	client := ghclient.NewClient(srv.URL, "test-token")
	_, _, err := getDiffShapes(client, "owner", "repo", 42)
	if err == nil {
		t.Fatal("expected error on 404 from diff endpoint, got nil")
	}
	if filesCallCount > 0 {
		t.Errorf("files endpoint should NOT be called for non-406 errors, got %d calls", filesCallCount)
	}
}

// TestGetDiffShapes_406BinaryFile verifies that a 406 fallback handles PRs that
// include binary files (missing patch field) without crashing and produces
// an empty line set for those files.
func TestGetDiffShapes_406BinaryFile(t *testing.T) {
	filesPayload := jsonPRFiles(t, []map[string]any{
		{
			"filename":  "image.png",
			"status":    "added",
			"additions": 0,
			"deletions": 0,
			// no "patch" field — binary file
		},
		{
			"filename":  "main.go",
			"status":    "modified",
			"additions": 1,
			"deletions": 0,
			"patch":     "@@ -1,1 +1,2 @@\n line1\n+line2\n",
		},
	})

	srv := newTestServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"diff too large"}`, http.StatusNotAcceptable)
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(filesPayload)
		},
	)

	client := ghclient.NewClient(srv.URL, "test-token")
	result, hunks, err := getDiffShapes(client, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("getDiffShapes binary file: %v", err)
	}

	// binary file should be present but have no commentable lines
	if _, ok := result["image.png"]; !ok {
		t.Error("expected image.png in result")
	}
	if len(result["image.png"]) != 0 {
		t.Errorf("expected no commentable lines for binary file, got %v", result["image.png"])
	}
	if len(hunks["image.png"]) != 0 {
		t.Errorf("expected no hunks for binary file, got %v", hunks["image.png"])
	}

	// text file should have correct commentable lines
	if !result["main.go"][1] || !result["main.go"][2] {
		t.Errorf("expected lines 1,2 commentable for main.go, got %v", result["main.go"])
	}
}

// prDiffBackend configures the fake GitHub responses newPRDiffServer serves
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

// prJSON builds the GET /pulls/N body with the fields persistPRDiff reads.
func prJSON(t *testing.T, sha, baseRef string, additions, deletions, changedFiles int) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"number":        42,
		"additions":     additions,
		"deletions":     deletions,
		"changed_files": changedFiles,
		"head":          map[string]any{"sha": sha, "ref": "feature"},
		"base":          map[string]any{"ref": baseRef},
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
// fetched verbatim, written to a SHA-keyed dir, and the manifest reflects the
// PR metadata + per-file rows.
func TestPersistPRDiff_WritesFullDiffAndManifest(t *testing.T) {
	const sha = "abcdef0123456789abcdef"
	diff := "diff --git a/foo.go b/foo.go\n@@ -1,2 +1,2 @@\n context\n-old\n+new\n"
	files := jsonPRFiles(t, []map[string]any{
		{"filename": "foo.go", "status": "modified", "additions": 1, "deletions": 1, "patch": "@@ -1,2 +1,2 @@\n context\n-old\n+new\n"},
	})
	srv := newPRDiffServer(t, prDiffBackend{
		prJSON:    prJSON(t, sha, "main", 1, 1, 1),
		diffBody:  diff,
		filesBody: files,
	})

	cwd := t.TempDir()
	client := ghclient.NewClient(srv.URL, "test-token")
	m, err := persistPRDiff(client, cwd, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("persistPRDiff: %v", err)
	}

	wantDir := filepath.Join(cwd, "_scratch", "pr-diffs", "owner__repo__42", sha[:12])
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
		prJSON:     prJSON(t, sha, "main", 2, 0, 1),
		diffBody:   `{"message":"diff too large"}`,
		diffStatus: http.StatusNotAcceptable,
		filesBody:  files,
	})

	cwd := t.TempDir()
	client := ghclient.NewClient(srv.URL, "test-token")
	m, err := persistPRDiff(client, cwd, "owner", "repo", 42)
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
		{"filename": "image.png", "status": "added", "additions": 0, "deletions": 0},                               // no patch → binary
		{"filename": "new.go", "status": "renamed", "previous_filename": "old.go", "additions": 0, "deletions": 0}, // no patch but a rename
		{"filename": "main.go", "status": "modified", "additions": 1, "deletions": 0, "patch": "@@ -1 +1,2 @@\n a\n+b"},
	})
	srv := newPRDiffServer(t, prDiffBackend{
		prJSON:    prJSON(t, sha, "main", 1, 0, 3),
		diffBody:  "diff --git a/main.go b/main.go\n@@ -1 +1,2 @@\n a\n+b\n",
		filesBody: files,
	})

	cwd := t.TempDir()
	client := ghclient.NewClient(srv.URL, "test-token")
	m, err := persistPRDiff(client, cwd, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("persistPRDiff: %v", err)
	}
	byPath := map[string]diffManifestFile{}
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
}

// TestPersistPRDiff_NoHeadSHAFails guards the hard-fail when the PR has no head
// SHA — the diff can't be keyed to a commit.
func TestPersistPRDiff_NoHeadSHAFails(t *testing.T) {
	srv := newPRDiffServer(t, prDiffBackend{
		prJSON:    prJSON(t, "", "main", 0, 0, 0),
		diffBody:  "",
		filesBody: []byte(`[]`),
	})
	cwd := t.TempDir()
	client := ghclient.NewClient(srv.URL, "test-token")
	if _, err := persistPRDiff(client, cwd, "owner", "repo", 42); err == nil {
		t.Fatal("expected error on missing head SHA, got nil")
	}
}

// TestPersistPRDiff_ReDiff verifies the SHA-keying contract: re-diffing the
// same SHA clobbers that dir (idempotent), while a moved branch (new SHA)
// writes a sibling dir and leaves the prior capture intact.
func TestPersistPRDiff_ReDiff(t *testing.T) {
	cwd := t.TempDir()
	const sha1 = "aaaaaaaaaaaa1111"
	files := jsonPRFiles(t, []map[string]any{
		{"filename": "foo.go", "status": "modified", "additions": 1, "deletions": 0, "patch": "@@ -1 +1,2 @@\n a\n+b"},
	})
	srv1 := newPRDiffServer(t, prDiffBackend{
		prJSON:    prJSON(t, sha1, "main", 1, 0, 1),
		diffBody:  "diff --git a/foo.go b/foo.go\n@@ -1 +1,2 @@\n a\n+b\n",
		filesBody: files,
	})
	client1 := ghclient.NewClient(srv1.URL, "test-token")

	m1, err := persistPRDiff(client1, cwd, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("first persistPRDiff: %v", err)
	}
	// Drop a stale file into the SHA dir; a re-diff of the same SHA must clobber it.
	stale := filepath.Join(m1.Dir, "stale.txt")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatalf("write stale file: %v", err)
	}
	if _, err := persistPRDiff(client1, cwd, "owner", "repo", 42); err != nil {
		t.Fatalf("re-diff same SHA: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale file should have been clobbered on same-SHA re-diff (stat err: %v)", err)
	}

	// Branch moves: a new SHA writes a sibling dir, leaving m1.Dir intact.
	const sha2 = "bbbbbbbbbbbb2222"
	srv2 := newPRDiffServer(t, prDiffBackend{
		prJSON:    prJSON(t, sha2, "main", 1, 0, 1),
		diffBody:  "diff --git a/foo.go b/foo.go\n@@ -1 +1,2 @@\n a\n+c\n",
		filesBody: files,
	})
	client2 := ghclient.NewClient(srv2.URL, "test-token")
	m2, err := persistPRDiff(client2, cwd, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("second-SHA persistPRDiff: %v", err)
	}
	if m1.Dir == m2.Dir {
		t.Fatalf("different SHAs should map to different dirs, both = %q", m1.Dir)
	}
	for _, d := range []string{m1.Dir, m2.Dir} {
		if _, err := os.Stat(filepath.Join(d, "full.diff")); err != nil {
			t.Errorf("expected full.diff under %q: %v", d, err)
		}
	}
}

// TestPersistPRDiff_RejectsSymlinkedScratch confirms the shared symlink guard
// fires for the pr-diffs path too: a symlinked _scratch component is refused.
func TestPersistPRDiff_RejectsSymlinkedScratch(t *testing.T) {
	cwd := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(cwd, "_scratch")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	srv := newPRDiffServer(t, prDiffBackend{
		prJSON:    prJSON(t, "abcdef0123456789", "main", 1, 0, 1),
		diffBody:  "diff --git a/foo.go b/foo.go\n@@ -1 +1,2 @@\n a\n+b\n",
		filesBody: jsonPRFiles(t, []map[string]any{{"filename": "foo.go", "status": "modified", "patch": "@@ -1 +1,2 @@\n a\n+b"}}),
	})
	client := ghclient.NewClient(srv.URL, "test-token")
	_, err := persistPRDiff(client, cwd, "owner", "repo", 42)
	if err == nil {
		t.Fatal("expected symlink rejection, got nil")
	}
	if !strings.Contains(err.Error(), "symlinked path component") {
		t.Errorf("error should mention symlink, got: %v", err)
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
