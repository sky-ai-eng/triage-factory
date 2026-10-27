//! `bash` beyond pi: a session's time limits, and a process the command
//! leaves running in the background with the command's output still open.
//!
//! Linux-only: the background cases read process state through `kill(pid, 0)`
//! and clean up by process group.

#![cfg(target_os = "linux")]

use std::path::Path;
use std::time::{Duration, Instant};

use tf_harness_tools::bash::{execute, BashArgs, BashOptions};

const BACKGROUND_NOTE: &str = "A process this command started in the background is still attached to its \
output. It keeps running, but what it prints from now on is discarded; to keep that, redirect its output \
to a file (`cmd > /tmp/cmd.log 2>&1 &`).";

fn args(value: serde_json::Value) -> BashArgs {
    serde_json::from_value(value).unwrap()
}

fn limits(default_s: u64, max_s: u64) -> BashOptions {
    BashOptions {
        default_timeout_s: default_s,
        max_timeout_s: max_s,
        ..Default::default()
    }
}

/// Kills the process group whose id the command wrote to `pgid` in `dir`, so a
/// background process a test started does not outlive it.
struct GroupKiller<'a>(&'a Path);

impl Drop for GroupKiller<'_> {
    fn drop(&mut self) {
        if let Ok(pgid) = std::fs::read_to_string(self.0.join("pgid")) {
            if let Ok(pgid) = pgid.trim().parse::<i32>() {
                unsafe {
                    libc::kill(-pgid, libc::SIGKILL);
                }
            }
        }
    }
}

fn group_alive(dir: &Path) -> bool {
    let pgid: i32 = std::fs::read_to_string(dir.join("pgid"))
        .unwrap()
        .trim()
        .parse()
        .unwrap();
    unsafe { libc::kill(-pgid, 0) == 0 }
}

#[test]
fn the_default_timeout_ends_a_command_given_none_and_says_how_to_run_longer() {
    let dir = tempfile::TempDir::new().unwrap();
    let started = Instant::now();
    let err = execute(
        dir.path().to_str().unwrap(),
        &args(serde_json::json!({ "command": "echo start; sleep 30" })),
        &limits(1, 3),
    )
    .unwrap_err();
    assert!(
        started.elapsed() < Duration::from_secs(10),
        "took {:?}",
        started.elapsed()
    );
    assert_eq!(
        err.0,
        "start\n\n\nCommand timed out after 1 second, the limit for a command run without a timeout. \
For a command that needs longer, pass a timeout of up to 3 seconds. For anything longer than that, or \
anything that does not exit on its own such as a server or a watcher, start it in the background with its \
output in a file (`cmd > /tmp/cmd.log 2>&1 &`) and check on it with later commands."
    );
}

/// A process that keeps writing to the command's output neither holds the call
/// open nor dies of SIGPIPE once the call returns: it writes every 50 ms, so
/// closed pipes would kill it well inside the 500 ms it is checked after. It
/// sleeps before its first write so that `launched` is reliably the first line.
#[test]
fn a_background_process_that_logs_steadily_does_not_hold_the_call() {
    let dir = tempfile::TempDir::new().unwrap();
    let _cleanup = GroupKiller(dir.path());
    let started = Instant::now();
    let out = execute(
        dir.path().to_str().unwrap(),
        &args(serde_json::json!({
            "command": "echo $$ > pgid; (while :; do sleep 0.05; echo tick; done) & echo launched"
        })),
        &BashOptions::default(),
    )
    .unwrap();
    assert!(
        started.elapsed() < Duration::from_secs(5),
        "the call waited {:?} on a process that never stops writing",
        started.elapsed()
    );
    let text = out.text_output();
    assert!(text.starts_with("launched\n"), "got: {text}");
    assert!(text.ends_with(BACKGROUND_NOTE), "got: {text}");
    std::thread::sleep(Duration::from_millis(500));
    assert!(
        group_alive(dir.path()),
        "the background process died once the call returned"
    );
}

/// The serving tool host ignores SIGXFSZ so that its own write past the jail's
/// file-size limit fails instead of ending it. A command it runs gets the
/// default back, so a process writing past the limit is ended by the signal
/// (exit status 128 + 25) rather than inheriting the tool host's disposition
/// and seeing only EFBIG.
#[test]
fn a_command_writing_past_the_file_size_limit_dies_of_sigxfsz_while_the_tool_host_ignores_it() {
    tf_harness_tools::serve::ignore_file_size_signal();
    let dir = tempfile::TempDir::new().unwrap();
    let out = execute(
        dir.path().to_str().unwrap(),
        &args(serde_json::json!({
            "command": "ulimit -f 1; head -c 8192 /dev/zero > out; echo \"status $?\""
        })),
        &BashOptions::default(),
    )
    .unwrap();
    let text = out.text_output();
    assert!(text.contains("status 153"), "got: {text}");
}
