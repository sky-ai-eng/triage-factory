package sandbox

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// alpineVersion pins the alpine minirootfs release. Same release as the
// validation probe (probe.sh line 64) so the runtime behavior matches
// what was tested. Bumping requires re-running the probe to verify
// nothing changed in alpine's syscall surface that the SDK depends on
// and re-pinning both per-arch sha256s in alpineRootfsForArch.
const alpineVersion = "3.20.3"

// alpineRootfsForArch returns the URL + sha256 for the alpine
// minirootfs tarball matching the current GOARCH. Pinned per-arch
// because the tarball content differs even at the same alpine
// version. Errors out cleanly on unsupported architectures (ppc64le,
// s390x, riscv64, ...) so a future user gets an actionable message
// instead of a sha mismatch on download.
//
// The cache key (rootfsCacheKey) hashes the sha so per-arch caches
// stay separate by construction — switching between amd64 and arm64
// builds on the same host (cross-compile workflows) does not clobber
// the other arch's cached extraction.
func alpineRootfsForArch(arch string) (url, sha string, err error) {
	switch arch {
	case "amd64":
		return "https://dl-cdn.alpinelinux.org/alpine/v" + majorMinor(alpineVersion) +
				"/releases/x86_64/alpine-minirootfs-" + alpineVersion + "-x86_64.tar.gz",
			"d4e6fd67dcf75e40c451560ac7265166c2b72a0f38ddc9aae756a7de3d1efa0c", nil
	case "arm64":
		return "https://dl-cdn.alpinelinux.org/alpine/v" + majorMinor(alpineVersion) +
				"/releases/aarch64/alpine-minirootfs-" + alpineVersion + "-aarch64.tar.gz",
			"041fa34a81788242df9e78fa69b97ab45b8ec47ddbf88864755610414a7bf3de", nil
	default:
		return "", "", fmt.Errorf("sandbox: unsupported GOARCH %q (only amd64 and arm64 are pinned; add a case to alpineRootfsForArch to support more)", arch)
	}
}

// majorMinor strips the patch suffix off a semver-ish version string —
// "3.20.3" → "3.20" — for use in the alpine release-tree URL.
func majorMinor(version string) string {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return version
	}
	return parts[0] + "." + parts[1]
}

// currentArchRootfs is the URL + sha for the host's GOARCH. Returns
// an error on unsupported arch so callers (ensureRootfs, the cache-
// key wrapper) can surface a clean diagnostic via their normal error
// path instead of crashing the whole process via panic.
func currentArchRootfs() (url, sha string, err error) {
	return alpineRootfsForArch(runtime.GOARCH)
}

// apkPackages is the toolchain installed into the cached alpine
// rootfs after extraction. Any change here invalidates the rootfs
// cache key (rootfsCacheKey hashes this slice alongside the alpine
// tarball sha) so a fresh extraction picks up the new package set
// on the next sandbox launch.
//
//   - nodejs/npm — agent SDK runtime. Corepack ships bundled with npm,
//     but shipping isn't the same as being on PATH: pnpm/yarn only
//     become runnable once `corepack enable` writes their dispatcher
//     shims, which installToolchain (rootfs_linux.go) now does at
//     build time — see enableCorepack and pnpmVersion below.
//   - git — every delegate/curator flow does status/diff/commit/push.
//   - ripgrep — agent's primary code-search tool; faster than grep
//     on large repos.
//   - bash — alpine ships ash by default; many shell scripts and
//     agent invocations assume bash.
//   - ca-certificates — outbound TLS from in-sandbox tools (git over
//     HTTPS, npm registry, the in-host proxy a follow-up ticket
//     wires in).
//   - python3 — common in build scripts and tooling glue.
//   - go — Go projects need go build/test/mod tidy to verify changes.
//   - make — ubiquitous in build flows.
//   - curl — ubiquitous, tiny, expected.
//   - openssh-client — git over SSH and any tooling that calls ssh.
//   - build-base — gcc + make + musl-dev; required for native deps
//     (node-gyp, cgo, anything that compiles at install time).
var apkPackages = []string{
	"nodejs", "npm", "git", "ripgrep", "bash", "ca-certificates",
	"python3", "go", "make", "curl", "openssh-client", "build-base",
}

// pnpmVersion pins the pnpm release that installToolchain pre-warms
// into the cached rootfs via `corepack prepare --activate`
// (rootfs_linux.go's enableCorepack), so a sandboxed agent's `pnpm
// install` resolves the pin from the local corepack cache instead of
// reaching for the network. Must match frontend/package.json's
// "packageManager" field — bump both together. Folded into
// rootfsCacheKeyFor's hash so a version bump invalidates existing
// operator caches the same way an apkPackages change does.
const pnpmVersion = "11.9.0"

// CorepackHome is the fixed, $HOME-independent corepack cache
// directory, set as COREPACK_HOME at BOTH rootfs-bake time
// (chrootToolchainEnv, rootfs_linux.go) and agent-run time
// (agentproc's buildSandboxEnv). Corepack resolves its cache from
// $HOME unless COREPACK_HOME overrides it, and the two phases run
// under different HOMEs (/root in the bake chroot, /work in the
// sandbox) — without this pin the pre-warmed pnpm release lands in
// /root/.cache, runtime corepack looks in /work/.cache, misses, and
// falls back to a network fetch, defeating the pre-warm entirely.
// Exported so the agentproc runtime env references the same constant
// the bake writes to — the compile-time link is what keeps the two
// sides from drifting. Folded into the rootfs cache key: moving the
// path changes what the bake produces, so it must re-extract.
const CorepackHome = "/opt/corepack"

// rootfsCacheKey returns the 12-hex-char cache key for the current
// rootfs build inputs, or an error if the host's GOARCH is not
// supported. Error-returning rather than panicking so a misconfigured
// arch surfaces through the same error log as any other startup
// failure rather than crashing the whole process.
func rootfsCacheKey() (string, error) {
	_, sha, err := currentArchRootfs()
	if err != nil {
		return "", err
	}
	return rootfsCacheKeyFor(sha, apkPackages), nil
}

// rootfsCacheKeyFor hashes (alpine_sha, sorted-package-set, pnpmVersion)
// and returns the first 12 hex chars. Sorting before hashing means the
// key depends on the package set, not on slice ordering — a future
// maintainer reshuffling apkPackages won't silently invalidate a
// cache that's actually still correct.
//
// The cache directory is derived from this key, so adding a package
// (or bumping pnpmVersion) produces a new key, forces a fresh
// extraction, and avoids the failure mode where the on-disk cache
// contains the old toolchain while the running binary expects the
// new one.
func rootfsCacheKeyFor(alpineSha string, packages []string) string {
	sorted := append([]string(nil), packages...)
	sort.Strings(sorted)
	h := sha256.New()
	h.Write([]byte(alpineSha))
	h.Write([]byte{0})
	for _, p := range sorted {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	h.Write([]byte(pnpmVersion))
	h.Write([]byte{0})
	h.Write([]byte(CorepackHome))
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// extractTarGz unpacks a gzip-compressed tar archive into dst.
// Two-pass for security:
//
//  1. First pass writes directories + regular files only. Defers
//     all symlink entries to a slice.
//  2. Second pass creates the deferred symlinks.
//
// Why two-pass: the obvious single-pass approach is vulnerable to a
// "symlink-then-write-through-it" attack. A malicious tarball can
// emit `link -> /etc` followed by `link/file`; in the single-pass
// path, the symlink write completes first, then `link/file`'s
// MkdirAll/OpenFile silently traverses the symlink and writes to
// `/etc/file` on the host. Two-pass extraction defers symlink
// creation until after every regular file has been written, so no
// subsequent open() can traverse a symlink we just created.
//
// Alpine's official rootfs is sha256-pinned and trusted (the
// production source), so this attack isn't reachable today. The
// two-pass approach defends future pin updates against compromised
// mirrors and makes the extractor reusable for less-trusted
// tarballs (e.g. user-supplied custom rootfs in a future feature).
//
// Path sanitization on each entry rejects ../ traversal regardless
// of which pass it falls into.
//
// Cross-platform so the security-sensitive extractor is covered by
// CI tests on every dev box, not just Linux.
func extractTarGz(tarballPath, dst string) error {
	f, err := os.Open(tarballPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	dstAbs, err := filepath.Abs(dst)
	if err != nil {
		return err
	}

	// Deferred symlink entries — name + linkname, after path
	// sanitization. Created in a second pass after all regular
	// files are written.
	type deferredLink struct {
		name     string // sanitized absolute target path under dst
		linkname string // verbatim from tar header
	}
	var symlinks []deferredLink

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		// Sanitize the path: reject anything that escapes dst.
		clean := filepath.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") || strings.Contains(clean, "/../") {
			return fmt.Errorf("tarball entry escapes dst: %q", hdr.Name)
		}
		target := filepath.Join(dstAbs, clean)
		relTarget, err := filepath.Rel(dstAbs, target)
		if err != nil || strings.HasPrefix(relTarget, "..") {
			return fmt.Errorf("tarball entry resolves outside dst: %q", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			w, err := os.OpenFile(target,
				os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
				os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(w, tr); err != nil {
				w.Close()
				return err
			}
			if err := w.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// Validate the link target — relative symlinks must
			// stay inside the rootfs. Absolute symlinks (e.g.
			// /usr/bin/sh → /bin/sh) are permitted; the sandbox
			// interprets them relative to its own root, so they
			// can't escape the rootfs even if they look absolute.
			linkClean := filepath.Clean(hdr.Linkname)
			if !filepath.IsAbs(linkClean) && strings.HasPrefix(linkClean, "..") {
				resolved := filepath.Join(filepath.Dir(target), linkClean)
				rel, err := filepath.Rel(dstAbs, resolved)
				if err != nil || strings.HasPrefix(rel, "..") {
					return fmt.Errorf("symlink %q points outside rootfs", hdr.Name)
				}
			}
			// Defer creation to the second pass so no later regular
			// file write can be funneled through this symlink.
			symlinks = append(symlinks, deferredLink{
				name:     target,
				linkname: hdr.Linkname,
			})
		default:
			// Skip char/block devices, fifos, etc. — alpine
			// minirootfs ships only files/dirs/symlinks; anything
			// else is suspicious and skipping is safer than handling.
		}
	}

	// Second pass: create symlinks now that every regular file is
	// in place. No subsequent open() runs after this, so creating
	// the symlinks last means nothing we write can traverse one.
	for _, l := range symlinks {
		if err := os.MkdirAll(filepath.Dir(l.name), 0o755); err != nil {
			return err
		}
		// Remove an existing target (tar duplicates are rare but
		// alpine's rootfs has a few).
		_ = os.Remove(l.name)
		if err := os.Symlink(l.linkname, l.name); err != nil {
			return err
		}
	}
	return nil
}
