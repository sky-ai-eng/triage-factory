// cmd/exec-test-stub is a tiny pure-Go stand-in for the real
// `triagefactory exec ...` binary used by the sandbox-side
// integration test (internal/sandbox/integration_linux_test.go's
// TestIntegration_AgentHostIPC).
//
// The real binary is glibc-linked on most dev/prod systems; bind-
// mounting it into an alpine (musl) rootfs makes it fail to exec
// because the dynamic loader can't resolve. The production fix is a
// static-built musl image of the real binary. For
// this ticket's acceptance, the integration test just needs to prove
// the IPC pipe works end-to-end — bind-mount socket, exec a binary
// inside the sandbox, round-trip an RPC over the socket.
//
// What it does:
//  1. Connects to /run/tf.sock (the canonical in-sandbox bind-mount
//     destination, matching agenthost.DefaultSocketPath).
//  2. Sends a single LookupConversation RPC using the exact wire format
//     cmd/exec/agenthost's IPCClient emits.
//  3. Prints the response as a JSON line on stdout and exits 0 on
//     success, non-zero with a stderr message on any failure.
//
// Compiled with CGO_ENABLED=0 so it's a pure-Go static binary that
// runs under both alpine (musl) and glibc rootfs — the integration
// test builds it on the fly via `go build`.
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

const (
	socketPath      = "/run/tf.sock"
	protocolVersion = 1
	method          = "LookupConversation"
	dialTimeout     = 5 * time.Second
	rpcTimeout      = 10 * time.Second
)

// request mirrors cmd/exec/agenthost.request. Inlined rather than
// imported because the stub is intentionally a standalone binary
// that doesn't drag in the rest of TF — that's what makes it
// musl-compatible.
type request struct {
	Version uint32          `json:"v"`
	Method  string          `json:"m"`
	Args    json.RawMessage `json:"a,omitempty"`
}

type response struct {
	Result json.RawMessage `json:"r,omitempty"`
	Error  string          `json:"e,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "exec-test-stub: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	conn, err := net.DialTimeout("unix", socketPath, dialTimeout)
	if err != nil {
		return fmt.Errorf("dial %s: %w", socketPath, err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(rpcTimeout)); err != nil {
		return fmt.Errorf("set deadline: %w", err)
	}

	req := request{Version: protocolVersion, Method: method, Args: json.RawMessage("{}")}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	if err := writeFrame(conn, body); err != nil {
		return fmt.Errorf("write request: %w", err)
	}
	rawResp, err := readFrame(conn)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	var resp response
	if err := json.Unmarshal(rawResp, &resp); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("daemon error: %s", resp.Error)
	}
	// Echo the result JSON to stdout — the test asserts on the
	// presence of the conversation id we registered on the host side.
	fmt.Printf("%s\n", string(resp.Result))
	return nil
}

func writeFrame(w io.Writer, body []byte) error {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(header[:])
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}
