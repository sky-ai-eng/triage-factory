// Package ghbin provisions the TF-owned `gh` release binary on a local-mode
// host. Multi mode bakes the same pinned release into the runtime image and
// bind-mounts it into the jail; local mode has no image, so the first run that
// wants the gh channel fetches the pinned asset here, verifies it against
// internal/ghpin, and installs it under the state root.
//
// # Never the user's gh
//
// The binary on the user's PATH is not a substitute. It may be any version, and
// — the load-bearing reason — it is authenticated as the *human*, so an agent
// invoking it would act under their full personal GitHub scope, outside the
// per-run credential channel entirely. Everything here exists so a run resolves
// gh to a TF-owned path with TF-owned auth.
//
// # Fetch failure is not run failure
//
// Ensure returns an error rather than installing anything half-verified, and
// every caller treats that error as "no gh channel this run" — the run proceeds
// through the existing scoped exec verbs. That degradation is what lets the
// channel ship before those verbs retire.
package ghbin

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ghpin"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// ErrUnsupportedPlatform is returned when the running GOOS/GOARCH has no pinned
// gh asset. A miss, not a defect: the caller drops the gh channel and the run
// continues on the exec verbs.
var ErrUnsupportedPlatform = errors.New("ghbin: no pinned gh release for this platform")

// downloadBase is where release assets are fetched from. A var so a test can
// serve the pinned asset locally instead of reaching GitHub.
var downloadBase = ghpin.DownloadBase

// fetchTimeout bounds one download. The asset is ~10-15 MB compressed; a stalled
// fetch must not pin a run's setup indefinitely.
const fetchTimeout = 3 * time.Minute

// maxExtractBytes caps what a single archive member may expand to. The gh
// binary is under 50 MB; the cap exists so a swapped-out archive can't fill the
// user's disk before the checksum ever gets compared.
const maxExtractBytes = 256 << 20

// ensureMu serializes provisioning across concurrent runs in one process. Two
// runs starting together would otherwise both fetch and both rename over the
// same path; the rename is atomic, so the race is wasteful rather than
// corrupting, but there is no reason to pay for it twice.
var ensureMu sync.Mutex

// Ensure returns the absolute path of the TF-owned gh binary, fetching and
// verifying the pinned release on first use.
//
// Idempotent: an installed binary whose recorded provenance matches the current
// pin AND whose bytes still hash to what was recorded is reused untouched.
// Anything else — no binary, a stale pin, a rewritten file — is re-fetched.
// Every fetch is checked against the pinned archive SHA256 before a single byte
// is installed.
func Ensure(ctx context.Context) (string, error) {
	asset, ok := ghpin.AssetFor(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return "", fmt.Errorf("%w (%s/%s)", ErrUnsupportedPlatform, runtime.GOOS, runtime.GOARCH)
	}
	// Pre-flight the state root through the error-returning form so an
	// unresolvable $HOME surfaces here rather than panicking inside the
	// path resolvers below.
	if _, err := paths.StateRootErr(); err != nil {
		return "", fmt.Errorf("ghbin: resolve state root: %w", err)
	}

	ensureMu.Lock()
	defer ensureMu.Unlock()

	path := paths.GHBinaryPath()
	if installedMatchesPin(path, asset) {
		return path, nil
	}
	if err := install(ctx, path, asset); err != nil {
		return "", err
	}
	return path, nil
}

// receiptPath is where the provenance of the installed binary is recorded,
// alongside it.
func receiptPath(binPath string) string { return binPath + ".pin" }

// receipt is what was installed and from what: the pin's version and archive
// checksum (so a version bump invalidates the install) plus the SHA256 of the
// extracted binary (so a rewritten file is detected).
type receipt struct {
	version   string
	assetSHA  string
	binarySHA string
}

func readReceipt(path string) (receipt, bool) {
	body, err := os.ReadFile(path)
	if err != nil {
		return receipt{}, false
	}
	fields := strings.Fields(string(body))
	if len(fields) != 3 {
		return receipt{}, false
	}
	return receipt{version: fields[0], assetSHA: fields[1], binarySHA: fields[2]}, true
}

// installedMatchesPin reports whether the binary already on disk is the pinned
// one. It re-hashes the file rather than trusting the receipt alone: the receipt
// says what was installed, the hash says what is there now.
func installedMatchesPin(binPath string, asset ghpin.Asset) bool {
	r, ok := readReceipt(receiptPath(binPath))
	if !ok || r.version != ghpin.Version || r.assetSHA != asset.SHA256 {
		return false
	}
	info, err := os.Stat(binPath)
	if err != nil || info.IsDir() {
		return false
	}
	sum, err := fileSHA256(binPath)
	if err != nil {
		return false
	}
	return sum == r.binarySHA
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// install downloads the pinned asset, verifies its checksum, extracts the gh
// executable, and publishes it atomically at binPath with its receipt.
func install(ctx context.Context, binPath string, asset ghpin.Asset) error {
	dir := filepath.Dir(binPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ghbin: create %s: %w", dir, err)
	}

	archive, err := download(ctx, asset)
	if err != nil {
		return err
	}
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archive.Name())
	}()

	sum, err := fileSHA256(archive.Name())
	if err != nil {
		return fmt.Errorf("ghbin: checksum downloaded asset: %w", err)
	}
	if sum != asset.SHA256 {
		return fmt.Errorf("ghbin: %s checksum mismatch: got %s, pinned %s", asset.Name, sum, asset.SHA256)
	}

	staged, err := extractBinary(archive.Name(), asset, dir)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(staged) }()

	binarySHA, err := fileSHA256(staged)
	if err != nil {
		return fmt.Errorf("ghbin: checksum extracted binary: %w", err)
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		return fmt.Errorf("ghbin: chmod %s: %w", staged, err)
	}
	// Receipt first, binary second: a crash between the two leaves a receipt
	// describing a binary that isn't there, which the next Ensure treats as a
	// miss and re-installs. The reverse order would leave an unrecorded binary
	// that also re-installs — both safe, but this way there is never a moment
	// where a receipt vouches for an older file at the final path.
	rec := fmt.Sprintf("%s %s %s\n", ghpin.Version, asset.SHA256, binarySHA)
	if err := os.WriteFile(receiptPath(binPath), []byte(rec), 0o600); err != nil {
		return fmt.Errorf("ghbin: write receipt: %w", err)
	}
	if err := os.Rename(staged, binPath); err != nil {
		return fmt.Errorf("ghbin: install %s: %w", binPath, err)
	}
	return nil
}

// download fetches the asset to a temp file and returns it open at offset 0's
// caller's discretion (callers only use Name()). The caller removes it.
func download(ctx context.Context, asset ghpin.Asset) (*os.File, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	url := asset.URL(downloadBase)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ghbin: build request for %s: %w", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ghbin: fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ghbin: fetch %s: HTTP %s", url, resp.Status)
	}

	f, err := os.CreateTemp("", "tf-gh-asset-*")
	if err != nil {
		return nil, fmt.Errorf("ghbin: temp file: %w", err)
	}
	if _, err := io.Copy(f, io.LimitReader(resp.Body, maxExtractBytes)); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("ghbin: download %s: %w", url, err)
	}
	return f, nil
}

// extractBinary pulls the single gh executable out of the verified archive into
// a temp file inside destDir (same filesystem, so the publish is a rename) and
// returns its path. Only the pinned archive-relative path is extracted — no
// other member is written anywhere.
func extractBinary(archivePath string, asset ghpin.Asset, destDir string) (string, error) {
	out, err := os.CreateTemp(destDir, "gh-staged-*")
	if err != nil {
		return "", fmt.Errorf("ghbin: stage binary: %w", err)
	}
	staged := out.Name()
	cleanup := func(err error) (string, error) {
		_ = out.Close()
		_ = os.Remove(staged)
		return "", err
	}

	var copyErr error
	switch asset.Format {
	case ghpin.FormatTarGz:
		copyErr = copyFromTarGz(archivePath, asset.BinPath, out)
	case ghpin.FormatZip:
		copyErr = copyFromZip(archivePath, asset.BinPath, out)
	default:
		copyErr = fmt.Errorf("ghbin: unknown archive format %q", asset.Format)
	}
	if copyErr != nil {
		return cleanup(copyErr)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(staged)
		return "", fmt.Errorf("ghbin: stage binary: %w", err)
	}
	return staged, nil
}

func copyFromTarGz(archivePath, member string, dst io.Writer) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("ghbin: open archive: %w", err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("ghbin: gunzip archive: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("ghbin: read archive: %w", err)
		}
		if hdr.Name != member || hdr.Typeflag != tar.TypeReg {
			continue
		}
		if _, err := io.Copy(dst, io.LimitReader(tr, maxExtractBytes)); err != nil {
			return fmt.Errorf("ghbin: extract %s: %w", member, err)
		}
		return nil
	}
	return fmt.Errorf("ghbin: archive has no member %q", member)
}

func copyFromZip(archivePath, member string, dst io.Writer) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("ghbin: open archive: %w", err)
	}
	defer func() { _ = zr.Close() }()

	for _, f := range zr.File {
		if f.Name != member || f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("ghbin: extract %s: %w", member, err)
		}
		_, err = io.Copy(dst, io.LimitReader(rc, maxExtractBytes))
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("ghbin: extract %s: %w", member, err)
		}
		return nil
	}
	return fmt.Errorf("ghbin: archive has no member %q", member)
}
