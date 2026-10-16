//go:build linux

package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// captureJailMemoryMB is the memory ceiling of one capture jail. The capture's
// /tmp is memory-backed and holds the objects `git add` writes for every
// changed file, because the tree itself is mounted read-only, so the ceiling
// has to cover the working set of a large uncommitted change.
const captureJailMemoryMB = 4096

// captureJailTmpMB bounds that /tmp inside the ceiling, leaving the rest for
// git and the capture process itself.
const captureJailTmpMB = 3072

// captureSubcommand is the pinned program and verb of the capture jail:
// the broker's own binary, which the broker resolves itself, and the one verb
// that reads a tree and writes a stream.
const captureSubcommand = "snapshot-capture"

// buildCaptureSpec is the OCI spec of the capture jail, from the same fixed
// template family as buildSpec: empty capabilities, no new privileges, the
// tree owner's uid and gid, a read-only root filesystem, and a network
// namespace of its own that nothing is ever attached to. The tree is mounted
// read-only, so the capture cannot change the thing it reads, and the only
// writable places are the memory-backed /tmp and /dev.
func buildCaptureSpec(p CaptureParams, rootSource string, uid, gid int, tfBinary string) (*specs.Spec, error) {
	if !IsJailUID(uid) || gid != uid {
		return nil, fmt.Errorf("spec: capture identity %d:%d is not a per-run jail identity", uid, gid)
	}
	args := append([]string{TrustedTFBinaryDestination, captureSubcommand, "/work"}, p.Checkouts...)
	noNewPrivs := true
	return &specs.Spec{
		Version: specs.Version,
		Process: &specs.Process{
			Terminal:        false,
			User:            specs.User{UID: uint32(uid), GID: uint32(gid)},
			Args:            args,
			Env:             CaptureJailEnv(),
			Cwd:             "/",
			NoNewPrivileges: noNewPrivs,
			Capabilities: &specs.LinuxCapabilities{
				Bounding: []string{}, Effective: []string{}, Permitted: []string{},
				Inheritable: []string{}, Ambient: []string{},
			},
			Rlimits: defaultRlimits(),
		},
		Root:     &specs.Root{Path: "rootfs", Readonly: true},
		Hostname: "tf-capture",
		Mounts: []specs.Mount{
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{Destination: "/dev", Type: "tmpfs", Source: "tmpfs",
				Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
			{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs",
				Options: []string{"nosuid", "nodev", "mode=1777", fmt.Sprintf("size=%dm", captureJailTmpMB)}},
			{Destination: "/work", Type: "bind", Source: rootSource, Options: []string{"rbind", "ro"}},
			{Destination: TrustedTFBinaryDestination, Type: "bind", Source: tfBinary, Options: []string{"rbind", "ro"}},
		},
		Linux: &specs.Linux{
			Namespaces: []specs.LinuxNamespace{
				{Type: specs.PIDNamespace},
				{Type: specs.IPCNamespace},
				{Type: specs.UTSNamespace},
				{Type: specs.MountNamespace},
				{Type: specs.NetworkNamespace},
			},
			Seccomp: defaultSeccompProfile(),
		},
	}, nil
}

// captureBinaryPath resolves the program the capture jail runs: the broker's
// own binary, the one it already re-execs and the one every jail's `tfac`
// verbs come from. A var so this package's integration tests can substitute a
// static build of the same capture body; production never reassigns it.
var captureBinaryPath = TrustedTFBinaryPath

// newRunscCaptureCommand is the runsc invocation of the capture jail, run
// under prlimit so that runsc and everything it starts may write no file past
// the stream's cap. The stream is a host file passed in as a descriptor, so
// neither the jail's tmpfs and cgroup limits nor a file-size limit inside the
// jail bound it (gVisor does not apply the latter to a host descriptor); the
// limit on the process that does the host write does. prlimit execs runsc, so
// the process the supervisor signals is runsc itself. --network=none, so the sandbox has a loopback and nothing else, no
// --host-uds, so it can reach no host socket, and the capture's one output
// passed in as descriptor CaptureStreamFD.
func newRunscCaptureCommand(ctx context.Context, bundleDir, containerID string, stream *os.File) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "prlimit", fmt.Sprintf("--fsize=%d", CaptureStreamMaxBytes), "--", "runsc",
		"--platform=systrap",
		"--ignore-cgroups",
		"--network=none",
		"run",
		"--bundle", bundleDir,
		"--pass-fd", fmt.Sprintf("%d:%d", CaptureStreamFD, CaptureStreamFD),
		containerID,
	)
	cmd.ExtraFiles = []*os.File{stream}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGKILL)
	}
	return cmd
}

// captureIDSeq numbers the capture jails this process launches. It starts at a
// random point, so a jail a previous process left behind is unlikely to share an
// id with one launched now, and counts up, so no two jails of this process share
// one however many run at once. Randomness alone cannot give that: the id has
// room for 32 bits, and launching a jail first force-deletes any container of
// the same id, which for a live twin is the other capture's jail.
var captureIDSeq atomic.Uint32

func init() {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(fmt.Sprintf("sandbox: seed capture container ids: %v", err))
	}
	captureIDSeq.Store(binary.BigEndian.Uint32(raw[:]))
}

// newCaptureContainerID mints a container id the boot sweep's matcher
// (tfContainerIDRE) recognizes, so a capture jail a crash left behind is
// reaped like any other.
func newCaptureContainerID() string {
	return fmt.Sprintf("tf-cap%08x-0", captureIDSeq.Add(1))
}

// LaunchCaptureJail runs one capture in a fresh, one-shot gVisor jail and
// waits for it. The jail holds nothing of the agent's but the files it is
// there to read: the run tree read-only, TF's own binary read-only, a root
// filesystem with git, and one output file. It has no network and no socket,
// runs with no capability, and shares nothing with the run's own jail, which
// the caller has stopped or frozen. So whatever the checkout's config and
// attributes make git execute executes in there, with no path to the host.
//
// The jail runs as the identity that owns the tree, so it reads what the agent
// made private to itself. stdout, which this function closes, becomes the
// jail's stdout and carries the small JSON manifest; the large members land in
// the staging stream, which the jail inherits as a descriptor, so no byte of
// the run passes through this process.
//
// A tree no jail identity holds is refused: there is no identity to read it as.
// Returns a bounded tail of the jail's stderr, which is diagnostics and never
// run data, whatever the outcome.
func LaunchCaptureJail(ctx context.Context, p CaptureParams, stdout *os.File) (stderrTail string, err error) {
	defer func() { _ = stdout.Close() }()

	if err := ValidateCaptureCheckouts(p.Checkouts); err != nil {
		return "", err
	}
	rootFD, rootStat, err := pinCaptureRoot(p.Root)
	if err != nil {
		return "", err
	}
	defer closeFd(rootFD)
	uid, gid, err := captureIdentity(p.Root, rootStat)
	if err != nil {
		return "", err
	}
	rootMount, unbind, err := bindPinnedRoot(rootFD)
	if err != nil {
		return "", err
	}
	defer unbind()
	stream, err := openCaptureStaging(p.StagingDir)
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()

	tfBinary, err := captureBinaryPath()
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve the TF binary for the capture jail: %w", err)
	}
	rootfs, err := resolveCatalogRootfs(ctx, RootfsSelector{})
	if err != nil {
		return "", err
	}
	spec, err := buildCaptureSpec(p, rootMount, uid, gid, tfBinary)
	if err != nil {
		return "", err
	}
	bundleDir, err := writeBundle(Config{ConversationID: "capture"}, spec, rootfs)
	if err != nil {
		return "", err
	}
	defer func() { _ = cleanupBundle(bundleDir) }()

	id := newCaptureContainerID()
	rt, err := launchSupervised(ctx, id, captureJailMemoryMB, stdout, io.Discard, func() *exec.Cmd {
		return newRunscCaptureCommand(ctx, bundleDir, id, stream)
	})
	if err != nil {
		return "", fmt.Errorf("sandbox: launch capture jail: %w", err)
	}
	_, waitErr := rt.Wait()
	tail := rt.StderrTail()
	if ctx.Err() != nil {
		return tail, ctx.Err()
	}
	if waitErr != nil {
		return tail, fmt.Errorf("sandbox: capture jail: %w", waitErr)
	}
	return tail, nil
}
