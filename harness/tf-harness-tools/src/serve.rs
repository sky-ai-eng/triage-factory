//! Resident tool-host `serve` mode: a long-lived process that executes the
//! seven ported tools (`read`, `bash`, `edit`, `write`, `grep`, `find`, `ls`)
//! on behalf of an out-of-process agent loop, over a unix stream socket.
//!
//! # Why resident
//!
//! In the native multi-mode runtime the agent loop runs *outside* the gVisor
//! jail and dispatches each tool call *into* it. A resident process behind a
//! pre-mounted socket keeps the dangerous, capability-holding supervisor
//! (`runsc exec` can only be driven by the cap-broker) out of the per-call hot
//! path: the broker's involvement stays at launch/supervise, and the hostile
//! agent-shaped traffic terminates in this zero-capability peer. The file tools
//! stay in-jail so symlink / path-traversal resolution over attacker-influenced
//! worktree content happens inside the Sentry, never on the host side of a bind
//! mount.
//!
//! Standalone by design: nothing here knows about sandboxes, mounts, or the
//! loop. `serve` binds a socket, accepts one connection, and answers frames.
//! The launch wiring (OCI main-process, socket bind-mount) lives with the loop.
//!
//! # Wire protocol
//!
//! The [`Request`] and [`Response`] doc comments below are the protocol
//! specification — there is no separate doc file.
//!
//! * **Transport** — a unix *stream* socket at a fixed path. `serve` is the
//!   listener; the loop connects. One connection is one engagement.
//! * **Framing** — length-prefixed JSON: a 4-byte big-endian unsigned length
//!   followed by exactly that many bytes of UTF-8 JSON. The length counts the
//!   JSON body only, not the 4-byte header. Frames larger than
//!   [`MAX_FRAME_BYTES`] are refused (a hostile length prefix cannot make the
//!   server allocate gigabytes). This mirrors the length-prefixed convention
//!   the Go `agenthost` IPC already uses on the credential-sidecar socket.
//! * **Request/response** — one [`Request`] frame in, one [`Response`] frame
//!   out, strictly paired by the caller-chosen `id`. Exactly one request is in
//!   flight at a time; the server reads, executes, and replies before touching
//!   the next frame, so tool executions never interleave (see
//!   [`serve_connection`]).
//! * **Lifecycle** — a clean EOF at a frame boundary (either direction) ends
//!   the engagement and `serve` returns. A response-write failure means the
//!   peer vanished mid-request; the loser contract is that the response simply
//!   never arrives and the client's own frame read fails on EOF. No partial
//!   frame is ever emitted — a frame is serialized in full and written from a
//!   single buffer, or the process dies before sending.
//! * **Bash children** — `bash` runs each command in its own session
//!   (`setsid`, via [`crate::shell`]) and [`crate::bash::execute`] waits for and
//!   reaps it before returning. Because dispatch is synchronous and serial, no
//!   bash child is ever alive at the moment `serve` observes EOF and exits, so
//!   an engagement teardown leaves no orphan. The process-group kill that
//!   backstops a runaway tree already lives in [`crate::shell::kill_process_tree`],
//!   invoked by `bash` on timeout.
//!
//! # Extensibility
//!
//! An unrecognized `tool` yields a structured protocol error with
//! `kind = "unknown_tool"` (see [`ErrorKind`]) rather than a tool result, so a
//! future verb (a subagent spawn, say) is addable without changing the framing.
//! No such verb exists yet; only the contract does.

use std::io::{self, Read, Write};
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::Path;

use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

use crate::definitions::ALL_TOOL_NAMES;

/// Upper bound on a single frame's JSON body, in bytes. Tool outputs are
/// truncation-bounded well under this; the cap exists so a malformed or hostile
/// length prefix can't drive an unbounded allocation. Matches the Go
/// `agenthost` frame cap so the two length-prefixed channels agree.
pub const MAX_FRAME_BYTES: usize = 16 * 1024 * 1024;

/// A request frame: one tool invocation.
///
/// ```json
/// { "id": <any>, "tool": "read", "args": { "path": "src/lib.rs" } }
/// ```
///
/// * `id` — an opaque token chosen by the caller and echoed verbatim on the
///   matching [`Response`]. The server never interprets it (number, string, or
///   any JSON value are all fine); it exists only so the caller can pair a
///   response with its request.
/// * `tool` — the tool name. Must be one of the seven the host implements
///   (`read`, `bash`, `edit`, `write`, `grep`, `find`, `ls`); anything else is
///   an `unknown_tool` protocol error.
/// * `args` — the tool's own JSON argument object, byte-for-byte the shape the
///   differential parity corpus pins. Absent `args` is treated as `{}` so the
///   tool's normal argument validation produces the model-facing message.
#[derive(Debug, Clone, Deserialize)]
pub struct Request {
    /// Opaque caller token, echoed on the response.
    #[serde(default)]
    pub id: Value,
    /// Tool name to dispatch.
    pub tool: String,
    /// Tool-specific argument object (the existing per-tool JSON schema).
    #[serde(default)]
    pub args: Value,
}

/// A response frame: the result of one tool invocation, or an error.
///
/// Success:
/// ```json
/// { "id": <echoed>, "ok": true, "result": { "content": [...], "details": {...} } }
/// ```
/// Failure:
/// ```json
/// { "id": <echoed>, "ok": false, "error": <string | object> }
/// ```
///
/// Exactly one of `result` / `error` is present. `result` is the tool's
/// pi-shaped `{content, details?}` object — identical bytes to what the
/// one-shot CLI prints, which is what lets the socket path stay byte-identical
/// to direct invocation.
///
/// `error` is deliberately polymorphic:
///
/// * a **string** when a tool ran and failed (edit target not found, bash
///   non-zero exit, …). This is the exact message the tool threw; the loop
///   surfaces it to the model as an `is_error` tool result. It is *not* a
///   transport failure — the RPC succeeded, the tool reported an error.
/// * an **object** `{ "kind": <ErrorKind>, "message": <string>, .. }` for a
///   protocol-level failure the tool layer never sees: an unknown tool, a
///   malformed request frame, an over-cap response. The stable `kind`
///   discriminator lets the loop branch without string-matching.
#[derive(Debug, Clone, Serialize)]
pub struct Response {
    /// The originating request's `id`, echoed. `null` when the request could
    /// not be parsed far enough to recover one.
    pub id: Value,
    /// `true` iff `result` is present.
    pub ok: bool,
    /// Present iff `ok`. The tool's `{content, details?}` object.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub result: Option<Value>,
    /// Present iff `!ok`. String for a tool failure, object for a protocol
    /// failure (see the type doc).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<Value>,
}

impl Response {
    /// A successful tool result.
    pub fn ok(id: Value, result: Value) -> Self {
        Response {
            id,
            ok: true,
            result: Some(result),
            error: None,
        }
    }

    /// A tool that ran and reported failure — `error` is the tool's own
    /// message string, surfaced to the model as an `is_error` result.
    pub fn tool_error(id: Value, message: String) -> Self {
        Response {
            id,
            ok: false,
            result: None,
            error: Some(Value::String(message)),
        }
    }

    /// A protocol-level failure — `error` is `{kind, message}` with a stable
    /// discriminator the loop can branch on.
    pub fn protocol_error(id: Value, kind: ErrorKind, message: impl Into<String>) -> Self {
        Response {
            id,
            ok: false,
            result: None,
            error: Some(json!({ "kind": kind.as_str(), "message": message.into() })),
        }
    }
}

/// Stable `kind` discriminators for protocol-level (non-tool) failures. The
/// string values are part of the wire contract; add variants, never rename.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ErrorKind {
    /// The `tool` field named a tool the host does not implement. The
    /// extensibility hook: a new verb becomes a new known name, and everything
    /// else keeps returning this.
    UnknownTool,
    /// The request frame's bytes were not a JSON object with a string `tool`
    /// field. The frame boundary was intact (a full length-delimited body was
    /// read), so the connection survives and the next frame is still read.
    MalformedRequest,
    /// A response body exceeded [`MAX_FRAME_BYTES`]. Defensive; truncation keeps
    /// real tool output far below the cap.
    ResponseTooLarge,
}

impl ErrorKind {
    /// The wire string for this kind.
    pub fn as_str(self) -> &'static str {
        match self {
            ErrorKind::UnknownTool => "unknown_tool",
            ErrorKind::MalformedRequest => "malformed_request",
            ErrorKind::ResponseTooLarge => "response_too_large",
        }
    }
}

/// Why a frame read stopped short of returning a body.
enum ReadOutcome {
    /// A complete frame body.
    Frame(Vec<u8>),
    /// Clean EOF at a frame boundary — the peer closed the write half between
    /// frames. Engagement over.
    Closed,
    /// The length prefix exceeded [`MAX_FRAME_BYTES`]. The stream can no longer
    /// be resynchronized (we don't know where the oversized body ends), so the
    /// caller reports the error and closes.
    TooLarge(u32),
    /// A transport error, or EOF *mid-frame* (a truncated header or body). The
    /// engagement is torn down; a half-delivered request is simply lost. The
    /// caller reacts identically to every cause, so no payload is carried.
    Io,
}

/// Read one length-prefixed frame from `r`.
///
/// Distinguishes a clean close at a frame boundary ([`ReadOutcome::Closed`])
/// from a truncated frame ([`ReadOutcome::Io`]) so the caller can tell an
/// orderly engagement end apart from a peer that died mid-send.
fn read_frame(r: &mut impl Read) -> ReadOutcome {
    let mut header = [0u8; 4];
    let mut filled = 0usize;
    while filled < header.len() {
        match r.read(&mut header[filled..]) {
            Ok(0) => {
                if filled == 0 {
                    return ReadOutcome::Closed;
                }
                return ReadOutcome::Io;
            }
            Ok(n) => filled += n,
            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(_) => return ReadOutcome::Io,
        }
    }
    let len = u32::from_be_bytes(header);
    if len as usize > MAX_FRAME_BYTES {
        return ReadOutcome::TooLarge(len);
    }
    let mut body = vec![0u8; len as usize];
    if r.read_exact(&mut body).is_err() {
        return ReadOutcome::Io;
    }
    ReadOutcome::Frame(body)
}

/// Serialize `resp` and write it as one length-prefixed frame.
///
/// The frame is built in a single buffer and written with one `write_all`, so
/// either the whole frame lands or the write errors — no valid-looking partial
/// frame is ever emitted. An over-cap body (a defensive impossibility given
/// truncation) is replaced by a small `response_too_large` protocol error so
/// the caller still gets a well-formed frame.
fn write_frame(w: &mut impl Write, resp: &Response) -> io::Result<()> {
    let body =
        serde_json::to_vec(resp).map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;
    if body.len() > MAX_FRAME_BYTES {
        let replacement = Response::protocol_error(
            resp.id.clone(),
            ErrorKind::ResponseTooLarge,
            format!(
                "response {} bytes exceeds frame cap {}",
                body.len(),
                MAX_FRAME_BYTES
            ),
        );
        // The replacement is tiny and cannot itself overflow the cap.
        return write_frame(w, &replacement);
    }
    let mut frame = Vec::with_capacity(4 + body.len());
    frame.extend_from_slice(&(body.len() as u32).to_be_bytes());
    frame.extend_from_slice(&body);
    w.write_all(&frame)?;
    w.flush()
}

/// Execute one request against `cwd`, producing the response to send back.
///
/// Pure and socket-free — the parsing, unknown-tool, and dispatch behavior is
/// unit-testable without a connection. Dispatch goes through
/// [`crate::run_tool`], the same entry point the one-shot CLI uses, so a tool's
/// result and error bytes are identical on both paths.
pub fn handle_request(body: &[u8], cwd: &str) -> Response {
    // Parse to a Value first so an otherwise-malformed request can still echo
    // its id, then into the typed [`Request`] frame.
    let value: Value = match serde_json::from_slice(body) {
        Ok(v) => v,
        Err(e) => {
            return Response::protocol_error(
                Value::Null,
                ErrorKind::MalformedRequest,
                format!("request frame is not valid JSON: {e}"),
            );
        }
    };
    let id = value.get("id").cloned().unwrap_or(Value::Null);
    let request: Request = match serde_json::from_value(value) {
        Ok(r) => r,
        Err(_) => {
            return Response::protocol_error(
                id,
                ErrorKind::MalformedRequest,
                "request is not a tool-call frame (needs a string \"tool\" field)",
            );
        }
    };
    if !ALL_TOOL_NAMES.contains(&request.tool.as_str()) {
        return Response::protocol_error(
            id,
            ErrorKind::UnknownTool,
            format!("unknown tool: {}", request.tool),
        );
    }
    // Absent (or null) args become an empty object so the tool's own argument
    // validation produces the model-facing message.
    let args = if request.args.is_null() {
        json!({})
    } else {
        request.args
    };
    match crate::run_tool(&request.tool, cwd, args) {
        Ok(result) => Response::ok(id, result),
        Err(err) => Response::tool_error(id, err.0),
    }
}

/// Drive one accepted connection to completion: read a frame, answer it, repeat
/// until the peer closes or a write fails.
///
/// Strictly serial — the next frame is not read until the current response is
/// written — which is what guarantees tool executions never interleave and
/// satisfies the "one in-flight request" contract by construction rather than
/// by rejecting a racing second request. `cwd` is the working directory every
/// tool call resolves against.
fn serve_connection(stream: UnixStream, cwd: &str) -> io::Result<()> {
    // Independent read/write handles over the same fd so a blocked reader and
    // the writer don't alias one buffered wrapper.
    let mut reader = io::BufReader::new(stream.try_clone()?);
    let mut writer = io::BufWriter::new(stream);
    loop {
        match read_frame(&mut reader) {
            ReadOutcome::Frame(body) => {
                let response = handle_request(&body, cwd);
                if write_frame(&mut writer, &response).is_err() {
                    // Peer vanished mid-request. The loser contract: the
                    // response never arrives, the client's read fails on EOF.
                    return Ok(());
                }
            }
            ReadOutcome::Closed => return Ok(()),
            ReadOutcome::TooLarge(len) => {
                // Best-effort: tell the peer why, then close — the stream can't
                // be resynchronized past an oversized body.
                let response = Response::protocol_error(
                    Value::Null,
                    ErrorKind::MalformedRequest,
                    format!("frame length {len} exceeds cap {MAX_FRAME_BYTES}; closing connection"),
                );
                let _ = write_frame(&mut writer, &response);
                return Ok(());
            }
            ReadOutcome::Io => return Ok(()),
        }
    }
}

/// Bind a unix stream socket at `socket_path`, serve a single engagement, and
/// return when it ends.
///
/// `cwd` is the working directory every tool call resolves against — the
/// worktree, in the real deployment — passed through verbatim to
/// [`crate::run_tool`] exactly as the one-shot CLI's `--cwd` is, so resolution
/// is identical on both paths and independent of the server's own process cwd.
/// One connection is accepted and served to EOF; additional connections are not
/// accepted (an engagement is one connection), so a second concurrent caller
/// cannot interleave.
///
/// Socket ownership/permission grants for a cross-uid peer are the launch
/// wiring's job (the loop ticket's mount helper, mirroring `agenthost`'s
/// `grantSocketToSandbox`); `serve` only binds and listens.
pub fn serve(socket_path: &Path, cwd: &str) -> io::Result<()> {
    // A stale socket file from a crashed predecessor would make bind fail with
    // EADDRINUSE. Only a socket is removed — never a regular file that happens
    // to sit at the path — so a misconfigured path fails loudly at bind.
    if let Ok(meta) = std::fs::symlink_metadata(socket_path) {
        use std::os::unix::fs::FileTypeExt;
        if meta.file_type().is_socket() {
            let _ = std::fs::remove_file(socket_path);
        }
    }

    let listener = UnixListener::bind(socket_path).map_err(|e| {
        io::Error::new(
            e.kind(),
            format!("serve: bind {}: {e}", socket_path.display()),
        )
    })?;

    let result = match listener.accept() {
        Ok((stream, _addr)) => serve_connection(stream, cwd),
        Err(e) => Err(e),
    };

    // Best-effort teardown: drop the listener and unlink the socket so a later
    // startup scan doesn't find a dangling path. The mount helper owns cleanup
    // in the real deployment; this keeps standalone runs tidy.
    drop(listener);
    let _ = std::fs::remove_file(socket_path);
    result
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ok_response_shape() {
        let r = Response::ok(json!(7), json!({"content": []}));
        let v = serde_json::to_value(&r).unwrap();
        assert_eq!(v["id"], json!(7));
        assert_eq!(v["ok"], json!(true));
        assert_eq!(v["result"], json!({"content": []}));
        assert!(v.get("error").is_none());
    }

    #[test]
    fn tool_error_is_a_string() {
        let r = Response::tool_error(json!("abc"), "boom".to_string());
        let v = serde_json::to_value(&r).unwrap();
        assert_eq!(v["ok"], json!(false));
        assert_eq!(v["error"], json!("boom"));
        assert!(v.get("result").is_none());
    }

    #[test]
    fn protocol_error_is_a_kinded_object() {
        let r = Response::protocol_error(Value::Null, ErrorKind::UnknownTool, "unknown tool: frob");
        let v = serde_json::to_value(&r).unwrap();
        assert_eq!(v["ok"], json!(false));
        assert_eq!(v["error"]["kind"], json!("unknown_tool"));
        assert_eq!(v["error"]["message"], json!("unknown tool: frob"));
    }

    #[test]
    fn unknown_tool_is_a_protocol_error_not_a_tool_error() {
        let resp = handle_request(br#"{"id":1,"tool":"frobnicate","args":{}}"#, ".");
        let v = serde_json::to_value(&resp).unwrap();
        assert_eq!(v["id"], json!(1));
        assert_eq!(v["error"]["kind"], json!("unknown_tool"));
    }

    #[test]
    fn malformed_json_recovers_null_id() {
        let resp = handle_request(b"not json at all", ".");
        let v = serde_json::to_value(&resp).unwrap();
        assert_eq!(v["id"], Value::Null);
        assert_eq!(v["error"]["kind"], json!("malformed_request"));
    }

    #[test]
    fn missing_tool_field_keeps_id() {
        let resp = handle_request(br#"{"id":"keep-me","args":{}}"#, ".");
        let v = serde_json::to_value(&resp).unwrap();
        assert_eq!(v["id"], json!("keep-me"));
        assert_eq!(v["error"]["kind"], json!("malformed_request"));
    }

    #[test]
    fn frame_roundtrips_through_read_and_write() {
        let resp = Response::ok(
            json!(42),
            json!({"content": [{"type": "text", "text": "hi"}]}),
        );
        let mut buf: Vec<u8> = Vec::new();
        write_frame(&mut buf, &resp).unwrap();
        // 4-byte header + body.
        let declared = u32::from_be_bytes([buf[0], buf[1], buf[2], buf[3]]) as usize;
        assert_eq!(declared, buf.len() - 4);
        let mut cursor = io::Cursor::new(buf);
        match read_frame(&mut cursor) {
            ReadOutcome::Frame(body) => {
                let v: Value = serde_json::from_slice(&body).unwrap();
                assert_eq!(v["id"], json!(42));
                assert_eq!(v["result"]["content"][0]["text"], json!("hi"));
            }
            _ => panic!("expected a frame"),
        }
    }

    #[test]
    fn empty_stream_reads_as_closed() {
        let mut cursor = io::Cursor::new(Vec::<u8>::new());
        assert!(matches!(read_frame(&mut cursor), ReadOutcome::Closed));
    }

    #[test]
    fn oversized_length_prefix_is_rejected() {
        // A 4-byte header claiming a body far past the cap, with no body bytes.
        let header = ((MAX_FRAME_BYTES as u64 + 1) as u32).to_be_bytes();
        let mut cursor = io::Cursor::new(header.to_vec());
        assert!(matches!(read_frame(&mut cursor), ReadOutcome::TooLarge(_)));
    }

    #[test]
    fn truncated_header_is_an_io_error_not_a_clean_close() {
        let mut cursor = io::Cursor::new(vec![0u8, 0u8]); // 2 of 4 header bytes
        assert!(matches!(read_frame(&mut cursor), ReadOutcome::Io));
    }
}
