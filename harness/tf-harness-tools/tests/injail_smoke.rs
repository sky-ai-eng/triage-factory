//! Env-gated in-jail smoke: run `tf-harness-tools serve` as the main process of
//! a real gVisor (runsc) container and drive tool calls into it from the host.
//!
//! This exercises the things the out-of-jail differential and the soak tests
//! can't: that `serve`'s syscall surface (the host-socket `connect`, the reader
//! `poll` loop, `fork`/`exec` for bash) runs under gVisor's Sentry, that a
//! host-bound socket reached through a gofer bind mount is connectable from
//! inside the jail with `--host-uds=open`, and that the memory watchdog's
//! `/proc` reads are served by the Sentry's own procfs rather than silently
//! reading zero. That last one has no substitute: a watchdog whose sampler
//! reads nothing under gVisor would pass every host-side test and never fire
//! where it matters. The bake/mount environment isn't reproducible in a docker
//! sim, so this must run on a real runsc host. See the epic's standing note.
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
//! rootfs won't load a glibc build) and that has a `/bin/sh` plus coreutils for
//! the bash tool.

use std::io::{Read, Write};
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::Path;
use std::process::{Child, Command};
use std::time::{Duration, Instant};

use serde_json::{json, Value};
use tempfile::TempDir;

const BIN: &str = env!("CARGO_BIN_EXE_tf-harness-tools");

/// Print why the smoke is skipping and return `None`, so each `#[test]` can
/// bail cleanly. Every prerequisite that only exists on the runsc host is
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

/// A `serve` process running as a runsc container's main process, with the
/// accepted connection to it.
///
/// The host binds and the jail dials — the production direction, and the only
/// one available: `--host-uds=open` permits a sandboxed `connect` but not a
/// `bind`, which is exactly why the deployed launch inverts the usual roles.
struct Jail {
    container_id: String,
    runsc: Child,
    stream: UnixStream,
    _bundle: TempDir,
}

impl Jail {
    fn start(seed: &[(&str, &str)]) -> Jail {
        let rootfs = std::env::var("TF_HARNESS_INJAIL_ROOTFS").unwrap();

        let bundle = TempDir::new().unwrap();
        let sockdir = bundle.path().join("sock");
        let workdir = bundle.path().join("work");
        std::fs::create_dir_all(&sockdir).unwrap();
        std::fs::create_dir_all(&workdir).unwrap();
        for (name, content) in seed {
            std::fs::write(workdir.join(name), content).unwrap();
        }
        // The jail runs as another uid and connect(2) needs write permission on
        // the socket; the deployed launch chowns to the sandbox identity, which
        // this smoke has no equivalent of.
        set_mode(&sockdir, 0o777);

        let sock = sockdir.join("toolhost.sock");
        let listener = UnixListener::bind(&sock).expect("bind the tool-host socket");
        set_mode(&sock, 0o666);

        // Bootstrap a valid OCI config.json for the installed runsc version,
        // then patch only what the smoke needs. Deriving from `runsc spec`
        // keeps the spec's many required fields correct across runsc releases.
        let status = Command::new("runsc")
            .arg("spec")
            .current_dir(bundle.path())
            .status()
            .expect("runsc spec");
        assert!(status.success(), "runsc spec failed");
        let config_path = bundle.path().join("config.json");
        let mut spec: Value =
            serde_json::from_slice(&std::fs::read(&config_path).unwrap()).unwrap();

        spec["root"] = json!({ "path": rootfs, "readonly": true });
        spec["process"]["terminal"] = json!(false);
        spec["process"]["cwd"] = json!("/");
        spec["process"]["args"] = json!([
            "/tf/tf-harness-tools",
            "serve",
            "--connect",
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
        // container alive until we close the socket, so run it detached and
        // drive it from here. --host-uds=open is what makes the connect below
        // possible at all, and is the deployed launch's flag.
        let container_id = format!(
            "tf-harness-smoke-{}-{}",
            std::process::id(),
            next_jail_seq()
        );
        let runsc = Command::new("runsc")
            .arg("--host-uds=open")
            .arg("--network=none")
            .arg("run")
            .arg("--bundle")
            .arg(bundle.path())
            .arg(&container_id)
            .spawn()
            .expect("runsc run");

        let stream = accept_with_deadline(&listener);
        Jail {
            container_id,
            runsc,
            stream,
            _bundle: bundle,
        }
    }

    fn call(&mut self, req: &Value) -> Value {
        call(&mut self.stream, req)
    }
}

impl Drop for Jail {
    fn drop(&mut self) {
        let _ = Command::new("runsc")
            .arg("kill")
            .arg(&self.container_id)
            .arg("KILL")
            .status();
        let _ = Command::new("runsc")
            .arg("delete")
            .arg("--force")
            .arg(&self.container_id)
            .status();
        let _ = self.runsc.kill();
        let _ = self.runsc.wait();
    }
}

/// A per-jail number, so the container ids of tests running concurrently in
/// one process never collide in runsc's state directory.
fn next_jail_seq() -> u32 {
    use std::sync::atomic::{AtomicU32, Ordering};
    static NEXT: AtomicU32 = AtomicU32::new(0);
    NEXT.fetch_add(1, Ordering::Relaxed)
}

fn accept_with_deadline(listener: &UnixListener) -> UnixStream {
    listener.set_nonblocking(true).unwrap();
    let deadline = Instant::now() + Duration::from_secs(30);
    loop {
        match listener.accept() {
            Ok((stream, _)) => {
                listener.set_nonblocking(false).unwrap();
                stream.set_nonblocking(false).unwrap();
                stream
                    .set_read_timeout(Some(Duration::from_secs(120)))
                    .unwrap();
                return stream;
            }
            Err(ref e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                if Instant::now() >= deadline {
                    panic!("the in-jail tool host never dialed in");
                }
                std::thread::sleep(Duration::from_millis(50));
            }
            Err(e) => panic!("accepting the in-jail connection failed: {e}"),
        }
    }
}

fn set_mode(path: &Path, mode: u32) {
    use std::os::unix::fs::PermissionsExt;
    let perms = std::fs::Permissions::from_mode(mode);
    std::fs::set_permissions(path, perms).unwrap();
}

fn text_of(resp: &Value) -> String {
    resp["result"]["content"][0]["text"]
        .as_str()
        .unwrap_or_default()
        .to_string()
}

#[test]
fn serve_runs_in_a_runsc_jail() {
    if preflight().is_none() {
        return;
    }
    let mut jail = Jail::start(&[("hello.txt", "in-jail hello\n")]);

    let read = jail.call(&json!({ "id": 1, "tool": "read", "args": { "path": "hello.txt" } }));
    assert_eq!(read["ok"], json!(true), "read failed in jail: {read}");
    assert!(
        text_of(&read).contains("in-jail hello"),
        "unexpected read content: {read}"
    );

    let bash = jail.call(&json!({ "id": 2, "tool": "bash", "args": { "command": "echo jailed" } }));
    assert_eq!(bash["ok"], json!(true), "bash failed in jail: {bash}");
}

/// The watchdog's one environmental assumption: gVisor's procfs serves
/// `statm` (and the `children` lists the tree walk descends through) for the
/// Sentry's own tasks. Asserted from inside the jail, because a sampler that
/// reads zero in here fires never and fails nothing.
#[test]
fn proc_statm_is_readable_under_gvisor() {
    if preflight().is_none() {
        return;
    }
    let mut jail = Jail::start(&[]);

    let statm = jail.call(&json!({
        "id": 1,
        "tool": "bash",
        "args": { "command": "cat /proc/$$/statm" },
    }));
    assert_eq!(
        statm["ok"],
        json!(true),
        "statm unreadable in jail: {statm}"
    );
    let fields: Vec<u64> = text_of(&statm)
        .split_whitespace()
        .filter_map(|f| f.parse().ok())
        .collect();
    assert!(
        fields.len() >= 2,
        "gVisor's statm does not have the fields the sampler reads: {statm}"
    );
    assert!(
        fields[1] > 0,
        "gVisor reported zero resident pages for a live process; the sampler would never fire: {statm}"
    );

    let children = jail.call(&json!({
        "id": 2,
        "tool": "bash",
        "args": { "command": "sleep 5 & cat /proc/$$/task/$$/children" },
    }));
    assert_eq!(
        children["ok"],
        json!(true),
        "the children list is unreadable in jail: {children}"
    );
    assert!(
        !text_of(&children).trim().is_empty(),
        "gVisor listed no children for a shell that has one; the tree walk would see only the shell: {children}"
    );
}

/// The whole feature, in the environment it ships in: configure a budget over
/// the socket, run a real allocator inside a real jail, and get the killed
/// command back as an ordinary tool error with the engagement intact.
#[test]
fn a_configured_budget_kills_a_real_allocator_in_the_jail() {
    if preflight().is_none() {
        return;
    }
    let mut jail = Jail::start(&[]);

    let configured = jail.call(&json!({
        "id": 1,
        "tool": "_configure",
        "args": { "bash_mem_budget_mb": 128 },
    }));
    assert_eq!(
        configured["ok"],
        json!(true),
        "configure failed in jail: {configured}"
    );

    let breach = jail.call(&json!({
        "id": 2,
        "tool": "bash",
        "args": { "command": "echo starting; head -c 400M /dev/zero | tail" },
    }));
    assert_eq!(
        breach["ok"],
        json!(false),
        "the in-jail hog was never stopped: {breach}"
    );
    let message = breach["error"].as_str().unwrap_or_default();
    assert!(
        message.contains("exceeded the per-command memory budget")
            && message.contains("128 MB budget"),
        "unexpected in-jail breach text: {breach}"
    );
    assert!(
        message.contains("starting"),
        "the partial output was dropped in jail: {breach}"
    );

    let after = jail.call(&json!({ "id": 3, "tool": "bash", "args": { "command": "echo alive" } }));
    assert_eq!(
        after["ok"],
        json!(true),
        "the jail's tool host did not survive the kill: {after}"
    );
}
