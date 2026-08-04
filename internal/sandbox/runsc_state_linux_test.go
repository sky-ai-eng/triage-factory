//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// deleteRecorder captures the container ids a stand-in `runsc delete` was
// invoked for, in order. The pre-launch delete runs on the caller's
// goroutine and the reap-time one on whichever goroutine drives Wait, so the
// record is mutex-guarded.
type deleteRecorder struct {
	mu  sync.Mutex
	ids []string
}

func (r *deleteRecorder) record(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
}

func (r *deleteRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

// stubRuntimeDelete points buildRuntimeDeleteCmd at a stand-in built by
// mkCmd, so every delete-force in this package can be observed (and its exit
// status scripted) on a host with no gVisor installed.
func stubRuntimeDelete(t *testing.T, mkCmd func(ctx context.Context, containerID string) *exec.Cmd) *deleteRecorder {
	t.Helper()
	rec := &deleteRecorder{}
	orig := buildRuntimeDeleteCmd
	buildRuntimeDeleteCmd = func(ctx context.Context, containerID string) *exec.Cmd {
		rec.record(containerID)
		return mkCmd(ctx, containerID)
	}
	t.Cleanup(func() { buildRuntimeDeleteCmd = orig })
	return rec
}

// okDelete is the stand-in for a delete that found something and removed it.
func okDelete(ctx context.Context, _ string) *exec.Cmd {
	return exec.CommandContext(ctx, "true")
}

// TestNewRunscDeleteCommand_ForcesAndTargetsTheID pins the invocation shape:
// --force (state left by a SIGKILLed runsc still reads as "running", which a
// plain delete refuses) and the container id as the operand.
func TestNewRunscDeleteCommand_ForcesAndTargetsTheID(t *testing.T) {
	cmd := newRunscDeleteCommand(context.Background(), "tf-abc-3")
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, " delete ") {
		t.Fatalf("not a delete invocation: %v", cmd.Args)
	}
	if !strings.Contains(args, "--force") && !strings.Contains(args, "-force") {
		t.Errorf("delete is not forced; state from a killed runsc would survive it: %v", cmd.Args)
	}
	if cmd.Args[len(cmd.Args)-1] != "tf-abc-3" {
		t.Errorf("container id is not the operand: %v", cmd.Args)
	}
	// --root is what decides where runsc keeps state, and neither this nor
	// the run command sets it. A delete that pointed at a different root
	// would silently clear nothing.
	if strings.Contains(args, "-root") {
		t.Errorf("delete sets --root but the launch does not; they would resolve different state roots: %v", cmd.Args)
	}
}

func TestDeleteRuntimeState_Succeeds(t *testing.T) {
	rec := stubRuntimeDelete(t, okDelete)
	if err := DeleteRuntimeState(context.Background(), "tf-abc-3"); err != nil {
		t.Fatalf("DeleteRuntimeState: %v", err)
	}
	if got := rec.seen(); len(got) != 1 || got[0] != "tf-abc-3" {
		t.Errorf("deleted ids = %v, want [tf-abc-3]", got)
	}
}

// TestDeleteRuntimeState_IgnoresUnknownContainer is the pre-launch case: the
// overwhelmingly common outcome is that there was nothing to clear, and that
// must not read as a failure anyone logs about.
func TestDeleteRuntimeState_IgnoresUnknownContainer(t *testing.T) {
	for _, msg := range []string{
		`loading container: container "tf-abc-3" does not exist`,
		`container tf-abc-3 not found`,
		`open /var/run/runsc/tf-abc-3.state: no such file or directory`,
	} {
		t.Run(msg, func(t *testing.T) {
			stubRuntimeDelete(t, func(ctx context.Context, _ string) *exec.Cmd {
				return exec.CommandContext(ctx, "sh", "-c", "echo "+shQuote(msg)+" >&2; exit 1")
			})
			if err := DeleteRuntimeState(context.Background(), "tf-abc-3"); err != nil {
				t.Errorf("DeleteRuntimeState on an absent container = %v, want nil", err)
			}
		})
	}
}

// TestDeleteRuntimeState_ReportsRealFailures proves the other half: a delete
// that failed for a reason other than "nothing there" is surfaced, since
// that is a launch about to collide with state nobody cleared.
func TestDeleteRuntimeState_ReportsRealFailures(t *testing.T) {
	stubRuntimeDelete(t, func(ctx context.Context, _ string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "echo 'permission denied' >&2; exit 1")
	})
	err := DeleteRuntimeState(context.Background(), "tf-abc-3")
	if err == nil {
		t.Fatal("DeleteRuntimeState swallowed a real delete failure")
	}
	if !strings.Contains(err.Error(), "tf-abc-3") || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error names neither the container nor the cause: %v", err)
	}
}

// TestDeleteRuntimeState_NoRuntimeInstalled covers the local-mode and
// unit-test host: no runsc means no state root and nothing to clear, so the
// PATH lookup failure exec.Command records must not become an error every
// launch logs.
func TestDeleteRuntimeState_NoRuntimeInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := DeleteRuntimeState(context.Background(), "tf-abc-3"); err != nil {
		t.Errorf("DeleteRuntimeState with no runsc on PATH = %v, want nil", err)
	}
}

func TestDeleteRuntimeState_EmptyIDIsANoop(t *testing.T) {
	rec := stubRuntimeDelete(t, okDelete)
	if err := DeleteRuntimeState(context.Background(), ""); err != nil {
		t.Fatalf("DeleteRuntimeState(\"\"): %v", err)
	}
	if got := rec.seen(); len(got) != 0 {
		t.Errorf("built a delete for an empty id: %v", got)
	}
}

// TestContainerIDFromRuntimeStateEntry is the collateral-damage guard for the
// boot sweep, which delete-forces whatever this matches. It must recognize
// both on-disk layouts gVisor has used and nothing else.
func TestContainerIDFromRuntimeStateEntry(t *testing.T) {
	cases := []struct {
		name string
		want string
		ok   bool
	}{
		// The per-container state file, current layout.
		{"tf-abc123def-3_sandbox:tf-abc123def-3.state", "tf-abc123def-3", true},
		{"tf-abc123def-3_sandbox:tf-abc123def-3.lock", "tf-abc123def-3", true},
		// A directory named for the container id alone, older layout.
		{"tf-abc123def-3", "tf-abc123def-3", true},
		{"tf-abc123def-3.state", "tf-abc123def-3", true},
		// Real ids: a truncated uuid fragment keeps its hyphens, and a
		// free-form TraceID (scorer-batch) is not hex.
		{"tf-550e8400-e2-11.state", "tf-550e8400-e2-11", true},
		{"tf-scorer-batc-0", "tf-scorer-batc-0", true},
		// Not ours: no tf- prefix, no subnet index, or someone else's
		// tf-shaped container.
		{"docker-abc-1.state", "", false},
		{"tf-someoneelses-app", "", false},
		{"tf-abc123def", "", false},
		{"abc123def-3", "", false},
		{"", "", false},
		// A subnet index is one byte in decimal; four digits is not one.
		{"tf-abc-1234", "", false},
		// The sidecar's registry key is not a runsc container at all.
		{"tf-abc123def-3-sc", "", false},
		// Longer than any id this package can mint (the fragment is a RunID
		// cut to containerIDRunFragmentMax), so it belongs to someone else's
		// runsc — matching it would delete a container we do not own.
		{"tf-thisfragmentiswaytoolong-3", "", false},
		{"tf-thisfragmentiswaytoolong-3_sandbox:tf-thisfragmentiswaytoolong-3.state", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := containerIDFromRuntimeStateEntry(tc.name)
			if ok != tc.ok || got != tc.want {
				t.Errorf("containerIDFromRuntimeStateEntry(%q) = (%q, %v), want (%q, %v)", tc.name, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestRunscStateRoot_MirrorsGVisorDefault pins the resolution the sweep
// depends on: runsc reads XDG_RUNTIME_DIR for its rootless default, and the
// sweep has to look where the launches actually wrote.
func TestRunscStateRoot_MirrorsGVisorDefault(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/xdg")
	if got, want := runscStateRoot(), "/xdg/runsc"; got != want {
		t.Errorf("runscStateRoot with XDG_RUNTIME_DIR = %q, want %q", got, want)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if got, want := runscStateRoot(), "/var/run/runsc"; got != want {
		t.Errorf("runscStateRoot without XDG_RUNTIME_DIR = %q, want %q", got, want)
	}
}

// TestReapOrphanRuntimeState_ClearsOurStateOnly simulates a crashed executor:
// state entries on disk with no process behind them. The sweep must clear
// every tf-* container once, and touch nothing else in a state root it
// shares with whatever else on the host runs runsc.
func TestReapOrphanRuntimeState_ClearsOurStateOnly(t *testing.T) {
	root := stubRuntimeStateRoot(t)
	writeStateEntries(t, root,
		"tf-abc123def-3_sandbox:tf-abc123def-3.state",
		"tf-abc123def-3_sandbox:tf-abc123def-3.lock", // same container, second entry
		"tf-550e8400-e2-11.state",
		"someone-elses.state",
		"tf-someoneelses-app",
	)
	rec := stubRuntimeDelete(t, okDelete)

	reapOrphanRuntimeState(context.Background())

	got := rec.seen()
	want := []string{"tf-abc123def-3", "tf-550e8400-e2-11"}
	if len(got) != len(want) {
		t.Fatalf("deleted %v, want exactly %v (one delete per container, nothing else touched)", got, want)
	}
	for _, id := range want {
		if !contains(got, id) {
			t.Errorf("orphan state for %s was not reaped: deleted %v", id, got)
		}
	}
}

// TestReapOrphanRuntimeState_NoStateRoot: a host that has never run runsc
// has no state root, which is not a failure and not something to log about.
func TestReapOrphanRuntimeState_NoStateRoot(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) // no runsc/ underneath it
	rec := stubRuntimeDelete(t, okDelete)

	reapOrphanRuntimeState(context.Background())

	if got := rec.seen(); len(got) != 0 {
		t.Errorf("swept a state root that does not exist: %v", got)
	}
}

// stubRuntimeStateRoot points runscStateRoot at a writable temp dir by the
// same env var gVisor itself reads, and returns it.
func stubRuntimeStateRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	root := filepath.Join(dir, "runsc")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir state root: %v", err)
	}
	return root
}

func writeStateEntries(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(root, n), []byte("{}"), 0o600); err != nil {
			t.Fatalf("write state entry %s: %v", n, err)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// shQuote single-quotes s for the `sh -c` stand-ins above.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TestTFContainerIDRE_TracksTheMint is the anti-drift pin between the id the
// sweep will delete-force and the id Wrap actually mints. It builds ids with
// the mint's own formula, so widening truncate's bound without widening the
// matcher (or the reverse) fails here rather than silently stranding our
// state — or, worse, reaching state we do not own.
func TestTFContainerIDRE_TracksTheMint(t *testing.T) {
	mint := func(runID string, idx uint8) string {
		return fmt.Sprintf("tf-%s-%d", truncate(runID, containerIDRunFragmentMax), idx)
	}
	for _, runID := range []string{
		"550e8400-e29b-41d4-a716-446655440000", // a conversation uuid, truncated
		"scorer-batch",                         // a fixed TraceID
		"a",                                    // the shortest thing a caller can pass
	} {
		for _, idx := range []uint8{0, 7, 255} {
			id := mint(runID, idx)
			if !tfContainerIDRE.MatchString(id) {
				t.Errorf("the sweep would not recognize its own container id %q (run %q, idx %d)", id, runID, idx)
			}
		}
	}

	// One character past what the mint can produce is not ours.
	overlong := "tf-" + strings.Repeat("a", containerIDRunFragmentMax+1) + "-0"
	if tfContainerIDRE.MatchString(overlong) {
		t.Errorf("matcher accepts %q, which is longer than any id this package mints — it reaches containers we do not own", overlong)
	}
}
