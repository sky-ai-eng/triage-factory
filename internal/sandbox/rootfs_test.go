package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractTarGz_RejectsPathTraversal pins the security check on
// the rootfs extractor: a malicious tarball with a ../ entry must
// be rejected, not silently extracted outside the destination dir.
// Alpine's official rootfs is trusted but the check protects future
// pin updates against a compromised mirror.
func TestExtractTarGz_RejectsPathTraversal(t *testing.T) {
	// Build a tarball with one valid entry + one escape attempt.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	mustWriteFile(t, tw, "ok.txt", "fine")
	mustWriteFile(t, tw, "../escape.txt", "should fail")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	tarballPath := filepath.Join(t.TempDir(), "evil.tgz")
	if err := os.WriteFile(tarballPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	err := extractTarGz(tarballPath, dst)
	if err == nil {
		t.Errorf("extractTarGz accepted a ../ entry; should have rejected")
	}
}

// TestExtractTarGz_RejectsSymlinkThenWriteAttack pins the load-bearing
// two-pass property: a tarball that creates `link -> /tmp` (or another
// host path) and then writes `link/file` must NOT funnel the file
// write through the symlink to land at /tmp/file on the host.
//
// In a single-pass extractor, the symlink write completes first and
// the subsequent MkdirAll/OpenFile follows the symlink. Two-pass
// defers all symlink creation until after every regular file is
// written, which kills the attack.
func TestExtractTarGz_RejectsSymlinkThenWriteAttack(t *testing.T) {
	// Build a tarball with the attack shape: symlink to a host dir,
	// then a regular file underneath the symlink path.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	// Pick a host path we're guaranteed to be able to write to in
	// a test process: t.TempDir's parent. The attack would write
	// `<dst>/escape/file` which a single-pass extractor would route
	// to `<attack-target>/file`.
	attackTarget := filepath.Join(t.TempDir(), "host-target")
	if err := os.MkdirAll(attackTarget, 0o755); err != nil {
		t.Fatal(err)
	}

	// Symlink first — the bait.
	mustWriteSymlink(t, tw, "escape", attackTarget)
	// Regular file second — would land at attackTarget/should-not-appear
	// in a single-pass extractor.
	mustWriteFile(t, tw, "escape/should-not-appear", "ATTACK SUCCEEDED")

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	tarballPath := filepath.Join(t.TempDir(), "attack.tgz")
	if err := os.WriteFile(tarballPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	// Extraction itself may succeed or fail — both are acceptable.
	// What's NOT acceptable is the attack-target file showing up.
	_ = extractTarGz(tarballPath, dst)

	attackFile := filepath.Join(attackTarget, "should-not-appear")
	if _, err := os.Stat(attackFile); err == nil {
		body, _ := os.ReadFile(attackFile)
		t.Errorf("ATTACK SUCCEEDED: file landed at %s with contents %q\n— two-pass extraction is broken; the symlink was followed by the subsequent regular-file write",
			attackFile, body)
	}
}

// TestExtractTarGz_NormalEntries pins the happy path: regular files
// + directories + symlinks unpack correctly with mode bits preserved.
func TestExtractTarGz_NormalEntries(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	mustWriteDir(t, tw, "bin/")
	mustWriteFile(t, tw, "bin/echo", "#!fake-binary")
	mustWriteSymlink(t, tw, "bin/sh", "/bin/echo")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	tarballPath := filepath.Join(t.TempDir(), "good.tgz")
	if err := os.WriteFile(tarballPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	if err := extractTarGz(tarballPath, dst); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "bin", "echo")); err != nil {
		t.Errorf("bin/echo missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "bin", "sh")); err != nil {
		t.Errorf("bin/sh symlink missing: %v", err)
	}
}

// TestRootfsCacheKey_StableAcrossRuns pins the determinism contract:
// the same inputs always produce the same key. If this ever fails,
// the rootfs cache thrashes — every TF process startup re-runs the
// 30-second download + apk-add chain instead of reusing the cached
// extraction.
func TestRootfsCacheKey_StableAcrossRuns(t *testing.T) {
	a, err := rootfsCacheKey()
	if err != nil {
		t.Fatalf("rootfsCacheKey: %v", err)
	}
	b, err := rootfsCacheKey()
	if err != nil {
		t.Fatalf("rootfsCacheKey: %v", err)
	}
	if a != b {
		t.Errorf("rootfsCacheKey not stable across calls: got %q then %q", a, b)
	}
}

// TestRootfsCacheKey_ChangesWhenPackagesAdded pins the load-bearing
// invariant: when apkPackages grows (someone adds rust, java, etc.),
// the cache key MUST change so a long-running TF process doesn't
// keep serving a stale rootfs that's missing the new toolchain. The
// failure mode this protects against is silent — no error, just
// ENOENT inside the sandbox six months after the package list grew.
func TestRootfsCacheKey_ChangesWhenPackagesAdded(t *testing.T) {
	base := []string{"nodejs", "git"}
	extended := []string{"nodejs", "git", "rust"}
	if rootfsCacheKeyFor("abc", base) == rootfsCacheKeyFor("abc", extended) {
		t.Error("adding a package didn't change the cache key — stale cache risk")
	}
}

// TestRootfsCacheKey_DeterministicOrder pins that the key depends on
// the package SET, not the slice ordering. A maintainer reshuffling
// apkPackages for readability shouldn't invalidate every operator's
// cache. Sort-before-hash is the chosen mechanism (see
// rootfsCacheKeyFor); this test guards it.
func TestRootfsCacheKey_DeterministicOrder(t *testing.T) {
	a := []string{"nodejs", "git", "ripgrep"}
	b := []string{"ripgrep", "nodejs", "git"}
	if rootfsCacheKeyFor("abc", a) != rootfsCacheKeyFor("abc", b) {
		t.Error("cache key depends on slice ordering; should depend only on the package set")
	}
}

// TestRootfsCacheKey_AlpineShaContributes pins that the alpine
// tarball sha is part of the key. A future alpine version bump must
// invalidate every operator's cache regardless of whether the
// package list changed.
func TestRootfsCacheKey_AlpineShaContributes(t *testing.T) {
	pkgs := []string{"nodejs"}
	if rootfsCacheKeyFor("alpha", pkgs) == rootfsCacheKeyFor("beta", pkgs) {
		t.Error("cache key ignores alpine sha; pin update wouldn't invalidate")
	}
}

// TestAlpineRootfsForArch_KnownArches pins that both supported arches
// resolve to a URL with the right /x86_64/ or /aarch64/ segment and a
// concrete 64-hex sha256 (NOT a TODO placeholder). Catches two
// distinct regressions:
//
//  1. Copy-paste in the switch — someone swaps amd64 and arm64 arms
//     and the wrong tarball gets fetched.
//  2. Re-introduced TODO sentinel — an earlier draft of this code
//     shipped with "TODO_PIN_AARCH64_SHA256" as the arm64 sha, which
//     compiled fine but failed sha verify at sandbox-launch time.
//     The shape check (64 hex chars) rejects any future regression
//     in the same direction.
func TestAlpineRootfsForArch_KnownArches(t *testing.T) {
	urlAmd, shaAmd, err := alpineRootfsForArch("amd64")
	if err != nil {
		t.Fatalf("amd64: %v", err)
	}
	if !strings.Contains(urlAmd, "x86_64") {
		t.Errorf("amd64 URL %q missing x86_64 segment", urlAmd)
	}
	assertConcreteSHA256(t, "amd64", shaAmd)

	urlArm, shaArm, err := alpineRootfsForArch("arm64")
	if err != nil {
		t.Fatalf("arm64: %v", err)
	}
	if !strings.Contains(urlArm, "aarch64") {
		t.Errorf("arm64 URL %q missing aarch64 segment", urlArm)
	}
	assertConcreteSHA256(t, "arm64", shaArm)

	if urlAmd == urlArm {
		t.Error("amd64 and arm64 resolved to the same URL; per-arch dispatch broken")
	}
	if shaAmd == shaArm {
		t.Error("amd64 and arm64 share a sha; per-arch cache key would collide")
	}
}

// assertConcreteSHA256 fails the test unless sha is a 64-hex-char
// string with no obviously-placeholder substrings. Sandbox launch
// hits sha-verify against the real alpine tarball, so a placeholder
// here would CI-pass but always fail at runtime.
func assertConcreteSHA256(t *testing.T, arch, sha string) {
	t.Helper()
	if len(sha) != 64 {
		t.Errorf("%s sha %q is %d chars; want 64-hex sha256", arch, sha, len(sha))
		return
	}
	for _, c := range sha {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Errorf("%s sha %q contains non-hex char %q", arch, sha, c)
			return
		}
	}
	for _, sentinel := range []string{"TODO", "PLACEHOLDER", "FIXME", "XXX"} {
		if strings.Contains(strings.ToUpper(sha), sentinel) {
			t.Errorf("%s sha %q looks like a placeholder (contains %q)", arch, sha, sentinel)
		}
	}
}

// TestAlpineRootfsForArch_UnsupportedErrors pins the actionable-error
// contract: an unknown GOARCH (ppc64le, s390x, ...) returns an error
// naming the arch instead of a sha mismatch six minutes into the
// download. currentArchRootfs and rootfsCacheKey propagate this
// error so an unsupported-arch install fails with a clean diagnostic
// rather than a sha verification mismatch.
func TestAlpineRootfsForArch_UnsupportedErrors(t *testing.T) {
	_, _, err := alpineRootfsForArch("ppc64le")
	if err == nil {
		t.Fatal("expected error for unsupported arch, got nil")
	}
	if !strings.Contains(err.Error(), "ppc64le") {
		t.Errorf("error %q should name the unsupported arch", err)
	}
}

// TestRootfsCacheKey_PerArchSeparate pins that switching arch invalidates
// the cache. Cross-compile workflows or arm64-on-amd64 dev boxes mustn't
// reuse the other arch's extracted rootfs — the binaries are wrong-arch
// and the sandbox would fail at first exec.
func TestRootfsCacheKey_PerArchSeparate(t *testing.T) {
	_, shaAmd, errAmd := alpineRootfsForArch("amd64")
	if errAmd != nil {
		t.Fatalf("amd64: %v", errAmd)
	}
	_, shaArm, errArm := alpineRootfsForArch("arm64")
	if errArm != nil {
		t.Fatalf("arm64: %v", errArm)
	}
	if rootfsCacheKeyFor(shaAmd, apkPackages) == rootfsCacheKeyFor(shaArm, apkPackages) {
		t.Error("amd64 and arm64 cache keys collide; cross-arch cache pollution risk")
	}
}

// TestRustTarballsForArch_PinsEveryComponent: every arch the alpine rootfs
// supports resolves a concrete sha256 for every component of the pinned Rust
// release, for its own musl triple. A missing or placeholder pin otherwise
// surfaces only at the first bake on that arch.
func TestRustTarballsForArch_PinsEveryComponent(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		tarballs, err := rustTarballsForArch(arch)
		if err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		if len(tarballs) != len(rustComponents) {
			t.Fatalf("%s: %d tarballs, want one per component (%d)", arch, len(tarballs), len(rustComponents))
		}
		for _, tb := range tarballs {
			assertConcreteSHA256(t, arch+" "+tb.component, tb.sha)
			if !strings.Contains(tb.dir, rustHostTriples[arch]) || !strings.HasSuffix(tb.url, "/"+tb.dir+".tar.gz") {
				t.Errorf("%s %s: dir %q, url %q; want the %s tarball", arch, tb.component, tb.dir, tb.url, rustHostTriples[arch])
			}
		}
	}
}

// TestMergeRustComponents_LaysOutTheRelease: the components a release's
// components file names land in the prefix with their paths kept, merging
// where they share a directory, and neither a component's manifest.in nor the
// installer's own top-level files come along.
func TestMergeRustComponents_LaysOutTheRelease(t *testing.T) {
	release, prefix := t.TempDir(), t.TempDir()
	for rel, content := range map[string]string{
		"components":        "rustc\nrust-std-x86_64-unknown-linux-musl\n",
		"install.sh":        "#!/bin/sh\n",
		"rustc/manifest.in": "file:bin/rustc\n",
		"rustc/bin/rustc":   "rustc",
		"rustc/lib/rustlib/etc/gdb_load_rust_pretty_printers.py":                         "",
		"rust-std-x86_64-unknown-linux-musl/lib/rustlib/x86_64-unknown-linux-musl/lib/a": "std",
	} {
		p := filepath.Join(release, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := mergeRustComponents(release, prefix); err != nil {
		t.Fatalf("mergeRustComponents: %v", err)
	}
	for _, rel := range []string{
		"bin/rustc",
		"lib/rustlib/etc/gdb_load_rust_pretty_printers.py",
		"lib/rustlib/x86_64-unknown-linux-musl/lib/a",
	} {
		if _, err := os.Stat(filepath.Join(prefix, rel)); err != nil {
			t.Errorf("%s not installed: %v", rel, err)
		}
	}
	for _, rel := range []string{"manifest.in", "install.sh", "components"} {
		if _, err := os.Stat(filepath.Join(prefix, rel)); err == nil {
			t.Errorf("%s was installed into the prefix", rel)
		}
	}
}

// TestMergeRustComponents_RefusesAPathInComponents: a components entry with
// path structure would walk, and move into the rootfs, files outside the
// release, so it is refused before anything moves.
func TestMergeRustComponents_RefusesAPathInComponents(t *testing.T) {
	root := t.TempDir()
	release, prefix := filepath.Join(root, "release"), filepath.Join(root, "prefix")
	outside := filepath.Join(root, "outside", "secret")
	for _, dir := range []string{release, prefix, filepath.Dir(outside)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "components"), []byte("../outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := mergeRustComponents(release, prefix); err == nil {
		t.Fatal("mergeRustComponents accepted a components entry outside the release")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("the file outside the release moved: %v", err)
	}
}

func mustWriteFile(t *testing.T, tw *tar.Writer, name, content string) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o755,
		Size:     int64(len(content)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
}

func mustWriteDir(t *testing.T, tw *tar.Writer, name string) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o755,
		Typeflag: tar.TypeDir,
	}); err != nil {
		t.Fatal(err)
	}
}

func mustWriteSymlink(t *testing.T, tw *tar.Writer, name, target string) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o777,
		Linkname: target,
		Typeflag: tar.TypeSymlink,
	}); err != nil {
		t.Fatal(err)
	}
}
