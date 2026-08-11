//! The `bash` per-command memory budget: the tree-RSS sampler, the breach
//! path's model-facing contract, and the promise that an unbudgeted command is
//! untouched by any of it.
//!
//! Linux-only, and not merely by convention: the sampler reads `/proc`, and the
//! memory hog these tests use (`head -c <N>M /dev/zero | tail` — `tail` must
//! buffer an unbounded input that contains no newline) relies on GNU coreutils'
//! size suffixes. No external dependency, no allocator fixture to build.

#![cfg(target_os = "linux")]

use std::os::unix::process::CommandExt;
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use tf_harness_tools::bash::{execute, tree_resident_bytes, BashArgs, BashOptions};
use tf_harness_tools::result::{BashToolDetails, ToolError, ToolResult};
use tf_harness_tools::shell::kill_process_tree;

const MB: u64 = 1024 * 1024;

fn args(command: &str) -> BashArgs {
    serde_json::from_value(serde_json::json!({ "command": command })).unwrap()
}

fn args_with_timeout(command: &str, timeout_secs: f64) -> BashArgs {
    serde_json::from_value(serde_json::json!({ "command": command, "timeout": timeout_secs }))
        .unwrap()
}

fn budget(mb: u64) -> BashOptions {
    BashOptions {
        mem_budget_mb: mb,
        ..Default::default()
    }
}

/// A command that goes over any budget these tests set and *stays* over it
/// until something stops it. `head` feeds `tail` a stream with no newline in
/// it, so `tail` can never retire a line and buffers all 400 MB it reads.
///
/// The trailing `| sleep` is what makes that state durable rather than a spike,
/// and it is load-bearing twice over. `head` exits once it has written its
/// 400 MB; `tail` then sees EOF and writes its whole buffer onward, and since
/// nothing ever reads that last pipe, `tail` blocks the moment it fills and
/// sits there still holding every byte it buffered. How much it takes to fill
/// is not something this relies on — capacity moves with kernel, page size and
/// `F_SETPIPE_SZ`, and gVisor implements its own pipes — only that it is
/// bounded, and that 400 MB is far past any of them.
///
/// So the tree is over budget for as long as the watchdog could conceivably
/// take to look at it. Unpinned, it is over budget only while it is running,
/// and how long that is depends entirely on how fast the host moves 400 MB
/// through a pipe — on one quick enough to do it inside a single sampling
/// interval, the whole pipeline can come and go between two ticks and never be
/// killed at all.
///
/// Pinning also keeps the hog's own output off the command's stdout. That
/// output is a single multi-hundred-KB line of NULs, and the tail-truncation
/// that renders it drops whole earlier lines to fit its 50KB budget — so a
/// marker printed *before* the hog survives into what the model is shown only
/// because the hog never gets to print over it.
const HOG: &str = "head -c 400M /dev/zero | tail | sleep 30";

fn text_of(result: &Result<ToolResult<BashToolDetails>, ToolError>) -> String {
    match result {
        Ok(ok) => ok.text_output(),
        Err(err) => err.0.clone(),
    }
}

/// A whole result — disposition, content and details alike — as one comparable
/// string, with the spill file's random name replaced. That name is the only
/// legitimate difference between two runs of the same command, so normalizing
/// it is what lets everything else be compared for equality rather than
/// sampled.
fn canonical(result: &Result<ToolResult<BashToolDetails>, ToolError>) -> String {
    let doc = match result {
        Ok(ok) => serde_json::json!({ "ok": true, "result": serde_json::to_value(ok).unwrap() }),
        Err(err) => serde_json::json!({ "ok": false, "error": err.0 }),
    };
    let rendered = doc.to_string();
    let mut out = String::with_capacity(rendered.len());
    let mut rest = rendered.as_str();
    while let Some(at) = rest.find("/tmp/tf-bash-") {
        out.push_str(&rest[..at]);
        let tail = &rest[at..];
        match tail.find(".log") {
            Some(end) => {
                out.push_str("$SPILL");
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

fn tmp_cwd() -> tempfile::TempDir {
    tempfile::tempdir().unwrap()
}

// --------------------------------------------------------------- the sampler

/// The sampler has to see memory a *descendant* allocated, not just the shell
/// it spawned — the whole failure mode it exists for is a build tool squatting
/// several processes down.
#[test]
fn tree_resident_bytes_sees_a_descendants_allocation() {
    let mut command = Command::new("/bin/sh");
    command
        .arg("-c")
        .arg(HOG)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    unsafe {
        // Its own session, so the group kill below reaps the whole pipeline
        // instead of signalling the test runner's own group.
        command.pre_exec(|| {
            if libc::setsid() == -1 {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
    }
    let mut child = command.spawn().expect("spawn the hog");
    let pid = child.id() as i32;

    // A threshold no shell process could reach on its own, so passing means the
    // walk descended rather than that /bin/sh happens to be fat.
    let threshold = 64 * MB;
    let deadline = Instant::now() + Duration::from_secs(30);
    let mut peak = 0u64;
    while Instant::now() < deadline {
        peak = peak.max(tree_resident_bytes(pid));
        if peak > threshold {
            break;
        }
        std::thread::sleep(Duration::from_millis(20));
    }
    kill_process_tree(pid);
    let _ = child.wait();

    assert!(
        peak > threshold,
        "the tree sampler never saw the descendant's memory (peak {peak} bytes)"
    );
}

/// A pid nothing is running under reads as zero rather than failing the sample:
/// a process that exits between two ticks must not take the watchdog with it.
#[test]
fn tree_resident_bytes_of_a_dead_pid_is_zero() {
    let mut child = Command::new("/bin/sh")
        .arg("-c")
        .arg("exit 0")
        .stdout(Stdio::null())
        .spawn()
        .unwrap();
    let pid = child.id() as i32;
    let _ = child.wait();
    assert_eq!(tree_resident_bytes(pid), 0);
}

// ---------------------------------------------------------------- the breach

#[test]
fn a_breach_kills_the_command_and_names_both_numbers() {
    let cwd = tmp_cwd();
    let started = Instant::now();
    let result = execute(
        cwd.path().to_str().unwrap(),
        &args(&format!("echo starting; {HOG}")),
        &budget(128),
    );
    let err = result.expect_err("a breaching command must come back as a tool error");

    assert!(
        err.0
            .starts_with("Command killed: exceeded the per-command memory budget"),
        "unexpected breach text: {}",
        err.0
    );
    assert!(
        err.0.contains("128 MB budget"),
        "the breach text must name the budget: {}",
        err.0
    );
    // The observed figure is the last sample, so it is at least the budget and
    // rendered with the ~ that says it may under-read the true peak.
    let observed = err
        .0
        .split_once("(~")
        .and_then(|(_, rest)| rest.split_once(" MB resident"))
        .and_then(|(mb, _)| mb.parse::<u64>().ok())
        .unwrap_or_else(|| panic!("the breach text carries no observed figure: {}", err.0));
    assert!(
        observed >= 128,
        "observed {observed} MB should be at or past the 128 MB budget: {}",
        err.0
    );
    // The loser contract: what the command printed before dying survives.
    assert!(
        err.0.contains("starting"),
        "the partial output was dropped: {}",
        err.0
    );
    assert!(
        started.elapsed() < Duration::from_secs(60),
        "the watchdog did not stop the command promptly"
    );
}

/// Both tripwires armed on the same command: exactly one kill, and exactly one
/// reason in the text the model reads.
#[test]
fn a_command_over_both_limits_dies_once_for_one_reason() {
    let cwd = tmp_cwd();
    let err = execute(
        cwd.path().to_str().unwrap(),
        &args_with_timeout(HOG, 1.0),
        &budget(64),
    )
    .expect_err("over both limits is still an error");

    let budgeted = err.0.contains("exceeded the per-command memory budget");
    let timed_out = err.0.contains("Command timed out after");
    assert!(
        budgeted || timed_out,
        "neither limit was reported: {}",
        err.0
    );
    assert!(
        !(budgeted && timed_out),
        "one kill must report one reason: {}",
        err.0
    );
}

// ------------------------------------------------------- the unbreached path

/// The happy path is the AC that matters most: an under-budget command must be
/// byte-identical to the same command run with no watchdog at all.
#[test]
fn an_under_budget_command_is_byte_identical_to_an_unwatched_one() {
    let cwd = tmp_cwd();
    let path = cwd.path().to_str().unwrap();
    for command in [
        "echo hello",
        "printf 'no trailing newline'",
        // Truncation and its spill notice: the widest result shape bash has.
        "seq 1 5000",
        "echo to-stderr >&2; exit 3",
        "true",
    ] {
        let unwatched = execute(path, &args(command), &BashOptions::default());
        let watched = execute(path, &args(command), &budget(4096));
        assert_eq!(
            canonical(&unwatched),
            canonical(&watched),
            "watching changed the result of `{command}`"
        );
    }
}

/// Living in the hot band is not a breach. Sampling tightens once a tree comes
/// near its budget, and the risk that introduces is a false kill: a command
/// that legitimately sits just under its limit must still run to completion.
///
/// The numbers are chosen to be stable rather than close: a 100 MB stream held
/// by `tail` peaks at a repeatable ~106 MB, which is above the 98 MB watermark
/// of a 140 MB budget and comfortably under the budget itself.
#[test]
fn a_command_inside_the_hot_band_is_sampled_faster_but_not_killed() {
    let cwd = tmp_cwd();
    let result = execute(
        cwd.path().to_str().unwrap(),
        &args("head -c 100M /dev/zero | tail | wc -c"),
        &budget(140),
    )
    .expect("a command under its budget must finish, however near the line it runs");
    assert!(
        text_of(&Ok(result)).contains("104857600"),
        "the command did not run to completion"
    );
}

/// A timeout with the watchdog armed still reports the timeout, unchanged: the
/// budget folds into that supervisor rather than replacing it.
#[test]
fn a_timeout_under_budget_still_reports_the_timeout() {
    let cwd = tmp_cwd();
    let err = execute(
        cwd.path().to_str().unwrap(),
        &args_with_timeout("echo before; sleep 30", 0.5),
        &budget(2048),
    )
    .expect_err("a timeout is an error");
    assert!(
        err.0.contains("Command timed out after 0.5 seconds"),
        "unexpected timeout text: {}",
        err.0
    );
    assert!(
        err.0.starts_with("before"),
        "the timeout path still leads with the command's output: {}",
        err.0
    );
}

/// Zero is off. A command that would breach any real budget runs to completion
/// when none was configured, which is what keeps the one-shot CLI and the
/// parity corpus outside this feature entirely.
#[test]
fn no_budget_means_no_watchdog() {
    let cwd = tmp_cwd();
    let result = execute(
        cwd.path().to_str().unwrap(),
        &args("head -c 200M /dev/zero | tail | wc -c"),
        &BashOptions::default(),
    )
    .expect("an unbudgeted command is never killed for its memory");
    assert!(
        text_of(&Ok(result)).contains("209715200"),
        "the unbudgeted hog did not run to completion"
    );
}
