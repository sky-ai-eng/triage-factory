//go:build linux

package gvisor

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/cell"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// deadRun is a LaunchedRun that never was: it stands in for a jail whose
// runtime exited before the in-jail tool host could dial back, which is what
// a runsc create failure looks like from this side. Only Stderr and the
// lifecycle no-ops are reached on that path.
type deadRun struct{ stderr string }

func (deadRun) Start() error                { return nil }
func (deadRun) Stdin() io.WriteCloser       { return nil }
func (deadRun) Stdout() io.Reader           { return nil }
func (r deadRun) Stderr() string            { return r.stderr }
func (deadRun) Wait() error                 { return errors.New("exit status 128") }
func (deadRun) OOMKilled() bool             { return false }
func (deadRun) Actuals() sandbox.RunActuals { return sandbox.RunActuals{} }
func (deadRun) Pid() int                    { return 0 }
func (deadRun) Close() error                { return nil }

// Compile-time proof the stand-in still satisfies what the jail holds.
var _ sandbox.LaunchedRun = deadRun{}

// TestToolHostJail_ConnectReportsJailStderr pins the diagnosis this error is
// supposed to give. The jail's stderr is the only place runsc's own create
// failure is written, so a Connect that reports a dead jail without it says
// nothing a reader can act on.
func TestToolHostJail_ConnectReportsJailStderr(t *testing.T) {
	const runscErr = `creating container: container with id "tf-conv-0" already exists`

	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "tools.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// The watcher's post-conditions, already satisfied: the runtime exited
	// and nothing ever dialed in.
	waitDone := make(chan struct{})
	close(waitDone)
	jail := &toolHostJail{
		listener: ln,
		run:      deadRun{stderr: runscErr},
		waitDone: waitDone,
		waitErr:  errors.New("exit status 128"),
	}

	_, err = jail.Connect(context.Background(), 5*time.Second)
	if err == nil {
		t.Fatal("Connect returned a connection for a jail that already exited")
	}
	if !strings.Contains(err.Error(), "exited before connecting") {
		t.Errorf("error = %v, want the fail-fast exited-before-connecting message", err)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v, want it to carry the jail's stderr; without it the failure is unattributable", err)
	}
}

type freezableRun struct {
	deadRun
	calls []string
}

func (r *freezableRun) Pause(context.Context) error  { r.calls = append(r.calls, "pause"); return nil }
func (r *freezableRun) Resume(context.Context) error { r.calls = append(r.calls, "resume"); return nil }

var _ sandbox.Freezer = (*freezableRun)(nil)

func TestToolHostJail_FreezeAndThaw(t *testing.T) {
	run := &freezableRun{}
	jail := &toolHostJail{run: run, waitDone: make(chan struct{})}
	if err := jail.Freeze(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := jail.Thaw(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(run.calls) != 2 || run.calls[0] != "pause" || run.calls[1] != "resume" {
		t.Errorf("calls = %v", run.calls)
	}
}

// A jail whose launcher cannot freeze is an error, never a quiet success: a
// capture that went ahead on it would read a moving tree and call it still.
func TestToolHostJail_FreezeWithoutAFreezerFails(t *testing.T) {
	jail := &toolHostJail{run: deadRun{}, waitDone: make(chan struct{})}
	if err := jail.Freeze(context.Background()); err == nil {
		t.Error("Freeze on a launcher that cannot freeze succeeded")
	}
}

// An exited jail is as still as a frozen one.
func TestToolHostJail_ExitedJailNeedsNoFreeze(t *testing.T) {
	done := make(chan struct{})
	close(done)
	jail := &toolHostJail{run: deadRun{}, waitDone: done}
	if err := jail.Freeze(context.Background()); err != nil {
		t.Errorf("Freeze of an exited jail: %v", err)
	}
	if err := jail.Thaw(context.Background()); err != nil {
		t.Errorf("Thaw of an exited jail: %v", err)
	}
}

// An exited jail has no count to read: its cgroup went with it, and a zero
// would read as "nothing was killed" to a caller comparing it to an earlier
// count.
func TestToolHostJail_ExitedJailHasNoOOMKillCount(t *testing.T) {
	done := make(chan struct{})
	close(done)
	jail := &toolHostJail{run: deadRun{}, sb: &sandbox.Sandbox{ContainerID: "tf-exited-1"}, waitDone: done}
	if n, ok := jail.OOMKills(); ok {
		t.Errorf("OOMKills on an exited jail = (%d, true), want no count", n)
	}
	var never *toolHostJail
	if n, ok := never.OOMKills(); ok {
		t.Errorf("OOMKills on a jail never launched = (%d, true), want no count", n)
	}
}

// Stop returns once the runtime has been reaped, and not before.
func TestToolHostJail_StopWaitsForTheRuntime(t *testing.T) {
	done := make(chan struct{})
	jail := &toolHostJail{run: deadRun{}, waitDone: done}
	returned := make(chan error, 1)
	go func() { returned <- jail.Stop() }()
	select {
	case <-returned:
		t.Fatal("Stop returned while the runtime was still alive")
	case <-time.After(100 * time.Millisecond):
	}
	close(done)
	select {
	case err := <-returned:
		if err != nil {
			t.Errorf("Stop = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after the runtime exited")
	}
	var nilJail *toolHostJail
	if err := nilJail.Stop(); err != nil {
		t.Errorf("Stop on nil = %v", err)
	}
}

// actualsRun is an exited run that measured something, so Close has actuals
// to stamp.
type actualsRun struct{ deadRun }

func (actualsRun) Actuals() sandbox.RunActuals {
	mb := 64
	return sandbox.RunActuals{PeakMemMB: &mb}
}

// Close is idempotent, as cell.Jail requires: a second Close stamps no second
// set of actuals.
func TestToolHostJail_CloseIsIdempotent(t *testing.T) {
	done := make(chan struct{})
	close(done)
	stamps := 0
	jail := &toolHostJail{
		run:      actualsRun{},
		waitDone: done,
		claimID:  "claim-a",
		recordActuals: func(context.Context, string, string, cell.Actuals) error {
			stamps++
			return nil
		},
	}
	_ = jail.Close()
	_ = jail.Close()
	if stamps != 1 {
		t.Fatalf("Close stamped actuals %d times, want 1", stamps)
	}

	var none *toolHostJail
	if !none.Exited() {
		t.Error("a nil jail reports processes still running")
	}
}

// A jail that is not running reads as unobserved, never as a jail that used
// nothing: the sampler skips it rather than recording zeros.
func TestToolHostJail_SampleOfAJailNotRunning(t *testing.T) {
	done := make(chan struct{})
	close(done)
	for name, jail := range map[string]*toolHostJail{
		"never launched": nil,
		"not started":    {},
		"exited":         {run: deadRun{}, waitDone: done, sb: &sandbox.Sandbox{ContainerID: "tf-gone-1"}},
	} {
		if s := jail.Sample(); s.Observed() {
			t.Errorf("%s: Sample = %+v, want unobserved", name, s)
		}
	}
}

// stubToolHostBinary points the mount source at a file a non-root test user
// can create, since toolHostMounts refuses to assemble without one.
func stubToolHostBinary(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tf-harness-tools")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := hostToolHostBinaryPath
	hostToolHostBinaryPath = path
	t.Cleanup(func() { hostToolHostBinaryPath = prev })
}

func destinations(mounts []sandbox.Mount) map[string]sandbox.Mount {
	byDest := make(map[string]sandbox.Mount, len(mounts))
	for _, m := range mounts {
		byDest[m.Destination] = m
	}
	return byDest
}

// testSpec is a launch with the two inputs every launch carries, at the host
// paths the sidecar uses for run-1, and the staging directories given.
func testSpec(skillsSource, memorySource string) cell.LaunchSpec {
	return cell.LaunchSpec{
		AgentHostSocket: "/run/tf/run-1.sock",
		CABundle:        "/run/tf/run-1/ca-bundle.crt",
		SkillsSource:    skillsSource,
		MemorySource:    memorySource,
	}
}

// TestToolHostMounts_CarryTheCABundle pins the trust-file mount: every launch
// carries it, a gh channel or not, because every registry the egress proxy
// admits is signed by the CA in it.
func TestToolHostMounts_CarryTheCABundle(t *testing.T) {
	stubToolHostBinary(t)

	mounts, err := toolHostMounts(t.TempDir(), testSpec("", ""))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := destinations(mounts)["/run/tf-ca-bundle.crt"]
	if !ok {
		t.Fatal("no mount at /run/tf-ca-bundle.crt; no registry the egress proxy admits would verify in this jail")
	}
	if got.Source != "/run/tf/run-1/ca-bundle.crt" || len(got.Options) != 1 || got.Options[0] != "ro" {
		t.Errorf("CA bundle mount = %+v, want the per-run bundle, read-only", got)
	}
}

// TestToolHostMounts_CarryTheAgentHostSocket pins the exec-verb socket mount.
// Without it every `tfac` verb inside the jail falls back to a local client
// over a fresh in-jail database and reports the run itself as nonexistent.
func TestToolHostMounts_CarryTheAgentHostSocket(t *testing.T) {
	stubToolHostBinary(t)

	mounts, err := toolHostMounts(t.TempDir(), testSpec("", ""))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := destinations(mounts)["/run/tf.sock"]
	if !ok {
		t.Fatal("no mount at /run/tf.sock; every exec verb in this jail is stranded on the local-client fallback")
	}
	if got.Source != "/run/tf/run-1.sock" {
		t.Errorf("agenthost socket mount source = %q, want the per-run socket handed in", got.Source)
	}
}

// TestToolHostMounts_CarryTheStagedStepSkill pins that a jail mounts this
// step's SKILL.md. The skill is staged outside the run tree, so without this
// mount a blueprint step stages a skill the agent never sees, silently, with
// the run otherwise healthy.
func TestToolHostMounts_CarryTheStagedStepSkill(t *testing.T) {
	stubToolHostBinary(t)
	staged := t.TempDir()

	mounts, err := toolHostMounts(t.TempDir(), testSpec(staged, ""))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := destinations(mounts)[sandbox.TrustedSkillsDestination]
	if !ok {
		t.Fatalf("no mount at %s; the step staged a skill the agent will never see", sandbox.TrustedSkillsDestination)
	}
	if got.Source != staged {
		t.Errorf("skills mount source = %q, want the staging dir %q", got.Source, staged)
	}
	// Read-only is required, not merely permitted: the mount exists so the
	// jail can READ a skill TF authored, and the broker rejects an rw bind.
	if len(got.Options) != 1 || got.Options[0] != "ro" {
		t.Errorf("skills mount options = %v, want [ro]", got.Options)
	}
}

// TestToolHostMounts_SkillsAreOptional pins the two ways a launch legitimately
// carries no skill: a run that is not a blueprint step, and a step whose
// staging dir was swept before a cold cross-executor resume. Neither may fail
// the launch — the agent continues from its transcript, which is a discovery
// degradation rather than a broken run.
func TestToolHostMounts_SkillsAreOptional(t *testing.T) {
	stubToolHostBinary(t)

	for _, tc := range []struct{ name, source string }{
		{"not a blueprint step", ""},
		{"staging dir swept", filepath.Join(t.TempDir(), "gone")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mounts, err := toolHostMounts(t.TempDir(), testSpec(tc.source, ""))
			if err != nil {
				t.Fatalf("a missing skill must not fail the launch: %v", err)
			}
			if _, ok := destinations(mounts)[sandbox.TrustedSkillsDestination]; ok {
				t.Error("a skills mount was assembled with no staged skill behind it; the broker would reject the launch")
			}
		})
	}
}

// TestToolHostMounts_CarryTheStagedEntityMemory pins the same contract for the
// blueprint handoff: without the memory mount a step reads none of its
// predecessors' notes.
func TestToolHostMounts_CarryTheStagedEntityMemory(t *testing.T) {
	stubToolHostBinary(t)
	staged := t.TempDir()

	mounts, err := toolHostMounts(t.TempDir(), testSpec("", staged))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := destinations(mounts)[sandbox.TrustedMemoryDestination]
	if !ok {
		t.Fatalf("no mount at %s; this step reads none of the prior memory it staged", sandbox.TrustedMemoryDestination)
	}
	if got.Source != staged {
		t.Errorf("memory mount source = %q, want the staging dir %q", got.Source, staged)
	}
	// Read-only for the reason the broker enforces it: an rw bind would let one
	// step rewrite the handoff a later step reads as fact.
	if len(got.Options) != 1 || got.Options[0] != "ro" {
		t.Errorf("memory mount options = %v, want [ro]", got.Options)
	}
}

// TestToolHostMounts_EntityMemoryIsOptional mirrors the skills case: a swept
// staging dir costs the step its handoff, which is a degraded start rather than
// a launch the broker must refuse.
func TestToolHostMounts_EntityMemoryIsOptional(t *testing.T) {
	stubToolHostBinary(t)

	for _, tc := range []struct{ name, source string }{
		{"nothing staged", ""},
		{"staging dir swept", filepath.Join(t.TempDir(), "gone")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mounts, err := toolHostMounts(t.TempDir(), testSpec("", tc.source))
			if err != nil {
				t.Fatalf("missing prior memory must not fail the launch: %v", err)
			}
			if _, ok := destinations(mounts)[sandbox.TrustedMemoryDestination]; ok {
				t.Error("a memory mount was assembled with nothing staged behind it; the broker would reject the launch")
			}
		})
	}
}
