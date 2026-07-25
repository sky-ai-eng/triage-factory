package agentloop

import (
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

	cmd := exec.Command(bin, "serve", "--socket", sock, "--cwd", dir)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	var host ToolHost
	deadline := time.Now().Add(10 * time.Second)
	for {
		h, err := DialToolHost(sock, 10*time.Second)
		if err == nil {
			host = h
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tool host never accepted a connection: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
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
func harnessBinary(t *testing.T) string {
	t.Helper()
	// The crate lives in a cargo workspace, so the target dir is the
	// workspace's; check the crate-local one too for a standalone build.
	roots := [][]string{
		{"..", "..", "harness", "target"},
		{"..", "..", "harness", "tf-harness-tools", "target"},
	}
	for _, root := range roots {
		for _, profile := range []string{"debug", "release"} {
			p := filepath.Join(append(append([]string{}, root...), profile, "tf-harness-tools")...)
			if _, err := os.Stat(p); err != nil {
				continue
			}
			if abs, err := filepath.Abs(p); err == nil {
				return abs
			}
		}
	}
	return ""
}
