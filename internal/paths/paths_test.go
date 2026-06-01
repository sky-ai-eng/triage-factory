package paths

import (
	"os"
	"path/filepath"
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
	cases := []struct{ name, got, want string }{
		{"StateRoot", StateRoot(), "/s"},
		{"OrgRoot", OrgRoot(realOrg), "/s"},
		{"BareCacheRoot", BareCacheRoot(realOrg), filepath.Join("/s", "repos")},
		{"BareCacheDir", BareCacheDir(realOrg, "octo", "cat"), filepath.Join("/s", "repos", "octo", "cat.git")},
		{"ProjectsRoot", ProjectsRoot(realOrg), filepath.Join("/s", "projects")},
		{"ProjectKBDir", ProjectKBDir(realOrg, "p1"), filepath.Join("/s", "projects", "p1")},
		{"SandboxRootfsDir", SandboxRootfsDir("abc"), filepath.Join("/s", "sandbox", "rootfs-abc")},
		{"SDKDir", SDKDir(), filepath.Join("/s", "sdk")},
		{"DBPath", DBPath(), filepath.Join("/s", "triagefactory.db")},
		{"TakeoversRoot", TakeoversRoot(), filepath.Join("/s", "takeovers")},
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
	orgBase := filepath.Join("/s", "orgs", realOrg)
	cases := []struct{ name, got, want string }{
		// Class 1 — org-scoped.
		{"OrgRoot", OrgRoot(realOrg), orgBase},
		{"BareCacheRoot", BareCacheRoot(realOrg), filepath.Join(orgBase, "repos")},
		{"BareCacheDir", BareCacheDir(realOrg, "octo", "cat"), filepath.Join(orgBase, "repos", "octo", "cat.git")},
		{"ProjectsRoot", ProjectsRoot(realOrg), filepath.Join(orgBase, "projects")},
		{"ProjectKBDir", ProjectKBDir(realOrg, "p1"), filepath.Join(orgBase, "projects", "p1")},
		// Class 2/3 — host-global / local, NO org segment.
		{"SandboxRootfsDir", SandboxRootfsDir("abc"), filepath.Join("/s", "sandbox", "rootfs-abc")},
		{"SDKDir", SDKDir(), filepath.Join("/s", "sdk")},
		{"DBPath", DBPath(), filepath.Join("/s", "triagefactory.db")},
		{"TakeoversRoot", TakeoversRoot(), filepath.Join("/s", "takeovers")},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// TestMultiOrg_Isolation is the core multi-tenant guarantee: two orgs
// running simultaneously get non-overlapping org-scoped paths, while the
// host-global caches stay identical regardless of org.
func TestMultiOrg_Isolation(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	SetForTest(t, "/s")

	if a, b := BareCacheDir(realOrg, "o", "r"), BareCacheDir(otherOrg, "o", "r"); a == b {
		t.Errorf("BareCacheDir must differ across orgs, both = %q", a)
	}
	if a, b := ProjectKBDir(realOrg, "p"), ProjectKBDir(otherOrg, "p"); a == b {
		t.Errorf("ProjectKBDir must differ across orgs, both = %q", a)
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
