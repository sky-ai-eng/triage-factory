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

// maxFrameSize caps one wire frame. Every payload here is a handful of
// strings/ints (run ids, subnet indices, iptables rule bookkeeping) — far
// under this ceiling; it exists to protect the broker from a malformed
// client sending a bogus multi-GB length header.
const maxFrameSize = 1 * 1024 * 1024 // 1 MiB

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

// writeFrame serializes msg as a length-prefixed JSON frame on w.
func writeFrame(w io.Writer, msg any) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("capbroker: marshal frame: %w", err)
	}
	if len(body) > maxFrameSize {
		return fmt.Errorf("capbroker: frame %d bytes exceeds cap %d", len(body), maxFrameSize)
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
// into dst. EOF on the header read is returned verbatim so callers (the
// broker's accept loop) can tell a clean connection close apart from a
// malformed frame.
func readFrame(r io.Reader, dst any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return io.EOF
		}
		return fmt.Errorf("capbroker: read frame header: %w", err)
	}
	length := binary.BigEndian.Uint32(header[:])
	if length > maxFrameSize {
		return fmt.Errorf("capbroker: frame %d bytes exceeds cap %d", length, maxFrameSize)
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
// One struct pair per sandbox.PrivilegedOps method, plus Ping (the
// orchestrator's readiness probe — not part of the PrivilegedOps
// interface). Adding a method = one pair here + one case in dispatch (server.go)
// + one method on IPCClient (ipc.go).

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

type setupRunCgroupArgs struct {
	Name    string `json:"name"`
	LimitMB int    `json:"limit_mb"`
}

// setupRunCgroupResult carries only the directory path — the
// sandbox.PrivilegedOps.SetupRunCgroup fd is meaningful only to a caller
// in the same address space as the create (see that method's doc), and a
// raw fd number crosses a process boundary as garbage without SCM_RIGHTS
// (deliberately not introduced — spec §6). IPCClient reopens the returned
// path locally instead; see ipc.go.
type setupRunCgroupResult struct {
	Dir string `json:"dir"`
}

type removeRunCgroupArgs struct {
	Dir string `json:"dir"`
}

// methodCallNames are the wire-name constants shared by client and server.
const (
	methodPing            = "Ping"
	methodSetupNetwork    = "SetupNetwork"
	methodTeardownNetwork = "TeardownNetwork"
	methodEnsureRootfs    = "EnsureRootfs"
	methodSetupRunCgroup  = "SetupRunCgroup"
	methodRemoveRunCgroup = "RemoveRunCgroup"
	methodReapOrphans     = "ReapOrphans"
)
