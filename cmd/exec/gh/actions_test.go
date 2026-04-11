package gh

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/todo-triage/internal/github"
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
	if err := extractZip(zipPath, destDir, maxPerFileBytes); err != nil {
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
			err := extractZip(zipPath, destDir, maxPerFileBytes)
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

	err := extractZip(zipPath, destDir, testCap)
	if err == nil {
		t.Fatal("expected extraction to fail on oversized entry, got nil error")
	}
	if !strings.Contains(err.Error(), "per-file size cap") {
		t.Errorf("error should mention size cap, got: %v", err)
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
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "todotriage-ci-logs-*.zip"))
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
	destDir := filepath.Join(t.TempDir(), "_scratch", "ci-logs", "123")

	before := countCILogTempFiles(t)
	n, err := downloadAndExtractLogs(client, "owner", "repo", 123, destDir)
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
	destDir := filepath.Join(t.TempDir(), "_scratch", "ci-logs", "123")

	before := countCILogTempFiles(t)
	_, err := downloadAndExtractLogs(client, "owner", "repo", 123, destDir)
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
	destDir := filepath.Join(t.TempDir(), "_scratch", "ci-logs", "123")

	before := countCILogTempFiles(t)
	_, err := downloadAndExtractLogs(client, "owner", "repo", 123, destDir)
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
