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

// maxFrameSize caps one REQUEST frame. Every request payload is a handful
// of strings/ints (run ids, subnet indices, iptables rule bookkeeping) —
// far under this ceiling; it exists to protect the broker from a malformed
// client sending a bogus multi-GB length header.
const maxFrameSize = 1 * 1024 * 1024 // 1 MiB

// responseFrameSize caps one RESPONSE frame — read by the orchestrator,
// authored by the broker it already trusts, so the cap is a sanity rail
// rather than a defense. Larger than maxFrameSize for exactly one method:
// CaptureRunDelta's result embeds a git bundle of the run's local-only
// commits plus a binary patch of everything uncommitted, which a run that
// generated large artifacts can push far past 1 MiB. A capture larger than
// this fails with a clear frame-size error (the park degrades to
// snapshot-less) instead of ballooning broker memory without bound.
const responseFrameSize = 512 * 1024 * 1024 // 512 MiB

// ProtocolVersion is the wire-format version. The broker rejects a
// mismatching client version so an old binary talking to a new broker (or
// vice versa) surfaces a clear error instead of silently misbehaving — the
// same defensive handshake agenthost uses, load-bearing here too because
// the broker is a long-lived process that can outlive a binary upgrade
// until the next restart.
const ProtocolVersion = 1

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

// writeFrame serializes msg as a length-prefixed JSON frame on w, capped
// at maxSize (maxFrameSize for requests, responseFrameSize for responses).
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

// readFrame reads one length-prefixed JSON frame from r and decodes it
// into dst, capped at maxSize (maxFrameSize when the broker reads a
// request from an untrusted-post-compromise client; responseFrameSize
// when the client reads the trusted broker's response). EOF on the
// header read is returned verbatim so callers (the broker's accept loop)
// can tell a clean connection close apart from a malformed frame.
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
type waitRunResult struct {
	ExitError string `json:"exit_error,omitempty"`
	OOMKilled bool   `json:"oom_killed,omitempty"`
}

type killRunArgs struct {
	ContainerID string `json:"container_id"`
}

type chownRunTreeArgs struct {
	Root    string `json:"root"`
	Subpath string `json:"subpath,omitempty"`
}

type removeRunTreeArgs struct {
	Path string `json:"path"`
}

type captureRunDeltaArgs struct {
	Worktree string `json:"worktree"`
}

// captureRunDeltaResult carries the capture child's raw JSON stdout — a
// worktree.GitDelta the ORCHESTRATOR decodes (the broker never interprets
// it). Delta can embed a git bundle plus a binary patch of everything
// uncommitted, so unlike every other result here it is not "a handful of
// strings" — the client reads this response under captureFrameSize, not
// maxFrameSize.
type captureRunDeltaResult struct {
	Delta []byte `json:"delta,omitempty"`
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
)
