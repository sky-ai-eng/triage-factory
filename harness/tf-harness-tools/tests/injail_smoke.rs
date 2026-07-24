//! Env-gated in-jail smoke: run `tf-harness-tools serve` as the main process of
//! a real gVisor (runsc) container, with the tool-host socket directory
//! bind-mounted host↔jail, then drive a couple of tool calls from the host over
//! that socket.
//!
//! This exercises the one thing the out-of-jail differential and the soak tests
//! can't: that `serve`'s syscall surface (unix `bind`/`listen`/`accept`, the
//! reader `poll` loop, `fork`/`exec` for bash) runs under gVisor's Sentry, and
//! that a socket a sandboxed process binds inside a gofer-backed bind mount is
//! reachable by a host-side `connect` — the reverse of the agenthost direction,
//! and the reason this must be validated on a real runsc host (a docker/dev sim
//! does not reproduce the bake/mount environment). See the epic's standing note.
//!
//! It is inert by default and skips cleanly wherever the prerequisites are
//! absent, so `cargo test` stays green on any host. To actually run it:
//!
//! ```sh
//! # On the runsc host, as root:
//! TF_HARNESS_INJAIL_SMOKE=1 \
//! TF_HARNESS_INJAIL_ROOTFS=/path/to/abi-compatible/rootfs \
//!   cargo test --test injail_smoke -- --nocapture
//! ```
//!
//! `TF_HARNESS_INJAIL_ROOTFS` must point at a root filesystem whose C library
//! ABI matches the built binary (the binary is bind-mounted in; a musl-only
//! rootfs won't load a glibc build) and that has a `/bin/sh` for the bash tool.

use std::io::{Read, Write};
use std::os::unix::net::UnixStream;
use std::path::Path;
use std::process::Command;
use std::time::{Duration, Instant};

use serde_json::{json, Value};
use tempfile::TempDir;

const BIN: &str = env!("CARGO_BIN_EXE_tf-harness-tools");

/// Print why the smoke is skipping and return `None`, so the single `#[test]`
/// can bail cleanly. Every prerequisite that only exists on the runsc host is
/// checked here.
fn preflight() -> Option<()> {
    if std::env::var_os("TF_HARNESS_INJAIL_SMOKE").is_none() {
        eprintln!("skip: set TF_HARNESS_INJAIL_SMOKE=1 to run the in-jail smoke");
        return None;
    }
    if which("runsc").is_none() {
        eprintln!("skip: runsc not on PATH (validate on the real runsc host)");
        return None;
    }
    // runsc container setup (netns join, mounts, the OCI process) needs root on
    // every host we deploy to; a rootless attempt would fail for unrelated
    // reasons and muddy the signal.
    if unsafe { libc::geteuid() } != 0 {
        eprintln!("skip: in-jail smoke needs root for runsc run");
        return None;
    }
    if std::env::var_os("TF_HARNESS_INJAIL_ROOTFS").is_none() {
        eprintln!("skip: set TF_HARNESS_INJAIL_ROOTFS to an ABI-compatible rootfs dir");
        return None;
    }
    Some(())
}

fn which(bin: &str) -> Option<String> {
    let path = std::env::var("PATH").ok()?;
    for dir in path.split(':') {
        let candidate = Path::new(dir).join(bin);
        if candidate.is_file() {
            return Some(candidate.to_string_lossy().into_owned());
        }
    }
    None
}

fn read_frame(stream: &mut UnixStream) -> std::io::Result<Vec<u8>> {
    let mut header = [0u8; 4];
    stream.read_exact(&mut header)?;
    let len = u32::from_be_bytes(header) as usize;
    let mut body = vec![0u8; len];
    stream.read_exact(&mut body)?;
    Ok(body)
}

fn call(stream: &mut UnixStream, req: &Value) -> Value {
    let body = req.to_string();
    let mut frame = Vec::with_capacity(4 + body.len());
    frame.extend_from_slice(&(body.len() as u32).to_be_bytes());
    frame.extend_from_slice(body.as_bytes());
    stream.write_all(&frame).expect("write request");
    stream.flush().expect("flush");
    let resp = read_frame(stream).expect("read response");
    serde_json::from_slice(&resp).expect("response JSON")
}

#[test]
fn serve_runs_in_a_runsc_jail() {
    if preflight().is_none() {
        return;
    }
    let rootfs = std::env::var("TF_HARNESS_INJAIL_ROOTFS").unwrap();

    let bundle = TempDir::new().unwrap();
    let sockdir = bundle.path().join("sock");
    let workdir = bundle.path().join("work");
    std::fs::create_dir_all(&sockdir).unwrap();
    std::fs::create_dir_all(&workdir).unwrap();
    // The in-jail server (uid 0 in the default spec) creates the socket file
    // here; the host connects to it. World-writable so a uid-mapped jail can
    // still bind.
    set_mode(&sockdir, 0o777);
    std::fs::write(workdir.join("hello.txt"), "in-jail hello\n").unwrap();

    // Bootstrap a valid OCI config.json for the installed runsc version, then
    // patch only what the smoke needs. Deriving from `runsc spec` keeps the
    // spec's many required fields correct across runsc releases.
    let status = Command::new("runsc")
        .arg("spec")
        .current_dir(bundle.path())
        .status()
        .expect("runsc spec");
    assert!(status.success(), "runsc spec failed");
    let config_path = bundle.path().join("config.json");
    let mut spec: Value = serde_json::from_slice(&std::fs::read(&config_path).unwrap()).unwrap();

    spec["root"] = json!({ "path": rootfs, "readonly": true });
    spec["process"]["terminal"] = json!(false);
    spec["process"]["cwd"] = json!("/");
    spec["process"]["args"] = json!([
        "/tf/tf-harness-tools",
        "serve",
        "--socket",
        "/tf/sock/toolhost.sock",
        "--cwd",
        "/tf/work",
    ]);
    let mounts = spec["mounts"].as_array_mut().expect("mounts array");
    mounts.push(json!({
        "destination": "/tf/tf-harness-tools",
        "source": BIN,
        "type": "bind",
        "options": ["bind", "ro"],
    }));
    mounts.push(json!({
        "destination": "/tf/sock",
        "source": sockdir.to_str().unwrap(),
        "type": "bind",
        "options": ["bind", "rw"],
    }));
    mounts.push(json!({
        "destination": "/tf/work",
        "source": workdir.to_str().unwrap(),
        "type": "bind",
        "options": ["bind", "rw"],
    }));
    std::fs::write(&config_path, serde_json::to_vec_pretty(&spec).unwrap()).unwrap();

    // `runsc run` is create+start+wait in the foreground; serve keeps the
    // container alive until we close the socket, so run it detached and drive
    // it from here.
    let container_id = format!("tf-harness-smoke-{}", std::process::id());
    let mut runsc = Command::new("runsc")
        .arg("--network=none")
        .arg("run")
        .arg("--bundle")
        .arg(bundle.path())
        .arg(&container_id)
        .spawn()
        .expect("runsc run");

    let sock = sockdir.join("toolhost.sock");
    let result = std::panic::catch_unwind(|| {
        let mut stream = connect_with_retry(&sock);

        let read = call(
            &mut stream,
            &json!({ "id": 1, "tool": "read", "args": { "path": "hello.txt" } }),
        );
        assert_eq!(read["ok"], json!(true), "read failed in jail: {read}");
        assert!(
            read["result"]["content"][0]["text"]
                .as_str()
                .unwrap_or_default()
                .contains("in-jail hello"),
            "unexpected read content: {read}"
        );

        let bash = call(
            &mut stream,
            &json!({ "id": 2, "tool": "bash", "args": { "command": "echo jailed" } }),
        );
        assert_eq!(bash["ok"], json!(true), "bash failed in jail: {bash}");
    });

    // Teardown regardless of assertions: closing our end lets serve exit, but
    // force the container down and reap so nothing lingers.
    let _ = Command::new("runsc")
        .arg("kill")
        .arg(&container_id)
        .arg("KILL")
        .status();
    let _ = Command::new("runsc")
        .arg("delete")
        .arg("--force")
        .arg(&container_id)
        .status();
    let _ = runsc.wait();

    if let Err(e) = result {
        std::panic::resume_unwind(e);
    }
}

fn connect_with_retry(sock: &Path) -> UnixStream {
    let deadline = Instant::now() + Duration::from_secs(15);
    loop {
        match UnixStream::connect(sock) {
            Ok(stream) => {
                stream
                    .set_read_timeout(Some(Duration::from_secs(20)))
                    .unwrap();
                return stream;
            }
            Err(_) if Instant::now() < deadline => {
                std::thread::sleep(Duration::from_millis(50));
            }
            Err(e) => panic!("in-jail socket never became connectable: {e}"),
        }
    }
}

fn set_mode(path: &Path, mode: u32) {
    use std::os::unix::fs::PermissionsExt;
    let perms = std::fs::Permissions::from_mode(mode);
    std::fs::set_permissions(path, perms).unwrap();
}
