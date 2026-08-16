package gh

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/github"
)

// buildZip creates an in-memory zip archive from a map of path → contents.
// Used to generate fixtures for the extraction tests without committing
// binary files. Entries ending in "/" are created as directories.
func buildZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range entries {
		header := &zip.FileHeader{
			Name:   name,
			Method: zip.Deflate,
		}
		if strings.HasSuffix(name, "/") {
			header.SetMode(0755 | os.ModeDir)
		} else {
			header.SetMode(0644)
		}
		f, err := w.CreateHeader(header)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if !strings.HasSuffix(name, "/") {
			if _, err := f.Write([]byte(content)); err != nil {
				t.Fatalf("zip write %q: %v", name, err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// writeZipFile writes bytes to a temp .zip file and returns its path.
// Cleanup is registered with t.
func writeZipFile(t *testing.T, data []byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "fixture-*.zip")
	if err != nil {
		t.Fatalf("create temp zip: %v", err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		t.Fatalf("write temp zip: %v", err)
	}
	return f.Name()
}

func TestExtractZip_SafeArchive(t *testing.T) {
	data := buildZip(t, map[string]string{
		"build (ubuntu-latest)/":               "",
		"build (ubuntu-latest)/1_Checkout.txt": "checkout log content\n",
		"build (ubuntu-latest)/2_Build.txt":    "build log content\n",
		"test/":                                "",
		"test/1_Run.txt":                       "test log content\n",
	})
	zipPath := writeZipFile(t, data)

	destDir := t.TempDir()
	if err := extractZip(zipPath, destDir, maxPerFileBytes, maxTotalUncompressedBytes); err != nil {
		t.Fatalf("extractZip: %v", err)
	}

	// Verify every expected file is present with correct content.
	wantFiles := map[string]string{
		"build (ubuntu-latest)/1_Checkout.txt": "checkout log content\n",
		"build (ubuntu-latest)/2_Build.txt":    "build log content\n",
		"test/1_Run.txt":                       "test log content\n",
	}
	for rel, wantContent := range wantFiles {
		got, err := os.ReadFile(filepath.Join(destDir, rel))
		if err != nil {
			t.Errorf("read %q: %v", rel, err)
			continue
		}
		if string(got) != wantContent {
			t.Errorf("%q content = %q, want %q", rel, string(got), wantContent)
		}
	}
}

// TestExtractZip_PathTraversalRejected is the most important test in this
// file. A zip entry with a name containing "../" is a real attack vector —
// an extractor that naively joins the entry name onto the destination dir
// will write files wherever the attacker specifies. This must fail with a
// clear error and refuse to write anything outside destDir.
func TestExtractZip_PathTraversalRejected(t *testing.T) {
	cases := []struct {
		name    string
		entries map[string]string
	}{
		{
			"parent escape via ../",
			map[string]string{"../pwned.txt": "gotcha"},
		},
		{
			"multi-level parent escape",
			map[string]string{"../../../etc/passwd": "gotcha"},
		},
		{
			"nested legit path with escape",
			map[string]string{
				"legit/file.txt":         "ok",
				"legit/../../escape.txt": "gotcha",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := buildZip(t, tc.entries)
			zipPath := writeZipFile(t, data)

			destDir := t.TempDir()
			err := extractZip(zipPath, destDir, maxPerFileBytes, maxTotalUncompressedBytes)
			if err == nil {
				t.Fatal("expected extraction to fail on path-traversal entry, got nil error")
			}
			if !strings.Contains(err.Error(), "unsafe archive entry") {
				t.Errorf("error message should mention unsafe entry, got: %v", err)
			}

			// Verify no file escaped the destination. The parent directory
			// (which is the tempdir root) must not contain any file that
			// only the zip would have put there.
			parent := filepath.Dir(destDir)
			if _, err := os.Stat(filepath.Join(parent, "pwned.txt")); err == nil {
				t.Error("path-traversal file escaped destination")
			}
			if _, err := os.Stat(filepath.Join(parent, "escape.txt")); err == nil {
				t.Error("path-traversal file escaped destination")
			}
		})
	}
}

// TestExtractZip_RejectsNonPositiveCap is the regression guard for a subtle
// bug: the extraction path had a conditional header check (`maxFileBytes >= 0`)
// but an unconditional runtime LimitReader check, which meant that for any
// negative cap the header check silently treated it as "no cap" while the
// runtime check turned into "reject everything" (io.LimitReader with a
// negative limit reads 0 bytes, and n > negativeValue is always true).
// The fix is a precondition: maxFileBytes must be positive, and callers
// that want effectively unlimited should pass math.MaxInt64 explicitly
// instead of a sentinel.
func TestExtractZip_RejectsNonPositiveCap(t *testing.T) {
	data := buildZip(t, map[string]string{"any.txt": "content"})
	zipPath := writeZipFile(t, data)
	destDir := t.TempDir()

	// maxFileBytes non-positive — precondition failure
	for _, cap := range []int64{-1, 0} {
		err := extractZip(zipPath, destDir, cap, maxTotalUncompressedBytes)
		if err == nil {
			t.Errorf("maxFileBytes=%d: expected precondition error, got nil", cap)
			continue
		}
		if !strings.Contains(err.Error(), "maxFileBytes must be positive") {
			t.Errorf("maxFileBytes=%d: error should mention precondition, got: %v", cap, err)
		}
	}

	// maxTotalBytes non-positive — also precondition failure
	for _, cap := range []int64{-1, 0} {
		err := extractZip(zipPath, destDir, maxPerFileBytes, cap)
		if err == nil {
			t.Errorf("maxTotalBytes=%d: expected precondition error, got nil", cap)
			continue
		}
		if !strings.Contains(err.Error(), "maxTotalBytes must be positive") {
			t.Errorf("maxTotalBytes=%d: error should mention precondition, got: %v", cap, err)
		}
	}
}

// TestExtractZip_PerFileSizeCapRejected verifies the per-entry size guard
// fires when real content exceeds the cap. Uses a small cap (1 KB) against
// a 2 KB payload so the test stays cheap — the guard is parameterized so
// production uses maxPerFileBytes while tests can pass anything.
//
// We exercise the runtime io.LimitReader path here, not the header pre-check:
// Go's zip.NewWriter overwrites FileHeader.UncompressedSize64 with the real
// size when writing, so you can't fake an oversized header via the standard
// writer API. The header check exists to fast-reject hand-crafted adversarial
// zips; the runtime check is what catches everything else, and it's the one
// that matters for real zip inputs.
func TestExtractZip_PerFileSizeCapRejected(t *testing.T) {
	const testCap int64 = 1024 // 1 KB
	oversized := strings.Repeat("A", int(testCap)*2)

	data := buildZip(t, map[string]string{
		"huge.log": oversized,
	})
	zipPath := writeZipFile(t, data)
	destDir := t.TempDir()

	err := extractZip(zipPath, destDir, testCap, maxTotalUncompressedBytes)
	if err == nil {
		t.Fatal("expected extraction to fail on oversized entry, got nil error")
	}
	if !strings.Contains(err.Error(), "per-file size cap") {
		t.Errorf("error should mention size cap, got: %v", err)
	}
}

// TestExtractZip_TotalUncompressedCapRejected verifies the cumulative-
// bytes guard: a zip archive whose individual entries each fit under the
// per-file cap but whose total uncompressed size exceeds the total cap
// must be rejected. This is the layer that catches highly-compressible
// many-entry archives (the classic "zip bomb" shape) that slip past the
// compressed-download cap AND the per-file cap.
//
// Uses a small totalCap (5 KB) against 10 entries of 1 KB each (10 KB
// total) so the test stays cheap. The per-file cap is set loose (10 KB)
// so this test exercises the total cap specifically, not the per-file
// cap.
func TestExtractZip_TotalUncompressedCapRejected(t *testing.T) {
	const (
		perFileCap int64 = 10 * 1024 // 10 KB per entry
		totalCap   int64 = 5 * 1024  // 5 KB total — less than 10 × 1 KB
	)

	entries := map[string]string{}
	chunk := strings.Repeat("A", 1024) // 1 KB of content per entry
	for i := 0; i < 10; i++ {
		entries[fmt.Sprintf("log-%02d.txt", i)] = chunk
	}

	data := buildZip(t, entries)
	zipPath := writeZipFile(t, data)
	destDir := t.TempDir()

	err := extractZip(zipPath, destDir, perFileCap, totalCap)
	if err == nil {
		t.Fatal("expected extraction to fail on total-cap exceed, got nil error")
	}
	if !strings.Contains(err.Error(), "total uncompressed size") {
		t.Errorf("error should mention total uncompressed, got: %v", err)
	}
}

func TestTopLevelEntries_SortedAndAnnotated(t *testing.T) {
	dir := t.TempDir()

	// Mix of files and directories, created in non-alphabetical order
	// so the sort guarantee is actually tested.
	mustMkdir(t, filepath.Join(dir, "zebra"))
	mustMkdir(t, filepath.Join(dir, "alpha"))
	mustWrite(t, filepath.Join(dir, "readme.txt"), "hi")
	mustMkdir(t, filepath.Join(dir, "mid"))

	got, err := topLevelEntries(dir)
	if err != nil {
		t.Fatalf("topLevelEntries: %v", err)
	}
	want := []string{"alpha/", "mid/", "readme.txt", "zebra/"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTopLevelEntries_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	got, err := topLevelEntries(dir)
	if err != nil {
		t.Fatalf("topLevelEntries: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

// TestSafeDestDirForRun_HappyPath verifies the common case where none of
// the path components exist yet — we just construct the path, no safety
// rejections needed.
func TestSafeDestDirForRun_HappyPath(t *testing.T) {
	cwd := t.TempDir()
	dest, err := safeDestDirForRun(cwd, 123)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(cwd, "_tfac", "ci-logs", "123")
	if dest != want {
		t.Errorf("dest = %q, want %q", dest, want)
	}
}

// TestSafeDestDirForRun_AllowsRealDirectories confirms that pre-existing
// real directories on the path are fine — only symlinks are rejected.
// This covers the "run #2 on the same cwd" case where _tfac and
// _tfac/ci-logs already exist from a previous run.
func TestSafeDestDirForRun_AllowsRealDirectories(t *testing.T) {
	cwd := t.TempDir()
	mustMkdir(t, filepath.Join(cwd, "_tfac", "ci-logs"))

	dest, err := safeDestDirForRun(cwd, 456)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(cwd, "_tfac", "ci-logs", "456")
	if dest != want {
		t.Errorf("dest = %q, want %q", dest, want)
	}
}

// TestSafeDestDirForRun_RejectsSymlinkedComponent is the regression guard
// for the symlink-escape bug: if any component under cwd (including the
// run_id leaf itself) exists as a symlink, we must refuse to proceed.
// Otherwise downloadAndExtractLogs would RemoveAll / MkdirAll / extract
// through the symlink and touch paths outside the working directory.
func TestSafeDestDirForRun_RejectsSymlinkedComponent(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, cwd string)
	}{
		{
			"_tfac is a symlink to somewhere outside",
			func(t *testing.T, cwd string) {
				outside := t.TempDir()
				if err := os.Symlink(outside, filepath.Join(cwd, "_tfac")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
		},
		{
			"_tfac/ci-logs is a symlink",
			func(t *testing.T, cwd string) {
				outside := t.TempDir()
				mustMkdir(t, filepath.Join(cwd, "_tfac"))
				if err := os.Symlink(outside, filepath.Join(cwd, "_tfac", "ci-logs")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
		},
		{
			"run_id leaf is a symlink",
			func(t *testing.T, cwd string) {
				outside := t.TempDir()
				mustMkdir(t, filepath.Join(cwd, "_tfac", "ci-logs"))
				if err := os.Symlink(outside, filepath.Join(cwd, "_tfac", "ci-logs", "789")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			tc.setup(t, cwd)

			_, err := safeDestDirForRun(cwd, 789)
			if err == nil {
				t.Fatal("expected symlink rejection, got nil error")
			}
			if !strings.Contains(err.Error(), "symlinked path component") {
				t.Errorf("error should mention symlink, got: %v", err)
			}
		})
	}
}

// TestSafeDestDirForRun_RejectsNonDirectoryComponent catches the case
// where a path component exists but as a regular file (not a symlink,
// not a directory). MkdirAll would fail on the real operation; we fail
// earlier with a clearer message.
func TestSafeDestDirForRun_RejectsNonDirectoryComponent(t *testing.T) {
	cwd := t.TempDir()
	// Create _tfac as a file instead of a directory
	if err := os.WriteFile(filepath.Join(cwd, "_tfac"), []byte("nope"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := safeDestDirForRun(cwd, 123)
	if err == nil {
		t.Fatal("expected error on non-directory component, got nil")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error should mention non-directory, got: %v", err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("mkdir %q: %v", path, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

// countCILogTempFiles counts currently-orphaned temp zip files matching the
// pattern we write in downloadAndExtractLogs. Used by the transactional
// tests below to assert that no temp file is leaked on either the happy or
// the failure path. The count is taken as a delta (before vs after) so
// concurrent tests in the same package don't pollute the assertion.
func countCILogTempFiles(t *testing.T) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "triagefactory-ci-logs-*.zip"))
	if err != nil {
		t.Fatalf("glob temp files: %v", err)
	}
	return len(matches)
}

// TestDownloadAndExtractLogs_HappyPath verifies the success path: the
// destination directory is populated with extracted content and the temp
// zip file is cleaned up. Guards against a regression where the refactor
// to the inner function accidentally drops a cleanup defer.
func TestDownloadAndExtractLogs_HappyPath(t *testing.T) {
	zipBytes := buildZip(t, map[string]string{
		"job1/step1.txt": "log content\n",
		"job2/step1.txt": "other log\n",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipBytes)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(zipBytes)
	}))
	t.Cleanup(srv.Close)

	client := github.NewClient(srv.URL, "test-token")
	destDir := filepath.Join(t.TempDir(), "_tfac", "ci-logs", "123")

	before := countCILogTempFiles(t)
	n, err := downloadAndExtractLogs(context.Background(), client, "owner", "repo", 123, destDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != int64(len(zipBytes)) {
		t.Errorf("bytes = %d, want %d", n, len(zipBytes))
	}

	// Extracted content is present under destDir
	if content, err := os.ReadFile(filepath.Join(destDir, "job1", "step1.txt")); err != nil {
		t.Errorf("extracted file missing: %v", err)
	} else if string(content) != "log content\n" {
		t.Errorf("extracted content = %q, want %q", string(content), "log content\n")
	}

	// No leaked temp zip
	after := countCILogTempFiles(t)
	if after != before {
		t.Errorf("temp zip files leaked on success: before=%d after=%d", before, after)
	}
}

// TestDownloadAndExtractLogs_FailureCleansUp is the regression test for the
// exit-defer-cleanup bug: when the download step fails, both the temp zip
// file AND the destination directory must be cleaned up. A previous version
// used exitErr (→ os.Exit) inline, which skipped defers and leaked both.
func TestDownloadAndExtractLogs_FailureCleansUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	client := github.NewClient(srv.URL, "test-token")
	destDir := filepath.Join(t.TempDir(), "_tfac", "ci-logs", "123")

	before := countCILogTempFiles(t)
	_, err := downloadAndExtractLogs(context.Background(), client, "owner", "repo", 123, destDir)
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}

	// destDir must have been rolled back — no stale partial extraction left
	// behind to confuse a retry.
	if _, statErr := os.Stat(destDir); !os.IsNotExist(statErr) {
		t.Errorf("expected destDir to be removed on failure, but stat returned: %v", statErr)
	}

	// Temp zip must have been cleaned up.
	after := countCILogTempFiles(t)
	if after != before {
		t.Errorf("temp zip files leaked on failure: before=%d after=%d", before, after)
	}
}

// TestDownloadAndExtractLogs_ClobberStaleRunDir verifies that re-running
// download-logs for the same run_id does NOT leave behind files from a
// previous extraction. The command owns <cwd>/_tfac/ci-logs/<run_id>
// completely, so any stale entries — an old job directory that no longer
// exists in the current workflow run, a renamed matrix leg — have to be
// cleared before the fresh extract. Otherwise the agent reading back the
// destination can't tell which files belong to this run.
func TestDownloadAndExtractLogs_ClobberStaleRunDir(t *testing.T) {
	zipBytes := buildZip(t, map[string]string{
		"current-job/step1.txt": "fresh content\n",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(zipBytes)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(zipBytes)
	}))
	t.Cleanup(srv.Close)

	client := github.NewClient(srv.URL, "test-token")
	destDir := filepath.Join(t.TempDir(), "_tfac", "ci-logs", "123")

	// Pre-populate destDir with stale content from an imaginary previous
	// run: an old job directory and a stale top-level summary file that
	// the new archive does not include.
	mustMkdir(t, filepath.Join(destDir, "removed-job"))
	mustWrite(t, filepath.Join(destDir, "removed-job", "old-step.txt"), "stale from previous run\n")
	mustWrite(t, filepath.Join(destDir, "0_old-summary.txt"), "stale summary\n")

	if _, err := downloadAndExtractLogs(context.Background(), client, "owner", "repo", 123, destDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Fresh content must be present
	if content, err := os.ReadFile(filepath.Join(destDir, "current-job", "step1.txt")); err != nil {
		t.Errorf("fresh file missing: %v", err)
	} else if string(content) != "fresh content\n" {
		t.Errorf("fresh content = %q, want %q", string(content), "fresh content\n")
	}

	// Stale content must be gone — not merged, not left alongside.
	if _, err := os.Stat(filepath.Join(destDir, "removed-job")); !os.IsNotExist(err) {
		t.Errorf("stale directory %q should have been removed, stat err: %v", "removed-job", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "0_old-summary.txt")); !os.IsNotExist(err) {
		t.Errorf("stale file %q should have been removed, stat err: %v", "0_old-summary.txt", err)
	}
}

// TestDownloadAndExtractLogs_ExtractFailureCleansUp covers the other
// failure boundary: download succeeds, extraction fails (invalid zip).
// Both resources must still be cleaned up because the success flag never
// flips.
func TestDownloadAndExtractLogs_ExtractFailureCleansUp(t *testing.T) {
	// Serve garbage that isn't a valid zip. Downloads fine, zip.OpenReader
	// rejects it.
	garbage := []byte("this is definitely not a zip file, it is just some text")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(garbage)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(garbage)
	}))
	t.Cleanup(srv.Close)

	client := github.NewClient(srv.URL, "test-token")
	destDir := filepath.Join(t.TempDir(), "_tfac", "ci-logs", "123")

	before := countCILogTempFiles(t)
	_, err := downloadAndExtractLogs(context.Background(), client, "owner", "repo", 123, destDir)
	if err == nil {
		t.Fatal("expected extraction failure on invalid zip, got nil")
	}
	if !strings.Contains(err.Error(), "extract archive") {
		t.Errorf("error should mention extraction, got: %v", err)
	}

	if _, statErr := os.Stat(destDir); !os.IsNotExist(statErr) {
		t.Errorf("expected destDir to be removed on extract failure, but stat returned: %v", statErr)
	}
	after := countCILogTempFiles(t)
	if after != before {
		t.Errorf("temp zip files leaked on extract failure: before=%d after=%d", before, after)
	}
}

// --- list-runs tests -------------------------------------------------------

// fetchRunsForSHA is the testable core of actionsListRuns — the outer handler
// calls os.Exit on error, so we exercise the API-facing path here and trust
// the glue (flag parsing, PR→SHA resolution) by inspection.
func TestFetchRunsForSHA_ShapesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/repos/owner/repo/actions/runs" {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("head_sha"); got != "abc123" {
			t.Errorf("expected head_sha=abc123, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"total_count": 2,
			"workflow_runs": [
				{"id": 100, "name": "test", "event": "pull_request", "status": "completed", "conclusion": "failure", "head_sha": "abc123", "head_branch": "feat/x", "created_at": "2026-04-20T00:00:00Z", "html_url": "https://github.com/owner/repo/actions/runs/100"},
				{"id": 101, "name": "lint", "event": "pull_request", "status": "completed", "conclusion": "success", "head_sha": "abc123", "head_branch": "feat/x", "created_at": "2026-04-20T00:01:00Z", "html_url": "https://github.com/owner/repo/actions/runs/101"}
			]
		}`))
	}))
	t.Cleanup(srv.Close)

	client := github.NewClient(srv.URL, "test-token")

	runs, err := fetchRunsForSHA(context.Background(), client, "owner", "repo", "abc123")
	if err != nil {
		t.Fatalf("fetchRunsForSHA: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
	if runs[0].RunID != 100 || runs[0].WorkflowName != "test" || runs[0].Conclusion != "failure" {
		t.Errorf("first run shape wrong: %+v", runs[0])
	}
	if runs[1].RunID != 101 || runs[1].Conclusion != "success" {
		t.Errorf("second run shape wrong: %+v", runs[1])
	}
}

func TestFetchRunsForSHA_EmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_count": 0, "workflow_runs": []}`))
	}))
	t.Cleanup(srv.Close)

	client := github.NewClient(srv.URL, "test-token")
	runs, err := fetchRunsForSHA(context.Background(), client, "owner", "repo", "deadbeef")
	if err != nil {
		t.Fatalf("fetchRunsForSHA: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("expected empty runs, got %d", len(runs))
	}
}

func TestFetchRunsForSHA_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	client := github.NewClient(srv.URL, "test-token")
	_, err := fetchRunsForSHA(context.Background(), client, "owner", "repo", "abc123")
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
}

func TestFlagVal(t *testing.T) {
	cases := []struct {
		name string
		args []string
		flag string
		want string
	}{
		{"present", []string{"--pr", "42"}, "--pr", "42"},
		{"with-others", []string{"--repo", "o/r", "--pr", "42"}, "--pr", "42"},
		{"absent", []string{"--repo", "o/r"}, "--pr", ""},
		{"empty-args", []string{}, "--pr", ""},
		{"flag-is-last-with-no-value", []string{"--pr"}, "--pr", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := flagVal(tc.args, tc.flag)
			if got != tc.want {
				t.Errorf("flagVal(%v, %q) = %q, want %q", tc.args, tc.flag, got, tc.want)
			}
		})
	}
}

// --- per-job fallback tests -----------------------------------------------

// TestFetchJobsForRun_ShapesResponse exercises the /actions/runs/{id}/jobs
// fetch — the read side of the per-job fallback. Verifies field mapping and
// the per_page=100 cap landing on the request.
func TestFetchJobsForRun_ShapesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/api/v3/repos/owner/repo/actions/runs/42/jobs"
		if r.URL.Path != wantPath {
			t.Errorf("path = %q, want %q", r.URL.Path, wantPath)
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"total_count": 2,
			"jobs": [
				{"id": 9001, "name": "Lint", "status": "completed", "conclusion": "failure", "started_at": "2026-04-20T00:00:00Z", "completed_at": "2026-04-20T00:00:30Z"},
				{"id": 9002, "name": "Build (node 20)", "status": "in_progress", "conclusion": null, "started_at": "2026-04-20T00:00:00Z", "completed_at": null}
			]
		}`))
	}))
	t.Cleanup(srv.Close)

	client := github.NewClient(srv.URL, "test-token")
	jobs, err := fetchJobsForRun(context.Background(), client, "owner", "repo", 42)
	if err != nil {
		t.Fatalf("fetchJobsForRun: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("len = %d, want 2", len(jobs))
	}
	if jobs[0].ID != 9001 || jobs[0].Name != "Lint" || jobs[0].Status != "completed" || jobs[0].Conclusion != "failure" {
		t.Errorf("job[0] shape wrong: %+v", jobs[0])
	}
	if jobs[1].ID != 9002 || jobs[1].Status != "in_progress" || jobs[1].Conclusion != "" {
		t.Errorf("job[1] shape wrong: %+v", jobs[1])
	}
}

// TestDeriveRunStatus walks the decision matrix that lets us avoid a
// second API call to /actions/runs/{id}. The mapping is the contract
// agents see in the response, so any change here ripples to prompt-side
// expectations.
func TestDeriveRunStatus(t *testing.T) {
	cases := []struct {
		name string
		jobs []jobInfo
		want string
	}{
		{"empty → queued", nil, "queued"},
		{"all queued → queued", []jobInfo{{Status: "queued"}, {Status: "queued"}}, "queued"},
		{"all completed → completed", []jobInfo{{Status: "completed"}, {Status: "completed"}}, "completed"},
		{"any in_progress → in_progress", []jobInfo{{Status: "completed"}, {Status: "in_progress"}}, "in_progress"},
		{"queued + completed → in_progress", []jobInfo{{Status: "queued"}, {Status: "completed"}}, "in_progress"},
		{"single in_progress → in_progress", []jobInfo{{Status: "in_progress"}}, "in_progress"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveRunStatus(tc.jobs); got != tc.want {
				t.Errorf("deriveRunStatus(%+v) = %q, want %q", tc.jobs, got, tc.want)
			}
		})
	}
}

// TestSanitizeJobNameForFile guards the filename-mangling rule. The
// agent reads LogPath out of the response so it doesn't need to recompute
// the path, but the contract is "any GitHub job name produces a usable
// POSIX filename component."
func TestSanitizeJobNameForFile(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Lint", "Lint"},
		{"build (ubuntu-latest)", "build__ubuntu-latest_"},
		{"Lint / typecheck (node 20)", "Lint___typecheck__node_20_"},
		{"matrix.os: ubuntu", "matrix.os__ubuntu"},
		{"123_abc-XYZ.log", "123_abc-XYZ.log"},
		{"", "job"},
		{"!!!", "___"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := sanitizeJobNameForFile(tc.in); got != tc.want {
				t.Errorf("sanitizeJobNameForFile(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDownloadPerJobLogsToDir_HappyPath verifies the fallback writes one
// file per completed job, skips in_progress / queued jobs (their LogPath
// stays empty as a stub), and reports the cumulative bytes downloaded.
func TestDownloadPerJobLogsToDir_HappyPath(t *testing.T) {
	const lintBody = "lint failed: missing semicolon at line 42\n"
	const buildBody = "build succeeded in 3m21s\n"

	// Map job ID → response body so the handler can serve per-job logs
	// without coupling to URL-parsing heuristics.
	logBodies := map[int64]string{
		1001: lintBody,
		1002: buildBody,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Match path: /api/v3/repos/owner/repo/actions/jobs/{id}/logs
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 2 || parts[len(parts)-1] != "logs" {
			http.NotFound(w, r)
			return
		}
		jobIDStr := parts[len(parts)-2]
		jobID, err := strconv.ParseInt(jobIDStr, 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		body, ok := logBodies[jobID]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	client := github.NewClient(srv.URL, "test-token")
	destDir := filepath.Join(t.TempDir(), "_tfac", "ci-logs", "42")

	jobs := []jobInfo{
		{ID: 1001, Name: "Lint / typecheck", Status: "completed", Conclusion: "failure"},
		{ID: 1002, Name: "Build", Status: "completed", Conclusion: "success"},
		{ID: 1003, Name: "E2E", Status: "in_progress"},
		{ID: 1004, Name: "Deploy", Status: "queued"},
	}

	bytes, updated, err := downloadPerJobLogsToDir(context.Background(), client, "owner", "repo", destDir, jobs)
	if err != nil {
		t.Fatalf("downloadPerJobLogsToDir: %v", err)
	}
	wantBytes := int64(len(lintBody) + len(buildBody))
	if bytes != wantBytes {
		t.Errorf("bytes = %d, want %d", bytes, wantBytes)
	}

	// Completed jobs got LogPath populated and the file contents match.
	wantLog := map[int64]string{1001: lintBody, 1002: buildBody}
	for i, j := range updated {
		if j.Status != "completed" {
			if j.LogPath != "" {
				t.Errorf("job %d (%s) is %s; LogPath should be empty stub, got %q", j.ID, j.Name, j.Status, j.LogPath)
			}
			continue
		}
		if j.LogPath == "" {
			t.Errorf("job %d (%s) completed but LogPath is empty", j.ID, j.Name)
			continue
		}
		got, err := os.ReadFile(j.LogPath)
		if err != nil {
			t.Errorf("read job[%d] log %q: %v", i, j.LogPath, err)
			continue
		}
		if string(got) != wantLog[j.ID] {
			t.Errorf("job %d log content = %q, want %q", j.ID, string(got), wantLog[j.ID])
		}
	}
}

// TestDownloadPerJobLogsToDir_FailureCleansUp asserts the cleanup defer
// fires when a per-job download errors mid-way: destDir must be removed
// so a retry sees a clean slate, not a partial extraction. Mirrors the
// equivalent guard on downloadAndExtractLogs (TestDownloadAndExtractLogs_FailureCleansUp).
func TestDownloadPerJobLogsToDir_FailureCleansUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Internal"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	client := github.NewClient(srv.URL, "test-token")
	destDir := filepath.Join(t.TempDir(), "_tfac", "ci-logs", "42")

	jobs := []jobInfo{{ID: 7777, Name: "Lint", Status: "completed"}}
	_, _, err := downloadPerJobLogsToDir(context.Background(), client, "owner", "repo", destDir, jobs)
	if err == nil {
		t.Fatal("expected error from 500 response, got nil")
	}
	if _, statErr := os.Stat(destDir); !os.IsNotExist(statErr) {
		t.Errorf("destDir should be cleaned up on failure, stat returned: %v", statErr)
	}
}

// TestDownloadPerJobLogsToDir_AllStubs covers the "queued early in run"
// case: no jobs are completed yet, so we write zero files but still
// return successfully with the stub list intact. This is the right
// behavior — the agent gets back the same job metadata, sees empty
// LogPaths, and knows to retry later.
func TestDownloadPerJobLogsToDir_AllStubs(t *testing.T) {
	// Server should never be hit since there's nothing to download. Fail
	// loudly if we ever do.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s — no completed jobs to download", r.URL.Path)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	client := github.NewClient(srv.URL, "test-token")
	destDir := filepath.Join(t.TempDir(), "_tfac", "ci-logs", "42")

	jobs := []jobInfo{
		{ID: 1, Name: "Lint", Status: "queued"},
		{ID: 2, Name: "Build", Status: "in_progress"},
	}
	bytes, updated, err := downloadPerJobLogsToDir(context.Background(), client, "owner", "repo", destDir, jobs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bytes != 0 {
		t.Errorf("bytes = %d, want 0", bytes)
	}
	for _, j := range updated {
		if j.LogPath != "" {
			t.Errorf("job %d should have empty LogPath stub, got %q", j.ID, j.LogPath)
		}
	}
	// destDir must exist (we MkdirAll'd it) but be empty.
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("readdir destDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("destDir should be empty, got %d entries", len(entries))
	}
}

// TestDownloadLogsResult_JSONShape pins the response envelope. Field
// renames here would break the prompt templates and `jq` paths agents
// rely on — fail the build before that ships.
func TestDownloadLogsResult_JSONShape(t *testing.T) {
	r := downloadLogsResult{
		RunID:           42,
		Owner:           "owner",
		Repo:            "repo",
		DestDir:         "/abs/dest",
		BytesDownloaded: 1024,
		RunStatus:       "in_progress",
		FallbackUsed:    true,
		Jobs: []jobInfo{
			{ID: 1, Name: "Lint", Status: "completed", Conclusion: "failure", LogPath: "/abs/dest/Lint-1.log"},
		},
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	for _, key := range []string{
		`"run_id"`, `"owner"`, `"repo"`, `"dest_dir"`, `"bytes_downloaded"`,
		`"run_status"`, `"fallback_used"`, `"jobs"`,
		`"id"`, `"name"`, `"status"`, `"conclusion"`, `"log_path"`,
	} {
		if !strings.Contains(s, key) {
			t.Errorf("expected JSON to contain %s; got %s", key, s)
		}
	}
	// entries omitted when nil — the fallback path doesn't extract an archive.
	if strings.Contains(s, `"entries"`) {
		t.Errorf("expected entries to be omitted when nil (fallback path); got %s", s)
	}
}

// Confirm the result struct serializes to the shape agents parse with jq.
// If a field rename ever drifts this test would catch it before the prompt
// templates started referencing stale JSON keys.
func TestListRunsResult_JSONShape(t *testing.T) {
	r := listRunsResult{
		Owner:   "owner",
		Repo:    "repo",
		HeadSHA: "abc",
		Filter:  listRunFilter{PR: 18},
		Runs: []listRun{
			{RunID: 1, WorkflowName: "test", Conclusion: "failure"},
		},
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	for _, key := range []string{`"owner"`, `"repo"`, `"filter"`, `"runs"`, `"head_sha"`, `"run_id"`, `"workflow_name"`, `"conclusion"`} {
		if !strings.Contains(s, key) {
			t.Errorf("expected JSON to contain %s; got %s", key, s)
		}
	}
}
