//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// alpineCommunityRepo is the community repository URL for the alpine
// version pinned in rootfs.go. Some toolchain packages (ripgrep, go)
// live in community rather than main, so the cache build enables
// this repo before invoking apk-add. Linux-only — referenced
// exclusively from the chroot+apk path in this file.
//
// Derived from majorMinor(alpineVersion) rather than hardcoded so a
// future alpineVersion bump can't leave the community repo URL
// pointing at the wrong release-tree.
var alpineCommunityRepo = "https://dl-cdn.alpinelinux.org/alpine/v" + majorMinor(alpineVersion) + "/community"

// rootfsCacheMu serializes ensureRootfs across concurrent Wrap calls.
// We use a mutex rather than sync.Once because sync.Once permanently
// caches the first call's result — a transient failure (CDN flap,
// disk full at extract time) would then poison every subsequent
// Wrap for the lifetime of the process. The mutex guards a check of
// the on-disk sentinel file, so successful prior calls return fast
// without re-extracting, while failures don't lock anyone out of
// retrying.
var rootfsCacheMu sync.Mutex

// ensureRootfs idempotently downloads + verifies + extracts the
// alpine minirootfs and apk-installs the agent toolchain into
// ~/.triagefactory/sandbox/rootfs-<cacheKey>/ (where cacheKey hashes
// the alpine sha + the apk package set). Returns the path.
// Concurrency-safe via a mutex; success cached via the sentinel file
// inside doEnsureRootfs (so re-entry after a successful first call
// is just one os.Stat).
//
// ctx threads through the cold-cache path: download honors deadline
// via http.NewRequestWithContext; the apk-add chroot runs via
// exec.CommandContext. The hot-cache path (sentinel hit) returns
// without consulting ctx.
func ensureRootfs(ctx context.Context) (string, error) {
	rootfsCacheMu.Lock()
	defer rootfsCacheMu.Unlock()
	return doEnsureRootfs(ctx)
}

func doEnsureRootfs(ctx context.Context) (string, error) {
	// Cache key hashes (alpine sha, package set). Adding a package to
	// apkPackages changes the key and forces a fresh extraction so we
	// never serve a cached rootfs whose toolchain doesn't match the
	// current build's expectations. Errors on unsupported GOARCH —
	// surfaced through the normal error path rather than panicking.
	cacheKey, err := rootfsCacheKey()
	if err != nil {
		return "", fmt.Errorf("rootfs: %w", err)
	}
	// Host-global cache: shared read-only across all tenants, so it hangs
	// off the state root with no org segment (org-scoping it would
	// re-extract an identical toolchain per org).
	cacheDir := paths.SandboxRootfsDir(cacheKey)

	// Sentinel file marking a successful extraction + toolchain
	// install. Distinguishes "fully populated cache" from "crashed
	// mid-build, leaving partial files."
	sentinel := filepath.Join(cacheDir, ".extracted-ok")
	if _, err := os.Stat(sentinel); err == nil {
		return cacheDir, nil
	}

	if err := os.MkdirAll(filepath.Dir(cacheDir), 0o755); err != nil {
		return "", fmt.Errorf("rootfs: mkdir parent: %w", err)
	}

	// Clean any partial extraction from a previous crash before
	// starting fresh.
	if err := os.RemoveAll(cacheDir); err != nil {
		return "", fmt.Errorf("rootfs: clean partial cache: %w", err)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("rootfs: mkdir cache: %w", err)
	}

	// Download to a temp file, sha256-verify, then extract. Failing
	// the verify means upstream alpine got tampered with or our pin
	// is stale — refuse loudly rather than silently extract.
	// rootfsCacheKey already proved currentArchRootfs is resolvable
	// above; the second call here cannot fail.
	url, sha, err := currentArchRootfs()
	if err != nil {
		return "", fmt.Errorf("rootfs: %w", err)
	}
	tmpTarball := filepath.Join(cacheDir, ".alpine.tgz.partial")
	if err := downloadFile(ctx, url, tmpTarball); err != nil {
		return "", fmt.Errorf("rootfs: download: %w", err)
	}
	defer func() { _ = os.Remove(tmpTarball) }()

	if err := verifySHA256(tmpTarball, sha); err != nil {
		return "", fmt.Errorf("rootfs: verify sha256: %w", err)
	}

	if err := extractTarGz(tmpTarball, cacheDir); err != nil {
		return "", fmt.Errorf("rootfs: extract: %w", err)
	}

	// Layer the agent toolchain on top of the bare minirootfs via
	// chroot+apk. Bare minirootfs ships busybox only — no node, git,
	// rg, python, go — which suffices for boot-shaped smoke tests
	// but every real agent run needs the toolchain. Done once per
	// cache build, then frozen in the cached rootfs and reused
	// across runs.
	if err := installToolchain(ctx, cacheDir); err != nil {
		return "", fmt.Errorf("rootfs: install toolchain: %w", err)
	}

	if err := os.WriteFile(sentinel, []byte("ok"), 0o644); err != nil {
		return "", fmt.Errorf("rootfs: write sentinel: %w", err)
	}
	return cacheDir, nil
}

// installToolchain runs `apk add` inside the freshly extracted rootfs
// via chroot. The bare alpine minirootfs has busybox only; every real
// agent run needs node + git + rg + python + go + build tools.
//
// chroot(2) needs CAP_SYS_CHROOT, which root has. The sandbox path
// already runs as root in production (netns + iptables manipulation
// require their own caps, and the wrapper script re-execs under
// sudo), so callers reach this with sufficient privilege; we don't
// add a separate preflight. apk ships as a static binary inside the
// alpine minirootfs, so we don't need apk installed on the host.
func installToolchain(ctx context.Context, rootfsDir string) error {
	// Some packages we install (ripgrep, go) live in the community
	// repository, which the bare minirootfs does NOT enable by
	// default. Enable it before apk-add or the install fails with
	// "unable to select packages."
	if err := enableCommunityRepo(filepath.Join(rootfsDir, "etc", "apk", "repositories")); err != nil {
		return fmt.Errorf("enable community repo: %w", err)
	}

	// apk needs DNS resolution to reach dl-cdn.alpinelinux.org from
	// inside the chroot. The minirootfs has no /etc/resolv.conf, so
	// without this apk fails with "temporary failure in name
	// resolution." Reuse the same public-resolver config the sandbox
	// itself uses at launch (the per-run resolv.conf bind mount
	// overwrites this at runtime, so leaving it in the cached rootfs
	// is cosmetic — no need to clean up after).
	resolvPath := filepath.Join(rootfsDir, "etc", "resolv.conf")
	if err := os.WriteFile(resolvPath, []byte(resolvConfContent), 0o644); err != nil {
		return fmt.Errorf("write chroot resolv.conf: %w", err)
	}

	args := append([]string{rootfsDir, "/sbin/apk", "add", "--no-cache"}, apkPackages...)
	cmd := exec.CommandContext(ctx, "chroot", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("chroot apk add: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// enableCommunityRepo appends the alpine community repo URL to the
// rootfs's /etc/apk/repositories if it isn't already listed. The
// minirootfs ships with main only.
func enableCommunityRepo(repoFile string) error {
	data, err := os.ReadFile(repoFile)
	if err != nil {
		return err
	}
	if bytes.Contains(data, []byte("/community")) {
		return nil
	}
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		data = append(data, '\n')
	}
	data = append(data, []byte(alpineCommunityRepo+"\n")...)
	return os.WriteFile(repoFile, data, 0o644)
}

func downloadFile(ctx context.Context, url, dst string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return err
	}
	return out.Sync()
}

func verifySHA256(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, want)
	}
	return nil
}
