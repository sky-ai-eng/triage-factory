package agentloop

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestClassifyResponse(t *testing.T) {
	tests := []struct {
		name    string
		resp    serveResponse
		wantOut ToolOutcome
	}{
		{
			name:    "a successful result flattens its text blocks",
			resp:    serveResponse{OK: true, Result: json.RawMessage(`{"content":[{"type":"text","text":"one"},{"type":"text","text":"two"}]}`)},
			wantOut: ToolOutcome{Content: "one\ntwo"},
		},
		{
			name:    "a string error is the tool's own message",
			resp:    serveResponse{Error: json.RawMessage(`"file not found"`)},
			wantOut: ToolOutcome{ToolError: "file not found"},
		},
		{
			name:    "a kinded object is a protocol error",
			resp:    serveResponse{Error: json.RawMessage(`{"kind":"unknown_tool","message":"unknown tool: frob"}`)},
			wantOut: ToolOutcome{Protocol: &ProtocolError{Kind: "unknown_tool", Message: "unknown tool: frob"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := classifyResponse(tc.resp)
			if err != nil {
				t.Fatal(err)
			}
			if got.Content != tc.wantOut.Content || got.ToolError != tc.wantOut.ToolError {
				t.Fatalf("got %+v, want %+v", got, tc.wantOut)
			}
			if (got.Protocol == nil) != (tc.wantOut.Protocol == nil) {
				t.Fatalf("protocol error presence differs: %+v vs %+v", got.Protocol, tc.wantOut.Protocol)
			}
			if got.Protocol != nil && got.Protocol.Kind != tc.wantOut.Protocol.Kind {
				t.Fatalf("kind = %q, want %q", got.Protocol.Kind, tc.wantOut.Protocol.Kind)
			}
		})
	}
}

// TestProtocolErrorFatality pins the classification against the harness's
// own ErrorKind::is_fatal. Only the kind that leaves the stream
// unresynchronizable ends the engagement.
func TestProtocolErrorFatality(t *testing.T) {
	for kind, wantFatal := range map[string]bool{
		protoUnknownTool:      false,
		protoMalformedRequest: false,
		protoResponseTooLarge: false,
		protoRequestTooLarge:  true,
	} {
		if got := (&ProtocolError{Kind: kind}).Fatal(); got != wantFatal {
			t.Errorf("%s fatal = %v, want %v", kind, got, wantFatal)
		}
	}
}

func TestRenderToolResult_SplitsImagesFromText(t *testing.T) {
	raw := json.RawMessage(`{"content":[{"type":"text","text":"here"},{"type":"image","data":"AAA","mimeType":"image/png"}]}`)
	content, images, err := renderToolResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	if content != "here" {
		t.Errorf("content = %q, want %q", content, "here")
	}
	if len(images) != 1 || images[0].Data != "AAA" || images[0].MimeType != "image/png" {
		t.Fatalf("images = %+v", images)
	}
}

func TestFraming_RoundTrips(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	body := []byte(`{"id":1,"ok":true}`)
	go func() { _ = writeFrame(a, body) }()

	_ = b.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := readFrame(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("round trip = %q, want %q", got, body)
	}
}

func TestFraming_RefusesOverCapLengthPrefix(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	go func() {
		// A header claiming a body past the cap, with no body behind it.
		header := []byte{0xFF, 0xFF, 0xFF, 0xFF}
		_, _ = a.Write(header)
	}()
	_ = b.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := readFrame(b); err == nil {
		t.Fatal("an over-cap length prefix must be refused before allocating")
	}
}

// TestServeSocket_EndToEnd drives the real compiled tool host over its real
// socket. It skips when the harness has not been built, so the default
// `go test ./...` needs no Rust toolchain.
func TestServeSocket_EndToEnd(t *testing.T) {
	bin := harnessBinary(t)
	if bin == "" {
		t.Skip("tf-harness-tools is not built; run `cargo build` in harness/tf-harness-tools")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("line one\nline two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(t.TempDir(), "tools.sock")

	// This side binds and the host dials in, which is the production
	// direction: the jail runs under gVisor's --host-uds=open and can connect
	// to a host socket but not create one. Binding before the spawn also
	// removes the race the old direction had to poll around.
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("bind tool host socket: %v", err)
	}
	defer listener.Close()

	conn := startAndAccept(t, listener, exec.Command(bin, "serve", "--connect", sock, "--cwd", dir))
	host := NewToolHost(conn, 10*time.Second)
	defer host.Close()

	t.Run("a successful call returns the tool's bytes", func(t *testing.T) {
		out, err := host.Call("read", map[string]any{"path": "hello.txt"})
		if err != nil {
			t.Fatal(err)
		}
		if out.ToolError != "" || out.Protocol != nil {
			t.Fatalf("unexpected failure: %+v", out)
		}
		if out.Content == "" {
			t.Fatal("expected the file's contents")
		}
	})

	t.Run("a failing tool returns its own message, not a protocol error", func(t *testing.T) {
		out, err := host.Call("read", map[string]any{"path": "nope.txt"})
		if err != nil {
			t.Fatal(err)
		}
		if out.ToolError == "" {
			t.Fatalf("expected a tool error, got %+v", out)
		}
		if out.Protocol != nil {
			t.Fatalf("a tool that ran and failed is not a protocol failure: %+v", out.Protocol)
		}
	})

	t.Run("the configure verb is accepted by the real host", func(t *testing.T) {
		// The cross-language check: the loop's frame for a verb that is not a
		// tool is the frame the Rust host parses, and its answer classifies as
		// an ordinary success rather than unknown_tool. Nothing else compares
		// the two sides of this contract.
		out, err := host.Call(toolHostConfigureTool, map[string]any{bashMemBudgetArg: 2048})
		if err != nil {
			t.Fatal(err)
		}
		if out.Protocol != nil {
			t.Fatalf("the host does not implement the configure verb: %+v", out.Protocol)
		}
		if out.ToolError != "" {
			t.Fatalf("the host rejected the configure args: %s", out.ToolError)
		}
		if out.Content != "" {
			t.Errorf("configure answered with content %q, want the empty result", out.Content)
		}
	})

	t.Run("an unknown tool is a survivable protocol error", func(t *testing.T) {
		out, err := host.Call("frobnicate", nil)
		if err != nil {
			t.Fatal(err)
		}
		if out.Protocol == nil || out.Protocol.Kind != protoUnknownTool {
			t.Fatalf("expected unknown_tool, got %+v", out)
		}
		if out.Protocol.Fatal() {
			t.Fatal("unknown_tool must be survivable")
		}
		// The connection survives: the next call still works.
		if _, err := host.Call("ls", nil); err != nil {
			t.Fatalf("the connection must survive a survivable error: %v", err)
		}
	})
}

// harnessBinary locates the compiled tool host, or "" when it has not been
// built.
// startAndAccept spawns a tool host that dials this listener and returns its
// connection, failing the test rather than blocking when it never arrives.
//
// The bare Accept this replaces turned any non-dialing child into a hang until
// the package timeout — ten minutes of nothing, for a child that had already
// exited with a one-line reason on stderr. Watching the process is what turns
// that into an immediate, self-explaining failure, and it is the same guard
// ToolHostJail.Accept carries in production for the same reason.
func startAndAccept(t *testing.T, listener net.Listener, cmd *exec.Cmd) net.Conn {
	t.Helper()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	conns := make(chan net.Conn, 1)
	go func() {
		c, err := listener.Accept()
		if err == nil {
			conns <- c
		}
		close(conns)
	}()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	select {
	case c := <-conns:
		if c == nil {
			t.Fatal("accepting the tool host's connection failed")
		}
		return c
	case err := <-exited:
		t.Fatalf("the tool host exited (%v) instead of dialing in; stderr: %s", err, stderr.String())
	case <-time.After(15 * time.Second):
		t.Fatal("the tool host neither dialed in nor exited")
	}
	return nil
}

// harnessBinary returns the freshest compiled tool host, or "" when none is
// built.
//
// Freshest, not first-found: the crate builds into two profiles and the stale
// one is a live hazard, because a binary predating a CLI change accepts none
// of the current arguments and exits on startup. Picking by mtime means a
// `cargo build` in either profile is what the test runs, rather than whichever
// profile this function happened to look at first.
func harnessBinary(t *testing.T) string {
	t.Helper()
	// The crate lives in a cargo workspace, so the target dir is the
	// workspace's; check the crate-local one too for a standalone build.
	roots := [][]string{
		{"..", "..", "harness", "target"},
		{"..", "..", "harness", "tf-harness-tools", "target"},
	}
	var newest string
	var newestAt time.Time
	for _, root := range roots {
		for _, profile := range []string{"debug", "release"} {
			p := filepath.Join(append(append([]string{}, root...), profile, "tf-harness-tools")...)
			fi, err := os.Stat(p)
			if err != nil {
				continue
			}
			if newest == "" || fi.ModTime().After(newestAt) {
				if abs, absErr := filepath.Abs(p); absErr == nil {
					newest, newestAt = abs, fi.ModTime()
				}
			}
		}
	}
	return newest
}
