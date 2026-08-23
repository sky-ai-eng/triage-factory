package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Two real (non-sentinel) org IDs for the multi-mode + isolation checks.
// Neither equals runmode.LocalDefaultOrgID, so OrgRoot does NOT strip
// their segment in multi mode.
const (
	realOrg  = "11111111-1111-1111-1111-111111111111"
	otherOrg = "22222222-2222-2222-2222-222222222222"
)

func TestStateRoot_LocalDefault(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	t.Setenv(envStateRoot, "") // local ignores it anyway, but be explicit
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir on this host: %v", err)
	}
	want := filepath.Join(home, dirName)
	if got := StateRoot(); got != want {
		t.Errorf("StateRoot() = %q, want %q", got, want)
	}
}

func TestStateRoot_SetForTestOverrideWins(t *testing.T) {
	// Override wins over the mode default even in multi mode + env set.
	runmode.SetForTest(t, runmode.ModeMulti)
	t.Setenv(envStateRoot, "/mnt/ignored")
	SetForTest(t, "/tmp/tf-test-root")
	if got := StateRoot(); got != "/tmp/tf-test-root" {
		t.Errorf("StateRoot() = %q, want the SetForTest override", got)
	}
}

func TestStateRoot_SetForTestRestores(t *testing.T) {
	before := StateRoot()
	t.Run("inner-override", func(t *testing.T) {
		SetForTest(t, "/tmp/inner-root")
		if got := StateRoot(); got != "/tmp/inner-root" {
			t.Errorf("StateRoot() = %q during override", got)
		}
	})
	if after := StateRoot(); after != before {
		t.Errorf("StateRoot() = %q after subtest, want restored %q", after, before)
	}
}

func TestStateRoot_MultiDefault(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	t.Setenv(envStateRoot, "")
	if got := StateRoot(); got != defaultMultiStateRoot {
		t.Errorf("StateRoot() = %q, want %q", got, defaultMultiStateRoot)
	}
}

func TestStateRoot_MultiHonorsEnv(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	t.Setenv(envStateRoot, "/mnt/tfstate")
	if got := StateRoot(); got != "/mnt/tfstate" {
		t.Errorf("StateRoot() = %q, want /mnt/tfstate", got)
	}
}

func TestStateRoot_LocalIgnoresEnv(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	t.Setenv(envStateRoot, "/mnt/should-be-ignored")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir on this host: %v", err)
	}
	want := filepath.Join(home, dirName)
	if got := StateRoot(); got != want {
		t.Errorf("StateRoot() = %q, want %q (local ignores %s)", got, want, envStateRoot)
	}
}

func TestStateRootErr_MatchesStateRoot(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	SetForTest(t, "/s")
	got, err := StateRootErr()
	if err != nil {
		t.Fatalf("StateRootErr() error = %v", err)
	}
	if got != "/s" || got != StateRoot() {
		t.Errorf("StateRootErr() = %q, want /s == StateRoot()", got)
	}
}

func TestStateRootErr_MultiHasNoHomeDependency(t *testing.T) {
	// Multi mode resolves from env/default and never consults the home
	// dir, so an unresolvable $HOME is irrelevant there.
	runmode.SetForTest(t, runmode.ModeMulti)
	t.Setenv("HOME", "")
	t.Setenv(envStateRoot, "/mnt/vol")
	got, err := StateRootErr()
	if err != nil || got != "/mnt/vol" {
		t.Errorf("StateRootErr() = (%q, %v), want (/mnt/vol, nil)", got, err)
	}
}

func TestStateRootErr_ErrorsWhenHomeUnresolvable(t *testing.T) {
	// No SetForTest override, local mode, $HOME unset → os.UserHomeDir
	// fails and StateRootErr surfaces it instead of panicking. Skipped on
	// Windows, where os.UserHomeDir reads %USERPROFILE% rather than $HOME,
	// so an empty $HOME wouldn't force the failure under test.
	if runtime.GOOS == "windows" {
		t.Skip("os.UserHomeDir is USERPROFILE-based on Windows, not $HOME")
	}
	runmode.SetForTest(t, runmode.ModeLocal)
	t.Setenv("HOME", "")
	if got, err := StateRootErr(); err == nil {
		t.Errorf("StateRootErr() = (%q, nil), want an error with $HOME unset", got)
	}
}

func TestStateRoot_PanicsWhenHomeUnresolvable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.UserHomeDir is USERPROFILE-based on Windows, not $HOME")
	}
	runmode.SetForTest(t, runmode.ModeLocal)
	t.Setenv("HOME", "")
	defer func() {
		if r := recover(); r == nil {
			t.Error("StateRoot() did not panic with $HOME unset")
		}
	}()
	_ = StateRoot()
}

func TestOrgRoot_LocalStripsOrgSegment(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	SetForTest(t, "/s")
	// Local mode strips the org segment for EVERY org, real or sentinel.
	if got := OrgRoot(realOrg); got != "/s" {
		t.Errorf("OrgRoot(real) = %q, want /s (local strips org)", got)
	}
	if got := OrgRoot(runmode.LocalDefaultOrgID); got != "/s" {
		t.Errorf("OrgRoot(default) = %q, want /s", got)
	}
}

func TestOrgRoot_MultiRealOrg(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	SetForTest(t, "/s")
	want := filepath.Join("/s", "orgs", realOrg)
	if got := OrgRoot(realOrg); got != want {
		t.Errorf("OrgRoot(real) = %q, want %q", got, want)
	}
}

func TestOrgRoot_MultiDefaultSentinelStripped(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	SetForTest(t, "/s")
	// The local-default sentinel strips even in multi, so callers that
	// aren't org-threaded yet keep the un-scoped path.
	if got := OrgRoot(runmode.LocalDefaultOrgID); got != "/s" {
		t.Errorf("OrgRoot(default) in multi = %q, want /s (sentinel stripped)", got)
	}
}

// TestResolvers_LocalLayout pins the byte-for-byte on-disk layout an
// existing local install relies on: no org segment anywhere, every
// subtree directly under the state root.
func TestResolvers_LocalLayout(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	SetForTest(t, "/s")
	t.Setenv(envToolchainRoot, "") // hermetic: ignore any ambient TF_TOOLCHAIN_ROOT
	cases := []struct{ name, got, want string }{
		{"StateRoot", StateRoot(), "/s"},
		{"OrgRoot", OrgRoot(realOrg), "/s"},
		{"BareCacheRoot", BareCacheRoot(realOrg), filepath.Join("/s", "repos")},
		{"BareCacheDir", BareCacheDir(realOrg, "octo", "cat"), filepath.Join("/s", "repos", "octo", "cat.git")},
		{"SandboxRootfsDir", SandboxRootfsDir("abc"), filepath.Join("/s", "sandbox", "rootfs-abc")},
		{"SDKDir", SDKDir(), filepath.Join("/s", "sdk")},
		{"DBPath", DBPath(), filepath.Join("/s", "triagefactory.db")},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// TestResolvers_MultiLayout asserts the org segment lands on the Class-1
// resolvers and stays OFF the Class-2/3 resolvers in multi mode.
func TestResolvers_MultiLayout(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	SetForTest(t, "/s")
	t.Setenv(envToolchainRoot, "") // hermetic: ignore any ambient TF_TOOLCHAIN_ROOT
	orgBase := filepath.Join("/s", "orgs", realOrg)
	cases := []struct{ name, got, want string }{
		// Class 1 — org-scoped.
		{"OrgRoot", OrgRoot(realOrg), orgBase},
		{"BareCacheRoot", BareCacheRoot(realOrg), filepath.Join(orgBase, "repos")},
		{"BareCacheDir", BareCacheDir(realOrg, "octo", "cat"), filepath.Join(orgBase, "repos", "octo", "cat.git")},
		// Class 2/3 — host-global / local, NO org segment.
		{"SandboxRootfsDir", SandboxRootfsDir("abc"), filepath.Join("/s", "sandbox", "rootfs-abc")},
		{"SDKDir", SDKDir(), filepath.Join("/s", "sdk")},
		{"DBPath", DBPath(), filepath.Join("/s", "triagefactory.db")},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// TestToolchainRoot_EnvOverrideDecouplesFromStateRoot pins the fix for
// the multi-mode container state-root mismatch: when TF_TOOLCHAIN_ROOT is
// set (the runtime image does this), the Class-2 toolchain resolvers land
// on that fixed path regardless of StateRoot — so an image-baked SDK is
// never shadowed by wherever TF_STATE_ROOT points tenant data.
func TestToolchainRoot_EnvOverrideDecouplesFromStateRoot(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	SetForTest(t, "/data")
	t.Setenv(envToolchainRoot, "/opt/triagefactory")

	if got := ToolchainRoot(); got != "/opt/triagefactory" {
		t.Errorf("ToolchainRoot() = %q, want the TF_TOOLCHAIN_ROOT override", got)
	}
	if got, want := SDKDir(), filepath.Join("/opt/triagefactory", "sdk"); got != want {
		t.Errorf("SDKDir() = %q, want %q (decoupled from StateRoot)", got, want)
	}
	if got, want := SandboxRootfsDir("abc"), filepath.Join("/opt/triagefactory", "sandbox", "rootfs-abc"); got != want {
		t.Errorf("SandboxRootfsDir() = %q, want %q", got, want)
	}
	// StateRoot (tenant data) is untouched by the toolchain override.
	if got := StateRoot(); got != "/data" {
		t.Errorf("StateRoot() = %q, want /data (unaffected by TF_TOOLCHAIN_ROOT)", got)
	}
}

// TestToolchainRoot_UnsetFollowsStateRoot is the back-compat guarantee:
// with no override the toolchain lives under the state root exactly as it
// did before the split (every laptop, and host-binary multi-mode dev runs),
// so existing on-disk layouts are unchanged.
func TestToolchainRoot_UnsetFollowsStateRoot(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	SetForTest(t, "/s")
	t.Setenv(envToolchainRoot, "")

	if got := ToolchainRoot(); got != "/s" {
		t.Errorf("ToolchainRoot() = %q, want StateRoot when env unset", got)
	}
	if got, want := SDKDir(), filepath.Join("/s", "sdk"); got != want {
		t.Errorf("SDKDir() = %q, want %q", got, want)
	}
}

// TestToolchainRootErr_MatchesToolchainRoot confirms the error-returning
// form agrees with the error-free form, and that an explicit
// TF_TOOLCHAIN_ROOT resolves with no error (no $HOME dependency) — which
// is what lets EnsureSDK / the rootfs extractor pre-flight it safely in
// the container regardless of $HOME.
func TestToolchainRootErr_MatchesToolchainRoot(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	SetForTest(t, "/s")

	t.Setenv(envToolchainRoot, "/opt/triagefactory")
	if got, err := ToolchainRootErr(); err != nil || got != "/opt/triagefactory" {
		t.Errorf("ToolchainRootErr() = (%q, %v), want (/opt/triagefactory, nil)", got, err)
	}

	t.Setenv(envToolchainRoot, "")
	if got, err := ToolchainRootErr(); err != nil || got != "/s" {
		t.Errorf("ToolchainRootErr() = (%q, %v), want (/s, nil)", got, err)
	}
}

// TestMultiOrg_Isolation is the core multi-tenant guarantee: two orgs
// running simultaneously get non-overlapping org-scoped paths, while the
// host-global caches stay identical regardless of org.
func TestMultiOrg_Isolation(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	SetForTest(t, "/s")
	t.Setenv(envToolchainRoot, "") // hermetic: the host-global asserts below go through ToolchainRoot

	if a, b := BareCacheDir(realOrg, "o", "r"), BareCacheDir(otherOrg, "o", "r"); a == b {
		t.Errorf("BareCacheDir must differ across orgs, both = %q", a)
	}
	// Each org's clone actually lives under its own /orgs/<id>/ subtree.
	if got := BareCacheDir(realOrg, "o", "r"); !strings.Contains(got, filepath.Join("orgs", realOrg)) {
		t.Errorf("BareCacheDir(%q) = %q, want it under the org subtree", realOrg, got)
	}

	// Host-global caches must be identical for the two orgs (they take no
	// orgID, so this guards against a future StateRoot→OrgRoot slip).
	if SandboxRootfsDir("k") != filepath.Join("/s", "sandbox", "rootfs-k") {
		t.Errorf("SandboxRootfsDir leaked an org segment: %q", SandboxRootfsDir("k"))
	}
	if SDKDir() != filepath.Join("/s", "sdk") {
		t.Errorf("SDKDir leaked an org segment: %q", SDKDir())
	}
}

func TestExpandHome(t *testing.T) {
	// Pin HOME to a temp dir so the expectations are hermetic rather than
	// dependent on the developer's real home.
	home := t.TempDir()
	t.Setenv("HOME", home)
	cases := []struct{ in, want string }{
		{"~", home},
		{"~/.local/bin/triagefactory", filepath.Join(home, ".local", "bin", "triagefactory")},
		{"/usr/local/bin/triagefactory", "/usr/local/bin/triagefactory"},
		{"relative/path", "relative/path"},
		{"", ""},
		{"~user/not-expanded", "~user/not-expanded"}, // only bare ~ and ~/ expand
	}
	for _, c := range cases {
		got, err := ExpandHome(c.in)
		if err != nil {
			t.Errorf("ExpandHome(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ExpandHome(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
