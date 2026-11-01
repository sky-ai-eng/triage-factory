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

// alpineVersion pins the alpine minirootfs release. The floor is 3.21:
// it is the first release whose nodejs apk package is new enough
// (>= 22.13) to run the pnpm release the frontend pins (see pnpmVersion)
// — 3.20 shipped node 20, on which that pnpm refuses to start. Bumping
// requires re-pinning both per-arch sha256s in alpineRootfsForArch and
// re-validating that the release still boots under gVisor with the
// toolchain reachable and nothing changed in the syscall surface the SDK
// depends on (the internal/sandbox integration suite, TestIntegration_*,
// exercises exactly that on real runsc).
const alpineVersion = "3.21.7"

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
			"8cba1ea3e8b500ea986a313d8eecf3d5952a2a0d23a69117bb81c023d9ceac05", nil
	case "arm64":
		return "https://dl-cdn.alpinelinux.org/alpine/v" + majorMinor(alpineVersion) +
				"/releases/aarch64/alpine-minirootfs-" + alpineVersion + "-aarch64.tar.gz",
			"d1d1a3fae5f4d6146e9742790a47fcb116199622cfb8439f218a4d5fbe5000da", nil
	default:
		return "", "", fmt.Errorf("sandbox: unsupported GOARCH %q (only amd64 and arm64 are pinned; add a case to alpineRootfsForArch to support more)", arch)
	}
}

// rustVersion pins the Rust release installed into the rootfs's /usr/local.
// It comes from upstream (static.rust-lang.org) rather than apk because
// every stable Alpine branch packages a Rust several releases behind
// upstream, and crates raise their minimum Rust fast enough that a
// repository's lockfile routinely refuses the packaged one. There is no
// rustup in the rootfs, so a repository's rust-toolchain.toml is not read
// and every build uses this release. Bumping it means re-pinning every sha
// in rustComponentSHA256 from the .sha256 file published beside each
// tarball; the version is folded into rootfsCacheKeyFor, so a bump re-bakes.
const rustVersion = "1.99.1"

// rustComponents are the parts of rustVersion's release the rootfs installs:
// the compiler and the host target's standard library, cargo, and the two
// linters an agent runs before it commits (cargo clippy, cargo fmt).
var rustComponents = []string{"rustc", "cargo", "rust-std", "clippy", "rustfmt"}

// rustHostTriples is the musl host target each supported GOARCH installs:
// the rootfs is Alpine, so the toolchain has to run on musl.
var rustHostTriples = map[string]string{
	"amd64": "x86_64-unknown-linux-musl",
	"arm64": "aarch64-unknown-linux-musl",
}

// rustComponentSHA256 pins each component tarball of rustVersion, by GOARCH.
var rustComponentSHA256 = map[string]map[string]string{
	"amd64": {
		"rustc":    "97e78bf9efd2896f176b15c9d380c9cb19479926693e320b0650a141e68e161f",
		"cargo":    "7ca474ad63165c5366ed3847c71aa0cdb60ba597f553bb415cd396c92023c8a5",
		"rust-std": "89703fc52323fad35169fcf649e56844b7dd62bf9dd9a4113bbcd96865690e36",
		"clippy":   "066f7517ab1be90a171e90fd6b731187d957b733f7090793f1f96b9e0a398f94",
		"rustfmt":  "9b09be3488330dbacae32ab2089d12c701565b7369d95217a342b2910cae120b",
	},
	"arm64": {
		"rustc":    "2f2e614d586b711913a39e7423d1cc2b58ff136da5e5c2de5eeda1b14864eeca",
		"cargo":    "cd14d5ddfe977c769f0446efb825133e76b611f3af2d2f8ee084ebff5ad4f314",
		"rust-std": "5202b22bf78e62f95304f6dfafd7e3e10ac839fb808e01cd12d66bbb99e7c1b8",
		"clippy":   "f9247ddded27ea21a3426ccbf5d9ad9f0d1da73a806449941cdc46f9bf931303",
		"rustfmt":  "b4348dcee85110175b9b46c23e093a33ff923380d4818d3967d8112e37b343d7",
	},
}

// rustTarball is one component of the pinned Rust release: the tarball's
// URL and sha256, and dir, the single top-level directory it extracts to.
type rustTarball struct {
	component string
	url       string
	sha       string
	dir       string
}

// rustTarballsForArch returns the component tarballs to install for arch, in
// rustComponents order. A missing pin is an error rather than a skipped
// component, so a toolchain that cannot run (rustc without its standard
// library) is never baked.
func rustTarballsForArch(arch string) ([]rustTarball, error) {
	triple, ok := rustHostTriples[arch]
	if !ok {
		return nil, fmt.Errorf("sandbox: no Rust toolchain pinned for GOARCH %q (add it to rustHostTriples and rustComponentSHA256)", arch)
	}
	tarballs := make([]rustTarball, 0, len(rustComponents))
	for _, c := range rustComponents {
		sha, ok := rustComponentSHA256[arch][c]
		if !ok {
			return nil, fmt.Errorf("sandbox: no sha256 pinned for Rust %s %s on GOARCH %q", rustVersion, c, arch)
		}
		dir := c + "-" + rustVersion + "-" + triple
		tarballs = append(tarballs, rustTarball{
			component: c,
			url:       "https://static.rust-lang.org/dist/" + dir + ".tar.gz",
			sha:       sha,
			dir:       dir,
		})
	}
	return tarballs, nil
}

// majorMinor strips the patch suffix off a semver-ish version string —
// "3.21.7" → "3.21" — for use in the alpine release-tree URL.
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
//   - nodejs/npm — developer toolchain for an agent working on a JS/TS
//     repo (npm install, npm test, node scripts). pnpm is not an apk
//     package, so installToolchain (rootfs_linux.go) npm-global-installs
//     the pinned pnpm on top at build time — see installPnpm and
//     pnpmVersion below.
//   - git — every delegate flow does status/diff/commit/push.
//   - ripgrep — agent's primary code-search tool; faster than grep
//     on large repos.
//   - bash — alpine ships ash by default; many shell scripts and
//     agent invocations assume bash.
//   - ca-certificates — the default trust store for in-sandbox TLS
//     clients. The trust variables a launch sets point them at the run's
//     CA bundle instead, which carries the egress interceptor's CA.
//   - python3 — common in build scripts and tooling glue. There is no
//     system pip: `python3 -m venv` installs pip from the wheel CPython
//     bundles, and the egress allowlist carries PyPI for it.
//   - go — Go projects need go build/test/mod tidy to verify changes.
//   - make — ubiquitous in build flows.
//   - curl — ubiquitous, tiny, expected.
//   - openssh-client — git over SSH and any tooling that calls ssh.
//   - openssh-keygen — ssh-keygen, which the githooks ssh-sign shim hands
//     every signature check to, so git verify-commit and git log
//     --show-signature work on a signing run's commits. Named rather than
//     left to openssh-client's dependencies, because the shim calls it
//     directly.
//   - build-base — gcc + make + musl-dev; required for native deps
//     (node-gyp, cgo, anything that compiles at install time).
var apkPackages = []string{
	"nodejs", "npm", "git", "ripgrep", "bash", "ca-certificates",
	"python3", "go", "make", "curl", "openssh-client", "openssh-keygen", "build-base",
}

// pnpmVersion pins the pnpm release that installToolchain bakes into
// the cached rootfs as an npm global (rootfs_linux.go's installPnpm),
// so a sandboxed agent building this repo's frontend finds pnpm already
// on PATH. Must match frontend/package.json's "packageManager" field —
// bump both together; because pnpm self-manages that field, an exact
// match runs the baked binary with zero network. Folded into
// rootfsCacheKeyFor's hash so a version bump invalidates existing
// operator caches the same way an apkPackages change does.
const pnpmVersion = "11.9.0"

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

// rootfsCacheKeyFor hashes (alpine_sha, sorted-package-set, pnpmVersion,
// rustVersion, rustComponents) and returns the first 12 hex chars. Sorting
// before hashing means the key depends on the package set, not on slice
// ordering — a future maintainer reshuffling apkPackages won't silently
// invalidate a cache that's actually still correct.
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
	h.Write([]byte(rustVersion))
	for _, c := range rustComponents {
		h.Write([]byte{0})
		h.Write([]byte(c))
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// mergeRustComponents installs one extracted Rust release tarball, release
// being its top-level directory, into prefix the way the tarball's own
// install.sh lays it out: every directory its components file names is moved
// into prefix with its relative paths kept, less the manifest.in that lists
// the component's files. Directories two components share (lib/rustlib)
// merge. The installer's script, licenses, and version files at the top level
// stay behind. release and prefix must be on one filesystem, since files move
// by rename.
func mergeRustComponents(release, prefix string) error {
	list, err := os.ReadFile(filepath.Join(release, "components"))
	if err != nil {
		return err
	}
	for _, name := range strings.Fields(string(list)) {
		// A name with any path structure would walk, and move into the
		// rootfs, files outside the release.
		if name == "." || name == ".." || name != filepath.Base(name) {
			return fmt.Errorf("components names %q, which is not a directory of the release", name)
		}
		src := filepath.Join(release, name)
		err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(src, path)
			if err != nil {
				return err
			}
			if rel == "manifest.in" {
				return nil
			}
			dst := filepath.Join(prefix, rel)
			if d.IsDir() {
				return os.MkdirAll(dst, 0o755)
			}
			return os.Rename(path, dst)
		})
		if err != nil {
			return fmt.Errorf("install component %s: %w", name, err)
		}
	}
	return nil
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
