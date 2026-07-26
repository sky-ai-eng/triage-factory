package ghbin

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/ghpin"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

const fakeGHBody = "#!/bin/sh\necho gh version 0.0.0-test\n"

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// tarGzArchive builds a gzipped tar carrying member at the given path, plus a
// decoy the extractor must ignore.
func tarGzArchive(t *testing.T, member, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct{ name, body string }{
		{filepath.Dir(filepath.Dir(member)) + "/LICENSE", "not the binary"},
		{member, body},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func zipArchive(t *testing.T, member, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range []struct{ name, body string }{
		{filepath.Dir(filepath.Dir(member)) + "/LICENSE", "not the binary"},
		{member, body},
	} {
		w, err := zw.Create(f.name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := w.Write([]byte(f.body)); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// serveAsset stands a release server in front of body and points the fetcher at
// it for the duration of the test. It returns the asset descriptor whose
// checksum matches body, so a test can corrupt either side independently.
func serveAsset(t *testing.T, name string, format ghpin.Format, binPath string, body []byte) ghpin.Asset {
	t.Helper()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/"+name) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		hits++
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	prev := downloadBase
	downloadBase = srv.URL
	t.Cleanup(func() { downloadBase = prev })

	return ghpin.Asset{Name: name, SHA256: sha256Hex(body), Format: format, BinPath: binPath}
}

func TestInstall_ExtractsVerifiesAndPublishes(t *testing.T) {
	member := "gh_x/bin/gh"
	asset := serveAsset(t, "gh_x.tar.gz", ghpin.FormatTarGz, member, tarGzArchive(t, member, fakeGHBody))

	binPath := filepath.Join(t.TempDir(), "bin", "gh")
	if err := install(context.Background(), binPath, asset); err != nil {
		t.Fatalf("install: %v", err)
	}

	got, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(got) != fakeGHBody {
		t.Errorf("installed binary = %q, want the archive member body", got)
	}
	info, err := os.Stat(binPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
		t.Errorf("installed mode = %v, want 0755", info.Mode().Perm())
	}
	if !installedMatchesPin(binPath, asset) {
		t.Error("installedMatchesPin = false right after install, want true (the reuse path would re-fetch every run)")
	}
	// Nothing but the binary and its receipt lands in the directory — the
	// decoy member in the archive must not be written anywhere.
	entries, err := os.ReadDir(filepath.Dir(binPath))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("bin dir = %v, want exactly the binary and its receipt", names)
	}
}

func TestInstall_Zip(t *testing.T) {
	member := "gh_x/bin/gh"
	asset := serveAsset(t, "gh_x.zip", ghpin.FormatZip, member, zipArchive(t, member, fakeGHBody))

	binPath := filepath.Join(t.TempDir(), "bin", "gh")
	if err := install(context.Background(), binPath, asset); err != nil {
		t.Fatalf("install: %v", err)
	}
	got, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(got) != fakeGHBody {
		t.Errorf("installed binary = %q, want the archive member body", got)
	}
}

// A served asset that doesn't hash to the pin must never reach the final path —
// this is the whole point of pinning, so assert the absence, not just the error.
func TestInstall_ChecksumMismatchInstallsNothing(t *testing.T) {
	member := "gh_x/bin/gh"
	asset := serveAsset(t, "gh_x.tar.gz", ghpin.FormatTarGz, member, tarGzArchive(t, member, fakeGHBody))
	asset.SHA256 = strings.Repeat("0", 64)

	binPath := filepath.Join(t.TempDir(), "bin", "gh")
	err := install(context.Background(), binPath, asset)
	if err == nil {
		t.Fatal("install succeeded on a checksum mismatch, want an error")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error = %v, want a checksum mismatch", err)
	}
	if _, statErr := os.Stat(binPath); !os.IsNotExist(statErr) {
		t.Errorf("stat(%s) = %v, want the binary never to have been published", binPath, statErr)
	}
}

func TestInstall_MissingMember(t *testing.T) {
	asset := serveAsset(t, "gh_x.tar.gz", ghpin.FormatTarGz, "gh_x/bin/gh", tarGzArchive(t, "gh_x/bin/other", fakeGHBody))

	binPath := filepath.Join(t.TempDir(), "bin", "gh")
	if err := install(context.Background(), binPath, asset); err == nil {
		t.Fatal("install succeeded with no gh member in the archive, want an error")
	}
}

// A rewritten binary is a mismatch even with an intact receipt — the receipt
// records what was installed, the hash decides what is there now.
func TestInstalledMatchesPin_DetectsRewrittenBinary(t *testing.T) {
	member := "gh_x/bin/gh"
	asset := serveAsset(t, "gh_x.tar.gz", ghpin.FormatTarGz, member, tarGzArchive(t, member, fakeGHBody))

	binPath := filepath.Join(t.TempDir(), "bin", "gh")
	if err := install(context.Background(), binPath, asset); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\nexfiltrate\n"), 0o755); err != nil {
		t.Fatalf("rewrite binary: %v", err)
	}
	if installedMatchesPin(binPath, asset) {
		t.Error("installedMatchesPin = true after the binary was rewritten, want false (it must be re-fetched)")
	}
}

// A pin bump invalidates an otherwise-intact install.
func TestInstalledMatchesPin_DetectsPinBump(t *testing.T) {
	member := "gh_x/bin/gh"
	asset := serveAsset(t, "gh_x.tar.gz", ghpin.FormatTarGz, member, tarGzArchive(t, member, fakeGHBody))

	binPath := filepath.Join(t.TempDir(), "bin", "gh")
	if err := install(context.Background(), binPath, asset); err != nil {
		t.Fatalf("install: %v", err)
	}
	bumped := asset
	bumped.SHA256 = strings.Repeat("a", 64)
	if installedMatchesPin(binPath, bumped) {
		t.Error("installedMatchesPin = true against a different pinned checksum, want false")
	}
}

// Ensure resolves through internal/paths and reuses an already-provisioned
// binary without touching the network — the steady-state path every run after
// the first takes.
func TestEnsure_ReusesProvisionedBinary(t *testing.T) {
	asset, ok := ghpin.AssetFor(runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Skipf("no pinned gh asset for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	root := t.TempDir()
	paths.SetForTest(t, root)

	// Point the fetcher at a server that fails the test if it is ever reached:
	// a reuse must not re-download.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("Ensure re-downloaded an already-provisioned binary")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	prev := downloadBase
	downloadBase = srv.URL
	t.Cleanup(func() { downloadBase = prev })

	binPath := paths.GHBinaryPath()
	if err := os.MkdirAll(filepath.Dir(binPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(binPath, []byte(fakeGHBody), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	rec := ghpin.Version + " " + asset.SHA256 + " " + sha256Hex([]byte(fakeGHBody)) + "\n"
	if err := os.WriteFile(receiptPath(binPath), []byte(rec), 0o600); err != nil {
		t.Fatalf("write receipt: %v", err)
	}

	got, err := Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got != binPath {
		t.Errorf("Ensure = %q, want %q", got, binPath)
	}
}
