package agentproc

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/egressrelay"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestBuildSandboxEnv_NoGitConfig enforces the invariant documented on
// buildSandboxEnv: the base sandbox env must never carry a GIT_CONFIG_*
// entry. The sandboxed git's config is delivered as one consolidated block
// by startProxiesForSandbox (core.hooksPath + proxy pairs) that owns
// GIT_CONFIG_COUNT and numbers from 0; a GIT_CONFIG_COUNT here would create
// a second count once sandbox.Wrap concatenates the two slices, making which
// one git reads platform-dependent. Asserted with both an empty extraEnv and
// a representative one so a future contributor who reaches for buildSandboxEnv
// to add a GIT_CONFIG_* var trips this immediately.
func TestBuildSandboxEnv_NoGitConfig(t *testing.T) {
	for _, extra := range [][]string{
		nil,
		{"TRIAGE_FACTORY_CONVERSATION_ID=r1", "TRIAGE_FACTORY_BLUEPRINT_RUN_ID=r1"},
	} {
		for _, kv := range buildSandboxEnv(extra) {
			if strings.HasPrefix(kv, "GIT_CONFIG_") {
				t.Errorf("buildSandboxEnv emitted %q; the base sandbox env must carry no GIT_CONFIG_* (see startProxiesForSandbox)", kv)
			}
		}
	}
}

// TestBuildSandboxEnv_CarriesSandboxMarker pins the "I am inside the jail"
// marker onto the base env, unconditionally and regardless of what the
// caller passes as ExtraEnv. Both jail shapes assemble their env here, so
// this is the single point that makes the marker's absence meaningful:
// cmd/exec/agenthost reads it to tell a missing exec-verb socket (an
// outage) apart from a local-mode CLI invocation (a mode signal), and a
// marker that were only sometimes set would make that read a coin flip.
// The last two cases are the ones that make the marker trustworthy rather
// than merely present: ExtraEnv is appended verbatim, so a caller that
// contributes its own marker entry would otherwise duplicate the key (which
// copy the reader sees is platform-dependent) or override the value, and the
// exact-match read would stop meaning what the assembler meant by it.
func TestBuildSandboxEnv_CarriesSandboxMarker(t *testing.T) {
	want := SandboxMarkerEnvVar + "=" + SandboxMarkerEnvValue
	for _, extra := range [][]string{
		nil,
		{"TRIAGE_FACTORY_CONVERSATION_ID=r1"},
		{SandboxMarkerEnvVar + "=" + SandboxMarkerEnvValue},
		{"TRIAGE_FACTORY_CONVERSATION_ID=r1", SandboxMarkerEnvVar + "=0"},
	} {
		var got []string
		for _, kv := range buildSandboxEnv(extra) {
			if strings.HasPrefix(kv, SandboxMarkerEnvVar+"=") {
				got = append(got, kv)
			}
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("buildSandboxEnv(%v) carries %v for %s; want exactly one entry, %q",
				extra, got, SandboxMarkerEnvVar, want)
		}
	}
}

// TestBuildSandboxEnv_CarriesGoToolchainAuto pins the Go version floor's
// escape hatch onto the base env. The rootfs's Go comes from alpine, whose
// go.env sets GOTOOLCHAIN=local; under that default a repo requiring a newer
// Go than the packaged one is simply unbuildable in the jail, because the
// agent is not root and no vendor download host is allowlisted. "auto" is
// what makes the floor self-healing, so its absence is a silent capability
// regression rather than a visible failure — hence a pin rather than a
// comment. Asserted across extraEnv variants because ExtraEnv is appended
// verbatim: a caller contributing its own GOTOOLCHAIN would otherwise
// duplicate the key, and which copy cmd/go reads is platform-dependent. The
// GOTOOLCHAIN=local cases are the ones that make the pin load-bearing rather
// than decorative — that is the value the rootfs's own go.env carries, so it
// is the one an inherited env is most likely to thread back in, and it is
// precisely the value that re-breaks the builds this entry exists to fix.
func TestBuildSandboxEnv_CarriesGoToolchainAuto(t *testing.T) {
	const want = "GOTOOLCHAIN=auto"
	for _, extra := range [][]string{
		nil,
		{"TRIAGE_FACTORY_CONVERSATION_ID=r1"},
		{"GOTOOLCHAIN=local"},
		{"TRIAGE_FACTORY_CONVERSATION_ID=r1", "GOTOOLCHAIN=local"},
		{"GOTOOLCHAIN=auto"},
		{"GOTOOLCHAIN=go1.21.0"},
	} {
		var got []string
		for _, kv := range buildSandboxEnv(extra) {
			if strings.HasPrefix(kv, "GOTOOLCHAIN=") {
				got = append(got, kv)
			}
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("buildSandboxEnv(%v) carries %v for GOTOOLCHAIN; want exactly one entry, %q",
				extra, got, want)
		}
	}
}

// TestBuildSandboxEnv_DropsCatalogEnvKeys pins the relay-key filter, and
// unlike the GOTOOLCHAIN case the ordering here is load-bearing: duplicate
// env keys resolve first-wins on Linux, and the relay's own GOPROXY is
// appended AFTER this function's output (run.go appends
// opts.PrebuiltProxyEnv last). So an inherited GOPROXY surviving into the
// output would shadow the relay's copy and point the jail's cmd/go at a
// host the allowlist doesn't carry. The base env carries no catalog key
// itself, so the correct post-filter count is zero — the relay's copy,
// arriving later, is then the only one. Driven off the catalog so a future
// entry's key is covered the moment it exists.
func TestBuildSandboxEnv_DropsCatalogEnvKeys(t *testing.T) {
	keys := egressrelay.CatalogEnvKeys()
	if len(keys) == 0 {
		t.Fatal("egressrelay.CatalogEnvKeys() is empty; expected at least GOPROXY")
	}
	for _, key := range keys {
		for _, extra := range [][]string{
			{key + "=direct"},
			{"TRIAGE_FACTORY_CONVERSATION_ID=r1", key + "=https://proxy.golang.org"},
		} {
			for _, kv := range buildSandboxEnv(extra) {
				if strings.HasPrefix(kv, key+"=") {
					t.Errorf("buildSandboxEnv(%v) emitted %q; catalog env keys must be dropped from ExtraEnv so the relay's later copy is the only one", extra, kv)
				}
			}
		}
	}
}

// TestBuildSandboxEnv_JSCJITDefaultOff pins the engine runtime tuning:
// the sandbox base env carries BUN_JSC_useJIT=0 by default (the memory
// win) and drops it when the operator opts back into the JIT.
func TestBuildSandboxEnv_JSCJITDefaultOff(t *testing.T) {
	contains := func(env []string, kv string) bool {
		for _, e := range env {
			if e == kv {
				return true
			}
		}
		return false
	}

	if !contains(buildSandboxEnv(nil), "BUN_JSC_useJIT=0") {
		t.Error("sandbox env missing BUN_JSC_useJIT=0; the JIT should be off by default")
	}

	t.Setenv("TF_AGENT_JSC_JIT", "1")
	for _, kv := range buildSandboxEnv(nil) {
		if strings.HasPrefix(kv, "BUN_JSC_") {
			t.Errorf("sandbox env carries %q despite TF_AGENT_JSC_JIT=1; the opt-out must restore the JIT", kv)
		}
	}
}

// TestAgentVisibleHelpers_LocalPassthrough exercises the un-sandboxed path
// (tests run in local mode, so shouldSandbox() is false): both helpers must
// return the host value unchanged. The sandbox branch ("/work" / the
// bind-mounted binary) is covered by the integration translate tests above and
// can't be unit-asserted here without forcing multi mode + a Linux host.
func TestAgentVisibleHelpers_LocalPassthrough(t *testing.T) {
	if got := AgentVisibleRoot("/data/worktrees/abc"); got != "/data/worktrees/abc" {
		t.Errorf("AgentVisibleRoot local passthrough: got %q want %q", got, "/data/worktrees/abc")
	}
	if got := AgentVisibleBinary("/home/user/bin/triagefactory"); got != "/home/user/bin/triagefactory" {
		t.Errorf("AgentVisibleBinary local passthrough: got %q want %q", got, "/home/user/bin/triagefactory")
	}
}

// TestWillSandbox_TracksGate confirms WillSandbox mirrors the internal gate
// (false in the default local-mode test environment).
func TestWillSandbox_TracksGate(t *testing.T) {
	if WillSandbox() != shouldSandbox() {
		t.Errorf("WillSandbox() = %v, want it to mirror shouldSandbox() = %v", WillSandbox(), shouldSandbox())
	}
}

// TestRefuseMultiModeSDKLoop pins the fail-closed gate Run and RunInteractive
// both call before spawning anything: multi mode refuses with
// errSDKLoopInMultiMode (the SDK loop has no isolation to offer there — its
// only wrapper on Linux is the bubblewrap courtesy sandbox, not a tenant
// boundary), and local mode — the SDK loop's only legitimate caller — is a
// no-op.
func TestRefuseMultiModeSDKLoop(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	if err := refuseMultiModeSDKLoop(); !errors.Is(err, errSDKLoopInMultiMode) {
		t.Errorf("refuseMultiModeSDKLoop() in multi mode = %v, want errSDKLoopInMultiMode", err)
	}

	runmode.SetForTest(t, runmode.ModeLocal)
	if err := refuseMultiModeSDKLoop(); err != nil {
		t.Errorf("refuseMultiModeSDKLoop() in local mode = %v, want nil", err)
	}
}

func TestTranslateAddDirsForSandbox(t *testing.T) {
	cases := []struct {
		name    string
		addDirs []string
		cwd     string
		want    []string
	}{
		{
			name:    "nil_input",
			addDirs: nil,
			cwd:     "/data/worktrees/abc",
			want:    nil,
		},
		{
			name:    "empty_cwd_drops_everything",
			addDirs: []string{"/some/path"},
			cwd:     "",
			want:    []string{},
		},
		{
			name:    "absolute_under_cwd_translates",
			addDirs: []string{"/data/worktrees/abc/knowledge-base"},
			cwd:     "/data/worktrees/abc",
			want:    []string{"/work/knowledge-base"},
		},
		{
			name:    "nested_subpath_preserved",
			addDirs: []string{"/data/worktrees/abc/repos/project1/src"},
			cwd:     "/data/worktrees/abc",
			want:    []string{"/work/repos/project1/src"},
		},
		{
			name:    "outside_cwd_dropped",
			addDirs: []string{"/etc/passwd"},
			cwd:     "/data/worktrees/abc",
			want:    []string{},
		},
		{
			name:    "mixed_in_and_out",
			addDirs: []string{"/data/worktrees/abc/kb", "/etc/passwd", "/data/worktrees/abc/repos"},
			cwd:     "/data/worktrees/abc",
			want:    []string{"/work/kb", "/work/repos"},
		},
		{
			name:    "empty_entries_skipped",
			addDirs: []string{"/data/worktrees/abc/kb", "", "/data/worktrees/abc/repos"},
			cwd:     "/data/worktrees/abc",
			want:    []string{"/work/kb", "/work/repos"},
		},
		{
			name:    "cwd_itself_becomes_work",
			addDirs: []string{"/data/worktrees/abc"},
			cwd:     "/data/worktrees/abc",
			want:    []string{"/work"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := translateAddDirsForSandbox(c.addDirs, c.cwd)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("translateAddDirsForSandbox(%v, %q) = %v, want %v",
					c.addDirs, c.cwd, got, c.want)
			}
		})
	}
}

func TestTranslateEnvForSandbox(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		cwd  string
		want []string
	}{
		{
			name: "nil_input",
			env:  nil,
			cwd:  "/data/worktrees/abc",
			want: nil,
		},
		{
			name: "non_path_values_passthrough",
			env: []string{
				"TRIAGE_FACTORY_CONVERSATION_ID=abc-123",
				"TRIAGE_FACTORY_REPO=owner/repo",
			},
			cwd: "/data/worktrees/abc",
			want: []string{
				"TRIAGE_FACTORY_CONVERSATION_ID=abc-123",
				"TRIAGE_FACTORY_REPO=owner/repo",
			},
		},
		{
			name: "abs_path_under_cwd_translates",
			env:  []string{"TRIAGE_FACTORY_CONVERSATION_ROOT=/data/worktrees/abc"},
			cwd:  "/data/worktrees/abc",
			want: []string{"TRIAGE_FACTORY_CONVERSATION_ROOT=/work"},
		},
		{
			name: "abs_subpath_under_cwd_translates",
			env:  []string{"TRIAGE_FACTORY_CONVERSATION_ROOT=/data/worktrees/abc/_tfac"},
			cwd:  "/data/worktrees/abc",
			want: []string{"TRIAGE_FACTORY_CONVERSATION_ROOT=/work/_tfac"},
		},
		{
			name: "abs_path_outside_cwd_dropped",
			env:  []string{"JAVA_HOME=/usr/lib/jvm/openjdk"},
			cwd:  "/data/worktrees/abc",
			want: []string{},
		},
		{
			name: "mixed_keep_translate_drop",
			env: []string{
				"TRIAGE_FACTORY_CONVERSATION_ID=abc-123",
				"TRIAGE_FACTORY_CONVERSATION_ROOT=/data/worktrees/abc",
				"JAVA_HOME=/usr/lib/jvm/openjdk",
			},
			cwd: "/data/worktrees/abc",
			want: []string{
				"TRIAGE_FACTORY_CONVERSATION_ID=abc-123",
				"TRIAGE_FACTORY_CONVERSATION_ROOT=/work",
			},
		},
		{
			name: "empty_cwd_drops_abs_paths_keeps_others",
			env: []string{
				"TRIAGE_FACTORY_CONVERSATION_ID=abc-123",
				"TRIAGE_FACTORY_CONVERSATION_ROOT=/data/worktrees/abc",
			},
			cwd: "",
			want: []string{
				"TRIAGE_FACTORY_CONVERSATION_ID=abc-123",
			},
		},
		{
			name: "malformed_no_equals_passthrough",
			env:  []string{"NOT_A_VALID_ENTRY"},
			cwd:  "/data/worktrees/abc",
			want: []string{"NOT_A_VALID_ENTRY"},
		},
		{
			name: "empty_value_passthrough",
			env:  []string{"TRIAGE_FACTORY_CONVERSATION_ROOT="},
			cwd:  "/data/worktrees/abc",
			want: []string{"TRIAGE_FACTORY_CONVERSATION_ROOT="},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := translateEnvForSandbox(c.env, c.cwd)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("translateEnvForSandbox(%v, %q) = %v, want %v",
					c.env, c.cwd, got, c.want)
			}
		})
	}
}

// TestTranslateAddDirsForSandbox_DropsSymlinkEscapes pins the
// defensive drop for paths that look in-tree but resolve outside.
// Pure filepath.Rel-level check — doesn't follow symlinks (the
// sandbox bind-mount handles those at the filesystem boundary).
func TestTranslateAddDirsForSandbox_DropsSymlinkEscapes(t *testing.T) {
	// `..` in the input — filepath.Rel would resolve outside cwd.
	got := translateAddDirsForSandbox(
		[]string{"/data/worktrees/abc/../other-worktree/sneaky"},
		"/data/worktrees/abc",
	)
	if len(got) != 0 {
		t.Errorf("got %v, want empty (the path resolves outside cwd)", got)
	}
}
