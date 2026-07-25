package agentloop

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// The serve-protocol frame cap, matching the harness's MAX_FRAME_BYTES. A
// length prefix past this is refused before any allocation, so a corrupt or
// hostile header can't drive an unbounded read on this side either.
const maxFrameBytes = 16 * 1024 * 1024

// Protocol-error kinds the tool host reports for failures the tool layer
// never sees. The strings are the wire contract; fatality mirrors the
// harness's ErrorKind::is_fatal, which is the authoritative classification —
// a kind added there and not here reads as survivable, which is the safe
// default only because the harness closes the connection on a fatal kind and
// the next frame read then fails on EOF regardless.
const (
	protoUnknownTool      = "unknown_tool"
	protoMalformedRequest = "malformed_request"
	protoRequestTooLarge  = "request_too_large"
	protoResponseTooLarge = "response_too_large"
)

// ProtocolError is a tool-host failure below the tool layer: the request
// never reached a tool, or its response could not be delivered.
type ProtocolError struct {
	Kind    string
	Message string
}

func (e *ProtocolError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("tool host protocol error (%s): %s", e.Kind, e.Message)
	}
	return "tool host protocol error (" + e.Kind + ")"
}

// Fatal reports whether the tool host closes the connection after sending
// this kind — i.e. whether the engagement is over. Only request_too_large is
// fatal by construction: the stream cannot be resynchronized past a body the
// server refused to read.
func (e *ProtocolError) Fatal() bool {
	return e.Kind == protoRequestTooLarge
}

// ToolOutcome is one dispatched call's result, already classified.
//
// Exactly one of the three is set. Content is the model-facing text of a
// successful call; ToolError is the message a tool that ran and failed threw
// (an ordinary is_error result — the RPC succeeded); Protocol is a
// transport-level failure whose Fatal() decides whether the engagement can
// continue.
type ToolOutcome struct {
	Content   string
	Images    []ToolImage
	ToolError string
	Protocol  *ProtocolError
}

// ToolImage is a non-text content block a tool returned (the read tool on an
// image file), carried as base64 data + mime type exactly as the harness
// emits it.
type ToolImage struct {
	Data     string
	MimeType string
}

// ToolHost is the loop's dispatch surface into the jail. The engine holds
// one; tests supply a fake.
type ToolHost interface {
	// Call dispatches one tool invocation and returns its classified
	// outcome. A non-nil error is a transport failure with no frame to
	// classify (the connection died, the write failed) — the engine treats
	// it exactly as it treats a fatal protocol error.
	Call(name string, args map[string]any) (ToolOutcome, error)
	// Close ends the engagement's connection. The host observes EOF at a
	// frame boundary and exits.
	Close() error
}

// socketToolHost is the production ToolHost: one unix-stream connection to
// the resident `tf-harness-tools serve` process inside the jail, speaking
// the length-prefixed JSON frame protocol.
//
// One connection is one engagement, and the protocol allows exactly one
// in-flight request, so calls serialize on a mutex. That is not a
// concession — the loop dispatches a batch's calls in order anyway, because
// a tool's effects are visible to the next tool.
type socketToolHost struct {
	mu   sync.Mutex
	conn net.Conn
	seq  int64
	// timeout bounds one request/response round trip. A tool that legitimately
	// runs long (a test suite under `bash`) is bounded by the harness's own
	// per-tool timeout; this is the backstop for a host that stopped answering
	// entirely, so it is generous.
	timeout time.Duration
}

// DialToolHost connects to the resident tool host's socket. The socket is
// created by the host inside the jail and reaches this side through a bind
// mount, so a dial can legitimately race the host's bind — the caller
// (setup) retries.
func DialToolHost(socketPath string, timeout time.Duration) (ToolHost, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("agentloop: dial tool host at %s: %w", socketPath, err)
	}
	if timeout <= 0 {
		timeout = defaultToolCallTimeout
	}
	return &socketToolHost{conn: conn, timeout: timeout}, nil
}

// defaultToolCallTimeout bounds one round trip when the caller names none.
// Well above any tool's own timeout: this catches a host that died or wedged,
// not a slow command.
const defaultToolCallTimeout = 30 * time.Minute

func (h *socketToolHost) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conn.Close()
}

// serveRequest / serveResponse are the wire frames. `id` is opaque to the
// host and echoed verbatim; we use a monotonically increasing integer and
// verify the echo so a desynchronized stream is detected rather than
// silently mis-attributed to the wrong call.
type serveRequest struct {
	ID   int64          `json:"id"`
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

type serveResponse struct {
	ID     int64           `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

func (h *socketToolHost) Call(name string, args map[string]any) (ToolOutcome, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.seq++
	id := h.seq
	if args == nil {
		// The host treats absent args as {} and lets the tool's own argument
		// validation produce the model-facing message. Send the empty object
		// explicitly so the frame is always well-shaped.
		args = map[string]any{}
	}
	body, err := json.Marshal(serveRequest{ID: id, Tool: name, Args: args})
	if err != nil {
		return ToolOutcome{}, fmt.Errorf("agentloop: marshal tool request: %w", err)
	}

	deadline := time.Now().Add(h.timeout)
	if err := h.conn.SetWriteDeadline(deadline); err != nil {
		return ToolOutcome{}, fmt.Errorf("agentloop: set write deadline: %w", err)
	}
	if err := writeFrame(h.conn, body); err != nil {
		return ToolOutcome{}, fmt.Errorf("agentloop: write tool request: %w", err)
	}
	if err := h.conn.SetReadDeadline(deadline); err != nil {
		return ToolOutcome{}, fmt.Errorf("agentloop: set read deadline: %w", err)
	}
	respBody, err := readFrame(h.conn)
	if err != nil {
		// The loser contract: the host vanished mid-request, so the response
		// never arrives and this read fails. The call may or may not have run;
		// the engine surfaces that to the model rather than re-dispatching.
		return ToolOutcome{}, fmt.Errorf("agentloop: read tool response: %w", err)
	}

	var resp serveResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return ToolOutcome{}, fmt.Errorf("agentloop: decode tool response: %w", err)
	}
	if resp.ID != id {
		// A mismatched echo means the stream is no longer paired. Never try to
		// recover by reading again — a wrong pairing would attribute one tool's
		// output to another call.
		return ToolOutcome{}, fmt.Errorf("agentloop: tool response id %d does not match request %d; stream desynchronized", resp.ID, id)
	}
	return classifyResponse(resp)
}

// classifyResponse sorts one response frame into the three outcomes.
func classifyResponse(resp serveResponse) (ToolOutcome, error) {
	if resp.OK {
		content, images, err := renderToolResult(resp.Result)
		if err != nil {
			return ToolOutcome{}, err
		}
		return ToolOutcome{Content: content, Images: images}, nil
	}
	// `error` is polymorphic: a string when a tool ran and failed, an object
	// with a stable `kind` when the failure is protocol-level.
	var asString string
	if err := json.Unmarshal(resp.Error, &asString); err == nil {
		return ToolOutcome{ToolError: asString}, nil
	}
	var asProto struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(resp.Error, &asProto); err != nil || asProto.Kind == "" {
		// Neither shape. Treat it as a protocol failure of unknown kind rather
		// than a tool error: an unrecognized envelope is not something to hand
		// the model as a tool's own message.
		return ToolOutcome{Protocol: &ProtocolError{
			Kind:    "unrecognized",
			Message: string(resp.Error),
		}}, nil
	}
	return ToolOutcome{Protocol: &ProtocolError{Kind: asProto.Kind, Message: asProto.Message}}, nil
}

// toolResultBlock is one entry of the harness's pi-shaped
// `{content: [...], details?}` result.
type toolResultBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// renderToolResult flattens a successful result into the model-facing text
// plus any image blocks. Text blocks join with newlines — the same
// concatenation the harness's own text_output helper performs — so the
// socket path and a direct CLI invocation put identical bytes in front of
// the model.
func renderToolResult(raw json.RawMessage) (string, []ToolImage, error) {
	var result struct {
		Content []toolResultBlock `json:"content"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", nil, fmt.Errorf("agentloop: decode tool result: %w", err)
	}
	var texts []string
	var images []ToolImage
	for _, b := range result.Content {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "image":
			images = append(images, ToolImage{Data: b.Data, MimeType: b.MimeType})
		}
	}
	return joinLines(texts), images, nil
}

func joinLines(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "\n" + p
	}
	return out
}

// writeFrame emits one length-prefixed frame from a single buffer, so either
// the whole frame lands or the write errors — never a valid-looking partial.
func writeFrame(w io.Writer, body []byte) error {
	if len(body) > maxFrameBytes {
		return fmt.Errorf("frame body %d bytes exceeds cap %d", len(body), maxFrameBytes)
	}
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(body)))
	copy(frame[4:], body)
	_, err := w.Write(frame)
	return err
}

// readFrame reads one length-prefixed frame. An over-cap length prefix is
// refused before allocating, and the connection is unusable afterward (the
// body's extent is unknown), which the caller surfaces as a transport error.
func readFrame(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if int(n) > maxFrameBytes {
		return nil, fmt.Errorf("frame length %d exceeds cap %d", n, maxFrameBytes)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}
