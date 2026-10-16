//go:build linux && integration

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests run the real capture jail: a one-shot gVisor jail around the
// production `snapshot-capture` body, against trees the agent controls. They
// need root, runsc and a prebuilt rootfs, like the rest of this suite, and a
// static build of the capture body (cmd/capture-test-stub, built by the test
// script as the invoking user and named by TF_TEST_CAPTURE_STUB; built here
// with `go build` when it is not set and a toolchain is on PATH).

func requireCaptureJail(t *testing.T) string {
	t.Helper()
	requireRunsc(t)
	if os.Geteuid() != 0 {
		t.Skip("a capture jail is launched with root's capabilities")
	}
	stub := os.Getenv("TF_TEST_CAPTURE_STUB")
	if stub == "" {
		if _, err := exec.LookPath("go"); err != nil {
			t.Skip("no TF_TEST_CAPTURE_STUB and no go toolchain to build cmd/capture-test-stub")
		}
		stub = filepath.Join(t.TempDir(), "capture-test-stub")
		cmd := exec.Command("go", "build", "-o", stub, "github.com/sky-ai-eng/triage-factory/cmd/capture-test-stub")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("build capture-test-stub: %v: %s", err, out)
		}
	}
	orig := captureBinaryPath
	captureBinaryPath = func() (string, error) { return stub, nil }
	t.Cleanup(func() { captureBinaryPath = orig })
	return stub
}

// worldTempDir makes a directory under the test TMPDIR that every uid can
// traverse — t.TempDir is owner-only, so a jail identity could not reach a
// tree under it.
func worldTempDir(t *testing.T, pattern string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", pattern)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func gitRoot(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func chownTree(t *testing.T, root string, uid int) {
	t.Helper()
	if out, err := exec.Command("chown", "-R", fmt.Sprintf("%d:%d", uid, uid), root).CombinedOutput(); err != nil {
		t.Fatalf("chown %s: %v: %s", root, err, out)
	}
	if err := os.Chmod(root, 0o770); err != nil {
		t.Fatal(err)
	}
}

// captureResult is what one capture came back with.
type captureResult struct {
	manifest []byte
	stream   []byte
}

// jailCapture runs LaunchCaptureJail over root's checkouts and returns the
// manifest and the raw stream.
func jailCapture(t *testing.T, root string, checkouts ...string) (captureResult, error) {
	t.Helper()
	staging := captureTestStaging(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	manifest := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r)
		_ = r.Close()
		manifest <- b
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tail, err := LaunchCaptureJail(ctx, CaptureParams{Root: root, Checkouts: checkouts, StagingDir: staging}, w)
	res := captureResult{manifest: <-manifest}
	res.stream, _ = os.ReadFile(filepath.Join(staging, CaptureStreamFile))
	if err != nil {
		return res, fmt.Errorf("%w (stderr: %s)", err, tail)
	}
	return res, nil
}

// hostCapture is the control: the same capture body, as the same identity,
// with the same environment, run on the host instead of in a jail — which is
// what the capture was before it had one, and what the markers below measure
// the jail against.
func hostCapture(t *testing.T, stub string, uid int, lazyFetch bool, root string, checkouts ...string) (captureResult, error) {
	t.Helper()
	staging := captureTestStaging(t)
	stream, err := os.OpenFile(filepath.Join(staging, CaptureStreamFile), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	var env []string
	for _, kv := range CaptureJailEnv() {
		if !lazyFetch || kv != "GIT_NO_LAZY_FETCH=1" {
			env = append(env, kv)
		}
	}
	bin := filepath.Join(worldTempDir(t, "tf-stub-"), "stub")
	src, err := os.ReadFile(stub)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, src, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, append([]string{"snapshot-capture", root}, checkouts...)...)
	cmd.Dir = "/"
	cmd.Env = env
	cmd.ExtraFiles = []*os.File{stream}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid), Groups: []uint32{}}}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	runErr := cmd.Run()
	res := captureResult{manifest: out.Bytes()}
	res.stream, _ = os.ReadFile(filepath.Join(staging, CaptureStreamFile))
	if runErr != nil {
		return res, fmt.Errorf("%w (stderr: %s)", runErr, errb.String())
	}
	return res, nil
}

// requireJailRan fails the test when a capture jail never started. A canary's
// "nothing reached the host" is only evidence if the planted config was given a
// jail to run in; a launch failure also leaves no marker.
func requireJailRan(t *testing.T, err error) {
	t.Helper()
	if err != nil && (strings.Contains(err.Error(), "creating container") || strings.Contains(err.Error(), "launch capture jail") || strings.Contains(err.Error(), "bind the pinned tree")) {
		t.Fatalf("the capture jail did not start, so the check proves nothing: %v", err)
	}
}

func requireMarker(t *testing.T, dir, name string, want bool, what string) {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	if want && err != nil {
		t.Errorf("control: %s did not run on the host (%v); the canary would pass whatever the jail does", what, err)
	}
	if !want && err == nil {
		t.Errorf("%s ran on the host: %s exists", what, filepath.Join(dir, name))
	}
}

// plantedFilterCheckout builds a checkout whose config and attributes are the
// agent's: a clean filter named only through an included config file, applied
// to every path, set to write a file under marks.
func plantedFilterCheckout(t *testing.T, root, marks string) {
	t.Helper()
	co := filepath.Join(root, "acme", "app", "default")
	if err := os.MkdirAll(co, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRoot(t, co, "init", "-q", "-b", "work")
	writeFileT(t, filepath.Join(co, "a.txt"), "one\n")
	gitRoot(t, co, "add", "-A")
	gitRoot(t, co, "commit", "-q", "-m", "seed")

	include := filepath.Join(root, "agent-include.cfg")
	writeFileT(t, include, "[filter \"x\"]\n\tclean = touch "+marks+"/include && cat\n")
	cfg, err := os.OpenFile(filepath.Join(co, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = cfg.WriteString("[include]\n\tpath = " + include + "\n")
	_ = cfg.Close()
	writeFileT(t, filepath.Join(co, ".gitattributes"), "* filter=x\n")
	writeFileT(t, filepath.Join(co, "a.txt"), "one\ntwo\n")
}

// plantedPromisorCheckout builds a checkout that names a promisor remote whose
// transport is a command writing a file under marks, with a blob its HEAD needs
// missing, so that git reaches for the remote.
func plantedPromisorCheckout(t *testing.T, root, marks string) {
	t.Helper()
	co := filepath.Join(root, "acme", "app", "default")
	if err := os.MkdirAll(co, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRoot(t, co, "init", "-q", "-b", "work")
	writeFileT(t, filepath.Join(co, "m.txt"), "needs a blob that is not here\n")
	gitRoot(t, co, "add", "-A")
	gitRoot(t, co, "commit", "-q", "-m", "seed")
	blob := gitRoot(t, co, "rev-parse", "HEAD:m.txt")
	if err := os.Remove(filepath.Join(co, ".git", "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	cfg, err := os.OpenFile(filepath.Join(co, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = cfg.WriteString(strings.Join([]string{
		"[core]", "\tsshCommand = sh -c 'touch " + marks + "/ssh' --",
		"[remote \"origin\"]", "\turl = ssh://agent-controlled.invalid/x.git",
		"\tpromisor = true", "\tpartialclonefilter = blob:none", "",
	}, "\n"))
	_ = cfg.Close()
}

// TestIntegration_CaptureJail_AgentControlledGitCannotReachTheHost is the
// canary for the capture: everything an agent can plant in a checkout that
// makes git run a program or read or fetch from somewhere else is planted, and
// each is set to write a file to a host path. The control runs the same
// capture on the host as the same identity and shows each one fire; in the jail
// none does.
func TestIntegration_CaptureJail_AgentControlledGitCannotReachTheHost(t *testing.T) {
	stub := requireCaptureJail(t)
	const idx = 9
	uid := JailUID(idx)

	marks := worldTempDir(t, "tf-marks-")
	if err := os.Chmod(marks, 0o777); err != nil {
		t.Fatal(err)
	}
	for _, sc := range []struct {
		name, marker, what string
		plant              func(*testing.T, string, string)
		lazyFetch          bool
	}{
		{"filter", "include", "a clean filter named through include.path", plantedFilterCheckout, false},
		{"promisor", "ssh", "core.sshCommand for a promisor fetch", plantedPromisorCheckout, true},
	} {
		t.Run(sc.name, func(t *testing.T) {
			marker := filepath.Join(marks, sc.marker)
			t.Run("control: on the host the planted config runs", func(t *testing.T) {
				_ = os.Remove(marker)
				root := worldTempDir(t, "tf-ctl-tree-")
				sc.plant(t, root, marks)
				chownTree(t, root, uid)
				_, err := hostCapture(t, stub, uid, sc.lazyFetch, root, "acme/app/default")
				t.Logf("host capture ended with: %v", err)
				requireMarker(t, marks, sc.marker, true, sc.what)
				_ = os.Remove(marker)
			})
			t.Run("in the capture jail it does not reach the host", func(t *testing.T) {
				_ = os.Remove(marker)
				root := worldTempDir(t, "tf-jail-tree-")
				sc.plant(t, root, marks)
				chownTree(t, root, uid)
				_, err := jailCapture(t, root, "acme/app/default")
				t.Logf("capture jail ended with: %v", err)
				requireJailRan(t, err)
				requireMarker(t, marks, sc.marker, false, sc.what)
			})
		})
	}
}

// TestIntegration_CaptureJail_AlternatesCannotReadAnotherRun: a checkout whose
// objects/info/alternates names another run's object store, with a HEAD only
// that store holds. On the host the capture carries the other run's commit
// into the stream; in the jail, where that path does not exist, it fails or
// carries nothing of it.
func TestIntegration_CaptureJail_AlternatesCannotReadAnotherRun(t *testing.T) {
	stub := requireCaptureJail(t)
	const idx = 10
	uid := JailUID(idx)

	build := func(t *testing.T) (root, secretCommit string) {
		other := worldTempDir(t, "tf-other-run-")
		gitRoot(t, other, "init", "-q", "-b", "main")
		writeFileT(t, filepath.Join(other, "secret.txt"), "another run's work\n")
		gitRoot(t, other, "add", "-A")
		gitRoot(t, other, "commit", "-q", "-m", "another run's commit")
		secretCommit = gitRoot(t, other, "rev-parse", "HEAD")

		root = worldTempDir(t, "tf-alt-tree-")
		co := filepath.Join(root, "acme", "app", "default")
		if err := os.MkdirAll(co, 0o755); err != nil {
			t.Fatal(err)
		}
		gitRoot(t, co, "init", "-q", "-b", "work")
		writeFileT(t, filepath.Join(co, ".git", "objects", "info", "alternates"), filepath.Join(other, ".git", "objects")+"\n")
		writeFileT(t, filepath.Join(co, ".git", "refs", "heads", "work"), secretCommit+"\n")
		chownTree(t, root, uid)
		chownTree(t, other, uid)
		return root, secretCommit
	}

	t.Run("control: on the host the other run's commit is carried", func(t *testing.T) {
		root, secret := build(t)
		res, err := hostCapture(t, stub, uid, false, root, "acme/app/default")
		if err != nil || !bytes.Contains(res.stream, []byte(secret)) {
			t.Errorf("control: host capture = %v, stream carries the other run's commit: %v; the canary would pass whatever the jail does",
				err, bytes.Contains(res.stream, []byte(secret)))
		}
	})

	t.Run("in the capture jail it fails or carries nothing of it", func(t *testing.T) {
		root, secret := build(t)
		res, err := jailCapture(t, root, "acme/app/default")
		t.Logf("capture jail ended with: %v", err)
		requireJailRan(t, err)
		if bytes.Contains(res.stream, []byte(secret)) {
			t.Error("the capture jail carried a commit from another run's object store")
		}
	})
}

// patchGens returns the number each of a.n and b.n holds in the checkout's
// patch member.
func patchGens(t *testing.T, stream []byte) (a, b int) {
	t.Helper()
	sr := NewCaptureStreamReader(bytes.NewReader(stream))
	for {
		name, body, err := sr.Next()
		if err != nil {
			t.Fatalf("no patch member in the stream: %v", err)
		}
		data, err := io.ReadAll(body)
		if err != nil {
			t.Fatal(err)
		}
		if name != "checkouts/acme/app/default/patch" {
			continue
		}
		got := map[string]int{}
		re := regexp.MustCompile(`(?m)^diff --git a/(\w)\.n b/\w\.n\n(?:.*\n)*?\+(\d+)$`)
		for _, m := range re.FindAllSubmatch(data, -1) {
			n, _ := strconv.Atoi(string(m[2]))
			got[string(m[1])] = n
		}
		if len(got) != 2 {
			t.Fatalf("the patch did not carry both files: %v\n%s", got, data)
		}
		return got["a"], got["b"]
	}
}

// TestIntegration_CaptureJail_FrozenRunStandsStill: a process in the run's jail
// rewrites two files in a loop, a before b, so at every instant a is b or one
// ahead. A capture of the frozen jail sees such an instant; a capture of the
// running one reads a, then b a moment later, and sees a gap. The frozen jail's
// process is still running after the resume.
func TestIntegration_CaptureJail_FrozenRunStandsStill(t *testing.T) {
	requireCaptureJail(t)
	root := worldTempDir(t, "tf-live-tree-")
	co := filepath.Join(root, "acme", "app", "default")
	if err := os.MkdirAll(co, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRoot(t, co, "init", "-q", "-b", "work")
	writeFileT(t, filepath.Join(co, "base.txt"), "base\n")
	gitRoot(t, co, "add", "-A")
	gitRoot(t, co, "commit", "-q", "-m", "seed")

	cfg := minimalConfig(t)
	cfg.Worktree = root
	cfg.Argv = []string{"/bin/sh", "-c", `
		n=0
		while :; do
			n=$((n+1))
			echo $n > /work/acme/app/default/a.n.tmp && mv /work/acme/app/default/a.n.tmp /work/acme/app/default/a.n
			echo $n > /work/acme/app/default/b.n.tmp && mv /work/acme/app/default/b.n.tmp /work/acme/app/default/b.n
		done`}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer func() { _ = sb.Close() }()
	defer func() { _ = run.Close() }()
	chownTree(t, root, JailUID(sb.SubnetIdx))
	if err := run.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	freezer, ok := run.(Freezer)
	if !ok {
		t.Fatal("the launched run cannot be frozen")
	}

	readA := func() int {
		b, _ := os.ReadFile(filepath.Join(co, "a.n"))
		n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return n
	}
	deadline := time.Now().Add(20 * time.Second)
	for readA() < 100 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if readA() < 100 {
		t.Fatalf("the run's writer did not start: a.n = %d", readA())
	}

	// Running: the capture reads a, then b later, and sees a gap.
	running, err := jailCapture(t, root, "acme/app/default")
	if err != nil {
		t.Fatalf("capture of the running jail: %v", err)
	}
	a, b := patchGens(t, running.stream)
	t.Logf("running capture: a=%d b=%d", a, b)
	if b-a < 2 && a-b < 2 {
		t.Logf("the running capture happened to read an instant (a=%d b=%d); the control is probabilistic", a, b)
	}

	// Frozen: one instant.
	if err := freezer.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	frozen, err := jailCapture(t, root, "acme/app/default")
	if err != nil {
		_ = freezer.Resume(ctx)
		t.Fatalf("capture of the frozen jail: %v", err)
	}
	stoppedAt := readA()
	time.Sleep(time.Second)
	if readA() != stoppedAt {
		t.Error("the frozen jail's writer kept running")
	}
	if err := freezer.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	a, b = patchGens(t, frozen.stream)
	t.Logf("frozen capture: a=%d b=%d", a, b)
	if a != b && a != b+1 {
		t.Errorf("the frozen capture read a=%d, b=%d: not one instant of the writer", a, b)
	}

	before := readA()
	deadline = time.Now().Add(10 * time.Second)
	for readA() <= before && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if readA() <= before {
		t.Error("the run's process did not carry on after the resume")
	}
}

// TestIntegration_PauseResume_UnknownJailIsNothingToFreeze pins that freezing
// a jail that has exited is not an error.
func TestIntegration_PauseResume_UnknownJailIsNothingToFreeze(t *testing.T) {
	requireRunsc(t)
	if err := PauseRuntime(context.Background(), "tf-nosuchjail-0"); err != nil {
		t.Errorf("PauseRuntime of an unknown jail: %v", err)
	}
	if err := ResumeRuntime(context.Background(), "tf-nosuchjail-0"); err != nil {
		t.Errorf("ResumeRuntime of an unknown jail: %v", err)
	}
}

// TestIntegration_CaptureJail_StreamWriteIsCapped: a capture that writes to its
// output descriptor without end is stopped by the jail at the stream's cap, so
// the host file never grows past it. The "capture" here is a shell script that
// writes zeros to descriptor 3 forever.
func TestIntegration_CaptureJail_StreamWriteIsCapped(t *testing.T) {
	requireRunsc(t)
	if os.Geteuid() != 0 {
		t.Skip("a capture jail is launched with root's capabilities")
	}
	const idx = 13
	uid := JailUID(idx)

	script := filepath.Join(worldTempDir(t, "tf-flood-"), "flood")
	writeFileT(t, script, "#!/bin/sh\nwhile :; do head -c 1048576 /dev/zero >&3 || exit 1; done\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := captureBinaryPath
	captureBinaryPath = func() (string, error) { return script, nil }
	t.Cleanup(func() { captureBinaryPath = orig })

	origMax := CaptureStreamMaxBytes
	CaptureStreamMaxBytes = 1 << 30
	t.Cleanup(func() { CaptureStreamMaxBytes = origMax })

	root := worldTempDir(t, "tf-flood-tree-")
	co := filepath.Join(root, "acme", "app", "default")
	if err := os.MkdirAll(co, 0o755); err != nil {
		t.Fatal(err)
	}
	chownTree(t, root, uid)

	staging := captureTestStaging(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, r); _ = r.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	start := time.Now()
	tail, lerr := LaunchCaptureJail(ctx, CaptureParams{Root: root, Checkouts: []string{"acme/app/default"}, StagingDir: staging}, w)
	info, err := os.Stat(filepath.Join(staging, CaptureStreamFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("capture jail ended with %v (stderr %q)", lerr, tail)
	t.Logf("the flood ended after %v with the stream at %d bytes (cap %d)", time.Since(start).Round(time.Millisecond), info.Size(), CaptureStreamMaxBytes)
	if info.Size() > CaptureStreamMaxBytes {
		t.Errorf("the stream file grew to %d bytes, past its cap %d", info.Size(), CaptureStreamMaxBytes)
	}
	if info.Size() == 0 {
		t.Error("the flood wrote nothing; the test would pass whatever the limit does")
	}
}

// TestIntegration_BindPinnedRoot_NamesThePinnedTreeAndLeavesNothing: the
// directory a capture jail mounts shows the pinned tree, keeps showing it after
// the path is swapped, and is gone after cleanup.
func TestIntegration_BindPinnedRoot_NamesThePinnedTreeAndLeavesNothing(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("mounting needs root")
	}
	base := worldTempDir(t, "tf-pin-")
	tree := filepath.Join(base, "tree")
	if err := os.Mkdir(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join(tree, "original"), "x")
	fd, _, err := pinCaptureRoot(tree)
	if err != nil {
		t.Skipf("tree owner not accepted here: %v", err)
	}
	defer closeFd(fd)
	target, cleanup, err := bindPinnedRoot(fd)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tree, filepath.Join(base, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "original")); err != nil {
		t.Errorf("the mountpoint does not show the pinned tree: %v", err)
	}
	cleanup()
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("the mountpoint is still there after cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "moved", "original")); err != nil {
		t.Errorf("cleanup reached into the tree: %v", err)
	}
}
