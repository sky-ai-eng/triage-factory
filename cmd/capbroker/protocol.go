//go:build linux

// Wire format for the cap-broker RPC. Shape copied from
// cmd/exec/agenthost's protocol.go/ipc.go/server.go (length-prefixed JSON
// frames, one-shot connection per call, a version handshake) rather than
// invented fresh.
package capbroker

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// maxFrameSize caps every request AND response frame. Every payload that
// crosses this RPC is a handful of strings/ints (run ids, subnet indices,
// iptables rule bookkeeping, a bounded stderr tail) — far under this
// ceiling; it exists to protect either side from a malformed peer sending a
// bogus multi-GB length header. Until ProtocolVersion 2 this was two
// different caps (a 512 MiB responseFrameSize existed solely for
// CaptureRunDelta's result, which used to embed the whole delta). That
// result now crosses as a passed-through socket stream, never as an RPC
// frame, so nothing on this RPC needs more than one small, uniform cap.
const maxFrameSize = 1 * 1024 * 1024 // 1 MiB

// ProtocolVersion is the wire-format version. The broker rejects a
// mismatching client version so an old binary talking to a new broker (or
// vice versa) surfaces a clear error instead of silently misbehaving — the
// same defensive handshake agenthost uses, load-bearing here too because
// the broker is a long-lived process that can outlive a binary upgrade
// until the next restart.
//
// Bumped to 2 for CaptureRunDelta's wire-shape change: the args gained
// StdoutSocketPath and the result dropped Delta (see below) — an old
// client/broker pairing across that change must surface the version
// mismatch rather than silently misinterpreting the new shape.
const ProtocolVersion = 2

// request is the envelope for every RPC. Method identifies the operation;
// Args is the method-specific payload (JSON-encoded).
type request struct {
	Version uint32          `json:"v"`
	Method  string          `json:"m"`
	Args    json.RawMessage `json:"a,omitempty"`
}

// response wraps either a method-specific Result (success) or an Error
// string (failure). Exactly one of Result / Error is set. Error is a plain
// string — the only consumers are internal/sandbox's PrivilegedOps call
// sites, which just wrap it with fmt.Errorf; there's no structured-error
// reconstruction need like agenthost's HTTP-status echo.
type response struct {
	Result json.RawMessage `json:"r,omitempty"`
	Error  string          `json:"e,omitempty"`
}

// writeFrame serializes msg as a length-prefixed JSON frame on w, capped at
// maxSize (maxFrameSize for both requests and responses — see maxFrameSize's
// doc for why one cap now covers both directions).
func writeFrame(w io.Writer, msg any, maxSize int) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("capbroker: marshal frame: %w", err)
	}
	if len(body) > maxSize {
		return fmt.Errorf("capbroker: frame %d bytes exceeds cap %d", len(body), maxSize)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("capbroker: write frame header: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("capbroker: write frame body: %w", err)
	}
	return nil
}

// readFrame reads one length-prefixed JSON frame from r and decodes it into
// dst, capped at maxSize (maxFrameSize on both sides of this RPC — see
// maxFrameSize's doc). EOF on the header read is returned verbatim so
// callers (the broker's accept loop) can tell a clean connection close
// apart from a malformed frame.
func readFrame(r io.Reader, dst any, maxSize int) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return io.EOF
		}
		return fmt.Errorf("capbroker: read frame header: %w", err)
	}
	length := binary.BigEndian.Uint32(header[:])
	if int64(length) > int64(maxSize) {
		return fmt.Errorf("capbroker: frame %d bytes exceeds cap %d", length, maxSize)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("capbroker: read frame body: %w", err)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("capbroker: decode frame: %w", err)
	}
	return nil
}

// --- per-method argv/result shapes ---
//
// One struct pair per RPC method (the sandbox.PrivilegedOps operations plus
// the run-launch methods), plus Ping (the orchestrator's readiness probe —
// not part of the interface). Adding a method = one pair here + one case in
// dispatch (server.go) + one method on IPCClient (ipc.go).

type emptyArgs struct{}
type emptyResult struct{}

type setupNetworkArgs struct {
	RunID     string `json:"run_id"`
	SubnetIdx uint8  `json:"subnet_idx"`
}

type setupNetworkResult struct {
	State sandbox.NetworkState `json:"state"`
}

type teardownNetworkArgs struct {
	State sandbox.NetworkState `json:"state"`
}

type ensureRootfsArgs struct {
	Selector sandbox.RootfsSelector `json:"selector"`
}

type ensureRootfsResult struct {
	Path string `json:"path"`
}

// launchRunArgs carries the narrow, validated launch data the broker folds
// into the OCI spec IT owns (Params.StdioSocketPath names the per-run stdio
// socket the orchestrator is already listening on). Deliberately NO bundle
// dir, config.json, rootfs path, or free command: the broker resolves the
// rootfs by catalog name, builds the spec from a fixed template, and
// validates every field (sandbox.ValidateLaunchParams) before it touches
// anything — so a compromised orchestrator can supply data the sandbox sees
// but cannot make the broker exec arbitrary code with capabilities.
// Params.ContainerID is the run's unique lifecycle key — the broker
// registers, waits, and kills by it, never by the non-unique RunID.
type launchRunArgs struct {
	Params sandbox.LaunchParams `json:"params"`
}

type waitRunArgs struct {
	ContainerID string `json:"container_id"`
}

// waitRunResult reports how a supervised run ended. ExitError is the
// runsc exit error rendered to a string (empty on clean exit); OOMKilled
// mirrors the pre-split cgroupOOMKilled read the orchestrator did itself.
//
// PeakMemMB / CPUUsec are the run's measured actuals, read from the same
// cgroup on the same exit path as the OOM attribution. They ride this
// result because the broker is the only process that can read them
// race-free — it owns the cgroup lifecycle, and the orchestrator never
// touches /sys/fs/cgroup. Absent (nil) whenever the kernel didn't offer the
// number: memory.peak needs ≥ 5.19, and a run that never had a cgroup (no
// memory limit) has nothing to measure — as is always true of the sidecar
// wait, which reuses this shape for an ordinary host process. Both are
// additive optional fields,
// so a version-skewed pairing across this change degrades to unrecorded
// actuals rather than a misread frame — which is why they did not need a
// ProtocolVersion bump.
type waitRunResult struct {
	ExitError string `json:"exit_error,omitempty"`
	OOMKilled bool   `json:"oom_killed,omitempty"`
	PeakMemMB *int   `json:"peak_mem_mb,omitempty"`
	CPUUsec   *int64 `json:"cpu_usec,omitempty"`
}

type killRunArgs struct {
	ContainerID string `json:"container_id"`
}

// launchSidecarArgs carries the validated launch data for one run's
// credential-sidecar process — the sibling of launchRunArgs for the
// per-run sidecar harness. Params.StdioSocketPath is populated the same
// way launchRunArgs.Params.StdioSocketPath is: by the orchestrator-side
// client, which owns the listener the broker dials.
type launchSidecarArgs struct {
	Params sandbox.SidecarLaunchParams `json:"params"`
}

// waitSidecarArgs / killSidecarArgs mirror waitRunArgs / killRunArgs
// exactly (both are just a registry key), kept as distinct wire types
// rather than reused ones so the RPC method list stays self-documenting —
// one struct pair per method, per this file's own stated convention.
type waitSidecarArgs struct {
	ContainerID string `json:"container_id"`
}

type killSidecarArgs struct {
	ContainerID string `json:"container_id"`
}

type chownRunTreeArgs struct {
	Root    string `json:"root"`
	Subpath string `json:"subpath,omitempty"`
}

type removeRunTreeArgs struct {
	Path string `json:"path"`
}

// captureRunDeltaArgs carries the fd-passthrough data for the streamed
// capture: StdoutSocketPath names the per-capture unix socket the
// orchestrator-side IPCClient is already listening on (validated with
// sandbox.ValidateCaptureStdoutSocketPath before the broker dials it) — the
// capture child's raw stdout streams straight to it over a passed-through
// fd, exactly like LaunchRun's StdioSocketPath, so the delta's bytes never
// ride this RPC's request/response frames at all.
type captureRunDeltaArgs struct {
	Worktree         string `json:"worktree"`
	StdoutSocketPath string `json:"stdout_socket_path"`
	// SessionID, when set, tells the capture child to also read the run's
	// Claude session transcript (owner-only to the sandbox uid) into the
	// emitted worktree.CapturedState. Empty for a run with no session.
	SessionID string `json:"session_id,omitempty"`
}

// captureRunDeltaResult carries only success/error plus a bounded stderr
// tail — diagnostics, never run data. The delta itself (a worktree.GitDelta
// the ORCHESTRATOR decodes; the broker never interprets it) crosses over
// the passed-through socket named in captureRunDeltaArgs, not this result,
// so unlike the pre-v2 shape this fits comfortably under maxFrameSize like
// every other method's result.
type captureRunDeltaResult struct {
	StderrTail string `json:"stderr_tail,omitempty"`
}

// methodCallNames are the wire-name constants shared by client and server.
const (
	methodPing            = "Ping"
	methodSetupNetwork    = "SetupNetwork"
	methodTeardownNetwork = "TeardownNetwork"
	methodEnsureRootfs    = "EnsureRootfs"
	methodReapOrphans     = "ReapOrphans"
	methodLaunchRun       = "LaunchRun"
	methodWaitRun         = "WaitRun"
	methodKillRun         = "KillRun"
	methodChownRunTree    = "ChownRunTree"
	methodRemoveRunTree   = "RemoveRunTree"
	methodCaptureRunDelta = "CaptureRunDelta"
	methodLaunchSidecar   = "LaunchSidecar"
	methodWaitSidecar     = "WaitSidecar"
	methodKillSidecar     = "KillSidecar"
)
