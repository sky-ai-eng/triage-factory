//! Soak + robustness suite for `tf-harness-tools serve`.
//!
//! These drive a real `serve` process over a real unix socket — the same
//! bytes, and the same connection direction, the out-of-process agent loop
//! uses: the harness binds and `serve` dials in. They assert the protocol
//! holds under stress and hostile input: rapid sequential calls, oversized
//! (truncated) output, a client that dies mid-request, a server that dies
//! mid-request, an unknown tool, and malformed frames. The server must answer
//! with a protocol error or close cleanly on any bad input — never crash into a
//! panic.
//!
//! Result parity with direct invocation (the socket path returns byte-identical
//! bytes to calling the tool in-process) is checked here for the truncation
//! cases; the full 81-case corpus parity lives in the differential harness
//! (`parity/parity.ts --mode socket`).

use std::io::{self, Read, Write};
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::Path;
use std::process::{Child, Command, Stdio};
use std::time::{Duration, Instant};

use serde_json::{json, Value};
use tempfile::TempDir;

const BIN: &str = env!("CARGO_BIN_EXE_tf-harness-tools");

// ------------------------------------------------------------------ harness

/// A running `serve` process with the listener it dialed into and a scratch
/// working directory.
struct ServeProc {
    child: Child,
    _sockdir: TempDir,
    listener: UnixListener,
    work: TempDir,
}

impl ServeProc {
    /// Bind the socket, then spawn `serve --connect <sock> --cwd <work>`.
    ///
    /// The harness owns the listener because the jail cannot bind one — see the
    /// `serve` module header. Binding BEFORE the spawn also removes the race
    /// the old direction had to retry around: the child's connect either
    /// succeeds immediately or fails loudly, with nothing to poll for.
    ///
    /// The socket lives in its own directory so it never shows up in
    /// `ls`/`find` over the work dir.
    fn start() -> ServeProc {
        let sockdir = TempDir::new().unwrap();
        let work = TempDir::new().unwrap();
        let sock = sockdir.path().join("toolhost.sock");
        let listener = UnixListener::bind(&sock).expect("bind toolhost socket");
        let child = Command::new(BIN)
            .arg("serve")
            .arg("--connect")
            .arg(&sock)
            .arg("--cwd")
            .arg(work.path())
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::piped())
            .spawn()
            .expect("spawn serve");
        ServeProc {
            child,
            _sockdir: sockdir,
            listener,
            work,
        }
    }

    fn work_path(&self) -> &str {
        self.work.path().to_str().unwrap()
    }

    /// Accept the connection `serve` dials in, bounded so a child that never
    /// dials fails the test instead of hanging it.
    fn accept(&self) -> UnixStream {
        self.listener.set_nonblocking(true).unwrap();
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            match self.listener.accept() {
                Ok((stream, _)) => {
                    self.listener.set_nonblocking(false).unwrap();
                    stream.set_nonblocking(false).unwrap();
                    stream
                        .set_read_timeout(Some(Duration::from_secs(20)))
                        .unwrap();
                    stream
                        .set_write_timeout(Some(Duration::from_secs(20)))
                        .unwrap();
                    return stream;
                }
                Err(ref e) if e.kind() == io::ErrorKind::WouldBlock => {
                    if Instant::now() >= deadline {
                        panic!("serve never dialed in");
                    }
                    std::thread::sleep(Duration::from_millis(10));
                }
                Err(e) => panic!("accepting serve's connection failed: {e}"),
            }
        }
    }

    /// Wait for the process to exit within `dur`, returning its status and
    /// captured stderr. Fails the test if it doesn't exit in time.
    fn expect_exit(mut self, dur: Duration) -> (std::process::ExitStatus, String) {
        let deadline = Instant::now() + dur;
        loop {
            match self.child.try_wait().unwrap() {
                Some(status) => {
                    let mut stderr = String::new();
                    if let Some(mut e) = self.child.stderr.take() {
                        let _ = e.read_to_string(&mut stderr);
                    }
                    return (status, stderr);
                }
                None if Instant::now() < deadline => {
                    std::thread::sleep(Duration::from_millis(10));
                }
                None => {
                    let _ = self.child.kill();
                    panic!("serve did not exit within {dur:?}");
                }
            }
        }
    }
}

impl Drop for ServeProc {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

fn write_frame(stream: &mut UnixStream, body: &[u8]) -> io::Result<()> {
    let mut buf = Vec::with_capacity(4 + body.len());
    buf.extend_from_slice(&(body.len() as u32).to_be_bytes());
    buf.extend_from_slice(body);
    stream.write_all(&buf)?;
    stream.flush()
}

fn read_frame(stream: &mut UnixStream) -> io::Result<Vec<u8>> {
    let mut header = [0u8; 4];
    stream.read_exact(&mut header)?;
    let len = u32::from_be_bytes(header) as usize;
    let mut body = vec![0u8; len];
    stream.read_exact(&mut body)?;
    Ok(body)
}

/// One request → one response, decoded as JSON.
fn call(stream: &mut UnixStream, req: &Value) -> Value {
    write_frame(stream, req.to_string().as_bytes()).expect("write request frame");
    let body = read_frame(stream).expect("read response frame");
    serde_json::from_slice(&body).expect("response is JSON")
}

fn write_file(dir: &str, rel: &str, content: &str) {
    let path = Path::new(dir).join(rel);
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent).unwrap();
    }
    std::fs::write(path, content).unwrap();
}

/// The exact bytes calling a tool in-process (i.e. what the one-shot CLI would
/// print in its `result` field) produces — the direct-invocation reference the
/// socket result must match.
fn direct_result(tool: &str, cwd: &str, args: Value) -> Value {
    serde_json::to_value(tf_harness_tools::run_tool(tool, cwd, args).unwrap()).unwrap()
}

/// Replace `/tmp/tf-bash-<hex>.log` spill paths (random per call) with a stable
/// placeholder so two truncated bash results compare by their content, not the
/// per-run temp file name.
fn normalize_spill(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    let mut rest = s;
    while let Some(idx) = rest.find("/tmp/tf-bash-") {
        out.push_str(&rest[..idx]);
        let tail = &rest[idx..];
        match tail.find(".log") {
            Some(end) => {
                out.push_str("$TMPFILE");
                rest = &tail[end + ".log".len()..];
            }
            None => {
                out.push_str(tail);
                rest = "";
            }
        }
    }
    out.push_str(rest);
    out
}

// -------------------------------------------------------------------- tests

#[test]
fn rapid_sequential_calls_stay_paired_and_alive() {
    let proc = ServeProc::start();
    let cwd = proc.work_path().to_string();
    write_file(&cwd, "seed.txt", "hello\nworld\n");
    let mut stream = proc.accept();

    // Many back-to-back requests on one connection; each response must carry
    // its own echoed id and the connection must survive the whole run.
    for i in 0..300u64 {
        let req = match i % 5 {
            0 => json!({ "id": i, "tool": "read", "args": { "path": "seed.txt" } }),
            1 => json!({ "id": i, "tool": "ls", "args": { "path": &cwd } }),
            2 => json!({ "id": i, "tool": "bash", "args": { "command": format!("echo n{i}") } }),
            3 => {
                json!({ "id": i, "tool": "write", "args": { "path": format!("out/{i}.txt"), "content": format!("{i}") } })
            }
            _ => json!({ "id": i, "tool": "grep", "args": { "pattern": "hello", "path": &cwd } }),
        };
        let resp = call(&mut stream, &req);
        assert_eq!(resp["id"], json!(i), "id must echo for request {i}");
        assert_eq!(resp["ok"], json!(true), "request {i} failed: {resp}");
        assert!(resp.get("result").is_some(), "request {i} missing result");
    }

    // Clean close ends the engagement; the server exits on its own.
    drop(stream);
    let (status, stderr) = proc.expect_exit(Duration::from_secs(5));
    assert!(status.success(), "serve exited non-zero: {status:?}");
    assert!(!stderr.contains("panic"), "serve panicked: {stderr}");
}

#[test]
fn oversized_read_output_is_byte_identical_to_direct() {
    let proc = ServeProc::start();
    let cwd = proc.work_path().to_string();
    // 2500 lines trips the read line-truncation path — deterministic, no spill
    // file, so the socket result must equal the direct result byte-for-byte.
    let big: String = (1..=2500)
        .map(|i| format!("Line {i}"))
        .collect::<Vec<_>>()
        .join("\n");
    write_file(&cwd, "big.txt", &big);

    let mut stream = proc.accept();
    let resp = call(
        &mut stream,
        &json!({ "id": "r", "tool": "read", "args": { "path": "big.txt" } }),
    );
    assert_eq!(resp["ok"], json!(true), "read failed: {resp}");

    let direct = direct_result("read", &cwd, json!({ "path": "big.txt" }));
    assert_eq!(
        resp["result"], direct,
        "socket read result diverged from direct"
    );
}

#[test]
fn oversized_bash_output_truncates_like_direct() {
    let proc = ServeProc::start();
    let cwd = proc.work_path().to_string();
    let mut stream = proc.accept();

    // `seq 3000` blows the line budget and spills to a temp file. The spill
    // path is random per call, so compare with it normalized away — everything
    // else (the truncated body, the notice, the truncation details) must match.
    let args = json!({ "command": "seq 3000" });
    let resp = call(
        &mut stream,
        &json!({ "id": 1, "tool": "bash", "args": args.clone() }),
    );
    assert_eq!(resp["ok"], json!(true), "bash failed: {resp}");

    let direct = direct_result("bash", &cwd, args);
    let socket_norm = normalize_spill(&resp["result"].to_string());
    let direct_norm = normalize_spill(&direct.to_string());
    assert_eq!(
        socket_norm, direct_norm,
        "socket bash truncation diverged from direct"
    );
    // Sanity: it really did truncate and reference a spill file.
    assert!(
        socket_norm.contains("$TMPFILE"),
        "expected a spill-file reference"
    );
}

#[test]
fn unknown_tool_is_a_structured_protocol_error() {
    let proc = ServeProc::start();
    let mut stream = proc.accept();
    let resp = call(
        &mut stream,
        &json!({ "id": 9, "tool": "frobnicate", "args": {} }),
    );
    assert_eq!(resp["id"], json!(9));
    assert_eq!(resp["ok"], json!(false));
    assert_eq!(resp["error"]["kind"], json!("unknown_tool"));
    // The connection survives a protocol error; a good request still answers.
    let ok = call(
        &mut stream,
        &json!({ "id": 10, "tool": "bash", "args": { "command": "echo hi" } }),
    );
    assert_eq!(ok["ok"], json!(true));
}

/// The configure frame end to end: policy the peer sets over the socket
/// reaches a real `bash` child, kills it, and leaves the engagement alive.
///
/// Linux-only for the memory hog (`tail` buffering a newline-free stream) and
/// the `/proc` sampler behind it — the frame itself is portable, its effect is
/// not.
#[cfg(target_os = "linux")]
#[test]
fn a_configured_budget_kills_a_hog_without_ending_the_engagement() {
    let proc = ServeProc::start();
    let mut stream = proc.accept();

    let configured = call(
        &mut stream,
        &json!({ "id": 1, "tool": "_configure", "args": { "bash_mem_budget_mb": 128 } }),
    );
    assert_eq!(
        configured["ok"],
        json!(true),
        "configure failed: {configured}"
    );
    assert_eq!(configured["result"], json!({ "content": [] }));

    let breach = call(
        &mut stream,
        &json!({ "id": 2, "tool": "bash", "args": { "command": "echo starting; head -c 400M /dev/zero | tail" } }),
    );
    // An ordinary tool error — a string, not a kinded protocol object — so the
    // loop hands it to the model as an is_error result and carries on.
    assert_eq!(
        breach["ok"],
        json!(false),
        "the hog was not stopped: {breach}"
    );
    let message = breach["error"].as_str().expect("a tool error string");
    assert!(
        message.contains("exceeded the per-command memory budget")
            && message.contains("128 MB budget"),
        "unexpected breach text: {message}"
    );
    assert!(
        message.contains("starting"),
        "the partial output was dropped: {message}"
    );

    // The engagement is untouched: the next call answers normally and the
    // server still exits cleanly on EOF.
    let after = call(
        &mut stream,
        &json!({ "id": 3, "tool": "bash", "args": { "command": "echo alive" } }),
    );
    assert_eq!(
        after["ok"],
        json!(true),
        "the host did not survive: {after}"
    );

    drop(stream);
    let (status, stderr) = proc.expect_exit(Duration::from_secs(5));
    assert!(status.success(), "serve exited non-zero: {status:?}");
    assert!(!stderr.contains("panic"), "serve panicked: {stderr}");
}

/// Without the frame there is no budget: the same hog runs to completion, which
/// is what keeps the one-shot CLI and the differential parity corpus — neither
/// of which ever sends a configure — outside this feature entirely.
#[cfg(target_os = "linux")]
#[test]
fn an_unconfigured_session_lets_the_same_hog_finish() {
    let proc = ServeProc::start();
    let mut stream = proc.accept();
    let resp = call(
        &mut stream,
        &json!({ "id": 1, "tool": "bash", "args": { "command": "head -c 200M /dev/zero | tail | wc -c" } }),
    );
    assert_eq!(
        resp["ok"],
        json!(true),
        "an unconfigured session killed a command: {resp}"
    );
    assert!(
        resp["result"]["content"][0]["text"]
            .as_str()
            .unwrap_or_default()
            .contains("209715200"),
        "the hog did not run to completion: {resp}"
    );
}

#[test]
fn malformed_frames_never_crash_the_server() {
    let proc = ServeProc::start();
    let mut stream = proc.accept();

    // (1) A body that is not JSON at all.
    let resp = call(&mut stream, &json!(null)); // sends the JSON `null` — valid JSON, not an object
    assert_eq!(resp["ok"], json!(false));
    assert_eq!(resp["error"]["kind"], json!("malformed_request"));

    // (2) Raw non-JSON bytes as a framed body.
    write_frame(&mut stream, b"\xff\xfe not json \x00\x01").unwrap();
    let body = read_frame(&mut stream).unwrap();
    let resp: Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(resp["error"]["kind"], json!("malformed_request"));

    // (3) Valid JSON, but an array rather than a request object.
    write_frame(&mut stream, b"[1,2,3]").unwrap();
    let body = read_frame(&mut stream).unwrap();
    let resp: Value = serde_json::from_slice(&body).unwrap();
    assert_eq!(resp["error"]["kind"], json!("malformed_request"));

    // After all that hostile input the connection is still healthy.
    let ok = call(
        &mut stream,
        &json!({ "id": 1, "tool": "bash", "args": { "command": "echo ok" } }),
    );
    assert_eq!(ok["ok"], json!(true));

    drop(stream);
    let (status, stderr) = proc.expect_exit(Duration::from_secs(5));
    assert!(
        status.success(),
        "serve exited non-zero after hostile input: {status:?}"
    );
    assert!(
        !stderr.contains("panic"),
        "serve panicked on hostile bytes: {stderr}"
    );
}

#[test]
fn oversized_length_prefix_is_refused_and_closes() {
    let proc = ServeProc::start();
    let mut stream = proc.accept();

    // A 4-byte header claiming a ~4 GiB body, and no body — the classic hostile
    // prefix. The server must refuse it without allocating, answer a protocol
    // error, then close (it can't resynchronize past a body it won't read).
    let bogus_len: u32 = u32::MAX;
    assert!(bogus_len as usize > tf_harness_tools::serve::MAX_FRAME_BYTES);
    stream.write_all(&bogus_len.to_be_bytes()).unwrap();
    stream.flush().unwrap();

    let body = read_frame(&mut stream).expect("expected a protocol-error frame");
    let resp: Value = serde_json::from_slice(&body).unwrap();
    // Its own kind, distinct from the survivable malformed_request: this is the
    // one protocol error after which the server always closes.
    assert_eq!(resp["error"]["kind"], json!("request_too_large"));

    // The server closed after answering: the next read hits EOF.
    let mut header = [0u8; 4];
    let after = stream.read(&mut header);
    assert!(
        matches!(after, Ok(0)) || after.is_err(),
        "expected the server to close after an oversized prefix, got {after:?}"
    );

    let (status, stderr) = proc.expect_exit(Duration::from_secs(5));
    assert!(status.success());
    assert!(!stderr.contains("panic"), "serve panicked: {stderr}");
}

#[test]
fn client_death_mid_request_exits_the_server_cleanly() {
    let proc = ServeProc::start();
    let mut stream = proc.accept();

    // Fire a request that takes a beat, then vanish without reading the reply.
    // The server finishes the tool, its response write fails (or buffers), it
    // reads EOF, and it exits cleanly — no panic, no non-zero status.
    write_frame(
        &mut stream,
        json!({ "id": 1, "tool": "bash", "args": { "command": "sleep 0.3; echo done" } })
            .to_string()
            .as_bytes(),
    )
    .unwrap();
    std::thread::sleep(Duration::from_millis(50));
    drop(stream); // client death mid-request

    let (status, stderr) = proc.expect_exit(Duration::from_secs(5));
    assert!(
        status.success(),
        "serve did not exit cleanly on client death: {status:?}"
    );
    assert!(
        !stderr.contains("panic"),
        "serve panicked on client death: {stderr}"
    );
}

#[test]
fn server_death_mid_request_fails_the_client_read() {
    let mut proc = ServeProc::start();
    let mut stream = proc.accept();

    // In-flight request, then hard-kill the server. The response never arrives;
    // the client's frame read fails on EOF (the loser contract).
    write_frame(
        &mut stream,
        json!({ "id": 1, "tool": "bash", "args": { "command": "sleep 1; echo late" } })
            .to_string()
            .as_bytes(),
    )
    .unwrap();
    std::thread::sleep(Duration::from_millis(50));
    proc.child.kill().unwrap();
    proc.child.wait().unwrap();

    let read = read_frame(&mut stream);
    assert!(
        read.is_err(),
        "expected the client read to fail on server death"
    );
}
