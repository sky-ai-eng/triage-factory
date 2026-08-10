//! Port of pi's `core/tools/bash.ts` execute path.
//!
//! The command runs in its own session (pi's `detached: true`), stdout and
//! stderr stream into the OutputAccumulator, a timeout SIGKILLs the process
//! group, and after exit the pipes get pi's 100ms idle grace so output from
//! detached descendants isn't cut mid-write (`waitForChildProcess`).
//!
//! # The per-command memory budget
//!
//! One divergence from pi rides the same supervisor: when the session was
//! configured with a budget, each poll also sums the command tree's resident
//! memory and kills the tree that exceeds it. `bash` is the only tool that
//! spawns children, so it is the only place a limit can be attributed to a
//! command at all.
//!
//! It is a *sampling* watchdog, and deliberately so. Between ticks a command
//! can overshoot by its allocation rate — the jail's own memory ceiling stays
//! underneath as the kernel-truth backstop, and a single giant allocation that
//! trips it first fails in-jail exactly as it did before. Both endings are an
//! agent-legible error; neither ends the engagement. The budget's job is blame
//! attribution and session headroom, not tight enforcement: without it, a
//! command that squats near the ceiling doesn't fail — every *later*,
//! innocent command does.
//!
//! Not rlimits, and not cgroups. `RLIMIT_AS`/`RLIMIT_DATA` bound virtual
//! address space, which JIT runtimes reserve far past their resident set, so
//! they produce false OOMs on healthy commands. Per-command cgroups don't
//! exist under gVisor: the jail is one host cgroup and the in-jail cgroupfs is
//! emulated and non-enforcing.

use std::os::unix::io::AsRawFd;
use std::os::unix::process::{CommandExt, ExitStatusExt};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{Receiver, RecvTimeoutError};
use std::sync::{Arc, Condvar, Mutex};
use std::time::{Duration, Instant};

use serde::Deserialize;

use crate::jsnum::js_num;
use crate::output_accumulator::{OutputAccumulator, OutputSnapshot};
use crate::result::{BashToolDetails, ContentBlock, ToolError, ToolResult, ToolResultOrError};
use crate::shell::{get_shell_config, kill_process_tree};
use crate::truncate::{format_size, DEFAULT_MAX_BYTES};

const MAX_TIMEOUT_MS: f64 = 2_147_483_647.0;
const POST_EXIT_GRACE: Duration = Duration::from_millis(100);
const READER_POLL_MS: i32 = 200;

/// How often the supervisor samples a command tree that is nowhere near its
/// budget. Only armed when a budget is set: with no budget the supervisor
/// waits exactly as it did before the watchdog existed, so an unconfigured
/// session gains no wakeups.
const MEM_SAMPLE_INTERVAL: Duration = Duration::from_millis(300);

/// How often it samples once the tree has come near its budget. The interval
/// bounds how far a breach can overshoot (overshoot ≈ allocation rate ×
/// interval), so it is spent where overshoot is actually reachable rather than
/// uniformly.
///
/// Measured rates make the trade concrete: a single-threaded allocator faulting
/// anonymous pages as fast as it can manages ~840 MB/s, and a pipe-buffering
/// hog ~380 MB/s, so 300ms of blindness is ~115–250 MB while 100ms is ~40–85.
/// Sampling that fast all the time would triple a cost the whole compiled
/// harness exists to minimize — guest syscall rate — for a window most commands
/// never enter. A command that is not near its budget cannot breach in one
/// tick unless it is allocating faster than the ceiling's own headroom
/// absorbs, and that case is what the jail ceiling is for.
const MEM_SAMPLE_INTERVAL_HOT: Duration = Duration::from_millis(100);

/// The share of the budget at which a sample counts as *hot* and tightens the
/// interval, as a fraction (7/10). Chosen so the fast window opens with a
/// budget's worth of slack still ahead: at 70% of the default 3072 MB budget,
/// ~920 MB remain, which is ~10 fast ticks even at the fastest rate measured.
const MEM_HOT_NUMERATOR: u64 = 7;
const MEM_HOT_DENOMINATOR: u64 = 10;

/// How many consecutive cold samples it takes to relax back to the slow
/// interval — the "last N" of the rule, and the reason it is a decay window
/// rather than a threshold read fresh each tick.
///
/// A tree at the watermark oscillates: a garbage collector, a linker freeing a
/// stage, a test process exiting all dip the sample below 70% for a tick while
/// the command is still very much near its limit. Relaxing on the first dip
/// would spend the fast interval exactly when it stops helping, so the tree
/// has to stay cold for a while to earn it back.
const MEM_HOT_DECAY_SAMPLES: usize = 5;

// The two relationships the policy is meaningless without: the tightened
// interval has to be tighter, and the watermark has to sit below the budget it
// warns about. Asserted at compile time rather than in a test, because a
// violation is not a behavior regression to catch — it is a contradiction that
// should never link.
const _: () = assert!(MEM_SAMPLE_INTERVAL_HOT.as_millis() < MEM_SAMPLE_INTERVAL.as_millis());
const _: () = assert!(MEM_HOT_NUMERATOR < MEM_HOT_DENOMINATOR);

/// Upper bound on processes visited in one memory sample. A tree cannot cycle,
/// so this only backstops a pathological fan-out from turning a sampling tick
/// into an unbounded `/proc` walk.
const MAX_SAMPLED_PROCS: usize = 4096;

const BYTES_PER_MB: u64 = 1024 * 1024;

#[derive(Debug, Clone, Deserialize)]
pub struct BashArgs {
    pub command: String,
    #[serde(default)]
    pub timeout: Option<f64>,
}

#[derive(Debug, Clone, Default)]
pub struct BashOptions {
    /// Prepended to every command with a newline, like pi's commandPrefix.
    pub command_prefix: Option<String>,
    /// Explicit shell path (pi's shellPath setting).
    pub shell_path: Option<String>,
    /// Directory to prepend to PATH (pi prepends its tool bin dir).
    pub path_prepend: Option<String>,
    /// Per-command resident-memory budget in MB, from the session's
    /// configuration rather than from the model. Zero — every caller but the
    /// configured resident host — disables the watchdog entirely: no sampling
    /// tick, no `/proc` walk, no behavior to observe.
    pub mem_budget_mb: u64,
}

fn resolve_timeout_ms(timeout: Option<f64>) -> Result<Option<f64>, ToolError> {
    let Some(timeout) = timeout else {
        return Ok(None);
    };
    if !timeout.is_finite() || timeout <= 0.0 {
        return Err(ToolError::new(
            "Invalid timeout: must be a finite number of seconds",
        ));
    }
    let timeout_ms = timeout * 1000.0;
    if timeout_ms > MAX_TIMEOUT_MS {
        return Err(ToolError::new(format!(
            "Invalid timeout: maximum is {} seconds",
            js_num(MAX_TIMEOUT_MS / 1000.0)
        )));
    }
    Ok(Some(timeout_ms))
}

/// Why the supervisor killed the command tree. Absent when the command exited
/// on its own.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum KillReason {
    /// The per-call wall-clock timeout elapsed.
    Timeout,
    /// The tree's sampled resident memory exceeded the session's budget.
    /// Carries the sample that tripped it, which is what the model is told.
    MemoryBudget { observed_bytes: u64 },
}

/// Whether a sample is close enough to the budget to sample faster.
///
/// Integer arithmetic on both sides rather than a float ratio, so a zero
/// budget (the disabled case, which never reaches here) cannot divide and a
/// huge sample cannot lose precision.
fn sample_is_hot(observed_bytes: u64, budget_bytes: u64) -> bool {
    observed_bytes.saturating_mul(MEM_HOT_DENOMINATOR)
        >= budget_bytes.saturating_mul(MEM_HOT_NUMERATOR)
}

/// The next sampling interval, given how many consecutive samples have been
/// cold. Fast while any of the last [`MEM_HOT_DECAY_SAMPLES`] samples was hot.
fn sample_interval(cold_run: usize) -> Duration {
    if cold_run < MEM_HOT_DECAY_SAMPLES {
        MEM_SAMPLE_INTERVAL_HOT
    } else {
        MEM_SAMPLE_INTERVAL
    }
}

/// Wait for the command, enforcing the deadline and the memory budget from one
/// poll loop.
///
/// One supervisor, one kill site: a command that is both over time and over
/// budget in the same tick dies exactly once and reports exactly one reason.
/// The deadline is checked first, because a command that has already outlived
/// its timeout is over-time regardless of what a sample taken in the same
/// instant says.
///
/// With no budget armed the wait is what it was before the watchdog existed —
/// a single receive bounded by the whole deadline, or an unbounded one — so an
/// unconfigured session's timing is unchanged rather than approximately
/// unchanged.
///
/// The sampling interval is adaptive: slow until a sample comes near the
/// budget, then fast until the tree has been clear of the watermark for
/// [`MEM_HOT_DECAY_SAMPLES`] samples running. Overshoot is proportional to the
/// interval, so the tight one is spent in the only window where a breach is
/// reachable, and a command that never approaches its budget — nearly all of
/// them — pays the slow rate throughout.
fn supervise(
    exit_rx: &Receiver<std::io::Result<std::process::ExitStatus>>,
    pid: i32,
    deadline: Option<Instant>,
    budget_bytes: u64,
) -> (
    std::io::Result<std::process::ExitStatus>,
    Option<KillReason>,
) {
    // After a kill the child is already doomed; block until the waiter reaps
    // it, exactly as the timeout path did.
    let reap = || {
        exit_rx
            .recv()
            .unwrap_or_else(|_| Ok(std::process::ExitStatus::from_raw(0)))
    };
    // Consecutive cold samples. Starts relaxed: a command is not near a budget
    // it has not begun to use.
    let mut cold_run = MEM_HOT_DECAY_SAMPLES;
    loop {
        let remaining = deadline.map(|d| d.saturating_duration_since(Instant::now()));
        let interval = sample_interval(cold_run);
        let tick = match (remaining, budget_bytes > 0) {
            (Some(left), false) => Some(left),
            (Some(left), true) => Some(left.min(interval)),
            (None, true) => Some(interval),
            (None, false) => None,
        };
        let waited = match tick {
            Some(dur) => exit_rx.recv_timeout(dur),
            None => exit_rx.recv().map_err(|_| RecvTimeoutError::Disconnected),
        };
        match waited {
            Ok(status) => return (status, None),
            Err(RecvTimeoutError::Disconnected) => {
                return (Ok(std::process::ExitStatus::from_raw(0)), None)
            }
            Err(RecvTimeoutError::Timeout) => {}
        }
        // Decide the reason, then kill in one place: a command over both
        // limits in the same tick must not be signalled twice, and the reason
        // the model reads must not depend on which check ran first.
        let reason = if deadline.is_some_and(|d| Instant::now() >= d) {
            Some(KillReason::Timeout)
        } else if budget_bytes > 0 {
            let observed_bytes = tree_resident_bytes(pid);
            cold_run = if sample_is_hot(observed_bytes, budget_bytes) {
                0
            } else {
                cold_run.saturating_add(1)
            };
            (observed_bytes > budget_bytes).then_some(KillReason::MemoryBudget { observed_bytes })
        } else {
            None
        };
        if let Some(reason) = reason {
            kill_process_tree(pid);
            return (reap(), Some(reason));
        }
    }
}

/// Sum resident memory, in bytes, across the process tree rooted at `pid`.
///
/// Children come from `/proc/<pid>/task/<tid>/children` and the resident set
/// from field 2 of `/proc/<pid>/statm` (resident pages) — cheaper to parse
/// than `status`, and served by gVisor's procfs for the Sentry's own tasks.
///
/// Shared pages count once per process that maps them, so a tree of forks
/// sharing one heap reads high. Accepted: the over-estimate kills earlier, and
/// the budget is policy rather than billing.
///
/// Every read is best-effort. A process that exits mid-walk contributes
/// nothing instead of failing the sample, which is the right direction too —
/// an unreadable `/proc` under-reports and the command lives. A host with no
/// procfs at all (a macOS dev machine) therefore samples zero and never fires,
/// which is the correct inert answer: the runtime that configures a budget
/// only ever runs on Linux.
pub fn tree_resident_bytes(pid: i32) -> u64 {
    let page_size = page_size();
    let mut total: u64 = 0;
    let mut pending = vec![pid];
    let mut visited = 0usize;
    while let Some(pid) = pending.pop() {
        visited += 1;
        if visited > MAX_SAMPLED_PROCS {
            break;
        }
        total = total.saturating_add(resident_pages(pid).saturating_mul(page_size));
        collect_children(pid, &mut pending);
    }
    total
}

fn page_size() -> u64 {
    let size = unsafe { libc::sysconf(libc::_SC_PAGESIZE) };
    if size > 0 {
        size as u64
    } else {
        4096
    }
}

/// Field 2 of `/proc/<pid>/statm`: resident pages. Zero for a process that is
/// gone, or whose procfs entry cannot be read or parsed.
fn resident_pages(pid: i32) -> u64 {
    let Ok(statm) = std::fs::read_to_string(format!("/proc/{pid}/statm")) else {
        return 0;
    };
    statm
        .split_whitespace()
        .nth(1)
        .and_then(|field| field.parse().ok())
        .unwrap_or(0)
}

/// Append `pid`'s direct children to `out`, from every thread's `children`
/// file — a child is listed under the thread that forked it, not under the
/// thread group.
fn collect_children(pid: i32, out: &mut Vec<i32>) {
    let Ok(tasks) = std::fs::read_dir(format!("/proc/{pid}/task")) else {
        return;
    };
    for task in tasks.flatten() {
        let path = task.path().join("children");
        let Ok(listed) = std::fs::read_to_string(&path) else {
            continue;
        };
        out.extend(
            listed
                .split_whitespace()
                .filter_map(|p| p.parse::<i32>().ok()),
        );
    }
}

struct ReaderShared {
    accepting: AtomicBool,
    stop: AtomicBool,
    accumulator: Mutex<OutputAccumulator>,
    state: Mutex<ReaderState>,
    condvar: Condvar,
}

struct ReaderState {
    readers_done: usize,
    last_data_at: Instant,
}

fn reader_thread(
    fd_owner: impl AsRawFd + Send + 'static,
    shared: Arc<ReaderShared>,
) -> std::thread::JoinHandle<()> {
    std::thread::spawn(move || {
        let fd = fd_owner.as_raw_fd();
        let mut buf = vec![0u8; 65536];
        loop {
            if shared.stop.load(Ordering::Relaxed) {
                break;
            }
            let mut pfd = libc::pollfd {
                fd,
                events: libc::POLLIN,
                revents: 0,
            };
            let rc = unsafe { libc::poll(&mut pfd, 1, READER_POLL_MS) };
            if rc < 0 {
                let err = std::io::Error::last_os_error();
                if err.raw_os_error() == Some(libc::EINTR) {
                    continue;
                }
                break;
            }
            if rc == 0 {
                continue;
            }
            let n = unsafe { libc::read(fd, buf.as_mut_ptr().cast(), buf.len()) };
            if n < 0 {
                let err = std::io::Error::last_os_error();
                if err.raw_os_error() == Some(libc::EINTR) {
                    continue;
                }
                break;
            }
            if n == 0 {
                break;
            }
            let data = &buf[..n as usize];
            if shared.accepting.load(Ordering::Relaxed) {
                let mut acc = shared.accumulator.lock().unwrap();
                acc.append(data);
            }
            let mut state = shared.state.lock().unwrap();
            state.last_data_at = Instant::now();
            shared.condvar.notify_all();
        }
        let mut state = shared.state.lock().unwrap();
        state.readers_done += 1;
        shared.condvar.notify_all();
        // fd_owner drops here, closing the pipe.
        drop(fd_owner);
    })
}

/// What the model is told when the budget kills its command.
///
/// The `~` is load-bearing: `observed` is the last sample, so a command that
/// allocated fast between ticks really did peak higher than the number it is
/// shown. Both numbers are stated because the actionable part is the gap
/// between them — the remedies that follow only make sense against a limit the
/// model can size its next attempt to.
fn memory_budget_notice(observed_bytes: u64, budget_mb: u64) -> String {
    format!(
        "Command killed: exceeded the per-command memory budget (~{} MB resident against a {} MB budget). \
The sandbox is unaffected. Retry with a lighter approach — lower build parallelism, stream instead of \
loading whole files, or split the work into smaller steps.",
        observed_bytes / BYTES_PER_MB,
        budget_mb
    )
}

fn prepend_status(status: &str, text: &str) -> String {
    if text.is_empty() {
        status.to_string()
    } else {
        format!("{status}\n\n{text}")
    }
}

pub fn execute(
    cwd: &str,
    args: &BashArgs,
    options: &BashOptions,
) -> ToolResultOrError<BashToolDetails> {
    let timeout_ms = resolve_timeout_ms(args.timeout)?;

    let resolved_command = match &options.command_prefix {
        Some(prefix) => format!("{prefix}\n{}", args.command),
        None => args.command.clone(),
    };

    let shell_config = get_shell_config(options.shell_path.as_deref())?;

    if !std::path::Path::new(cwd).exists() {
        return Err(ToolError::new(format!(
            "Working directory does not exist: {cwd}\nCannot execute bash commands."
        )));
    }

    let shared = Arc::new(ReaderShared {
        accepting: AtomicBool::new(true),
        stop: AtomicBool::new(false),
        // Agent-facing path (the model reads it back via the truncation
        // notice), so it carries TF branding — a deliberate divergence from
        // pi's "pi-bash" prefix.
        accumulator: Mutex::new(OutputAccumulator::new("tf-bash")),
        state: Mutex::new(ReaderState {
            readers_done: 0,
            last_data_at: Instant::now(),
        }),
        condvar: Condvar::new(),
    });

    let mut command = std::process::Command::new(&shell_config.shell);
    for arg in &shell_config.args {
        command.arg(arg);
    }
    command.arg(&resolved_command);
    command.current_dir(cwd);
    command.stdin(std::process::Stdio::null());
    command.stdout(std::process::Stdio::piped());
    command.stderr(std::process::Stdio::piped());
    if let Some(prepend) = &options.path_prepend {
        let current = std::env::var("PATH").unwrap_or_default();
        let entries: Vec<&str> = current.split(':').filter(|s| !s.is_empty()).collect();
        if !entries.contains(&prepend.as_str()) {
            let joined: Vec<&str> = std::iter::once(prepend.as_str()).chain(entries).collect();
            command.env("PATH", joined.join(":"));
        }
    }
    unsafe {
        command.pre_exec(|| {
            // pi spawns detached; a fresh session makes -pid kill the tree.
            if libc::setsid() == -1 {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
    }

    let spawn_result = command.spawn();
    let mut child = match spawn_result {
        Ok(child) => child,
        Err(err) => {
            // Node reports spawn failures as `spawn <path> <CODE>`.
            let code = crate::errors::node_fs_error(&err, "spawn", &shell_config.shell).code;
            return Err(ToolError::new(format!(
                "spawn {} {}",
                shell_config.shell, code
            )));
        }
    };
    let pid = child.id() as i32;

    let stdout = child.stdout.take().expect("stdout piped");
    let stderr = child.stderr.take().expect("stderr piped");
    let reader_a = reader_thread(stdout, Arc::clone(&shared));
    let reader_b = reader_thread(stderr, Arc::clone(&shared));

    let (exit_tx, exit_rx) =
        std::sync::mpsc::channel::<std::io::Result<std::process::ExitStatus>>();
    let wait_handle = std::thread::spawn(move || {
        let _ = exit_tx.send(child.wait());
    });

    let deadline = timeout_ms.map(|ms| Instant::now() + Duration::from_millis(ms as u64));
    let (wait_result, killed) = supervise(
        &exit_rx,
        pid,
        deadline,
        options.mem_budget_mb.saturating_mul(BYTES_PER_MB),
    );
    let _ = wait_handle.join();

    // Post-exit stdio grace: keep reading while data still arrives; release
    // after both pipes end or the grace elapses idle.
    {
        let mut state = shared.state.lock().unwrap();
        state.last_data_at = Instant::now();
        loop {
            if state.readers_done >= 2 {
                break;
            }
            let idle = state.last_data_at.elapsed();
            if idle >= POST_EXIT_GRACE {
                break;
            }
            let wait_for = POST_EXIT_GRACE - idle;
            let (next, _timeout) = shared.condvar.wait_timeout(state, wait_for).unwrap();
            state = next;
        }
    }
    shared.accepting.store(false, Ordering::Relaxed);
    shared.stop.store(true, Ordering::Relaxed);
    let _ = reader_a.join();
    let _ = reader_b.join();

    let exit_code: Option<i64> = match &wait_result {
        Ok(status) => status.code().map(i64::from),
        Err(_) => None,
    };

    let finish_output = |shared: &ReaderShared| -> OutputSnapshot {
        let mut acc = shared.accumulator.lock().unwrap();
        acc.finish();
        let snapshot = acc.snapshot(true);
        acc.close_temp_file();
        snapshot
    };

    let format_output = |snapshot: &OutputSnapshot,
                         empty_text: &str,
                         acc_last_line_bytes: usize|
     -> (String, Option<BashToolDetails>) {
        let truncation = &snapshot.truncation;
        let mut text = if snapshot.content.is_empty() {
            empty_text.to_string()
        } else {
            snapshot.content.clone()
        };
        let mut details: Option<BashToolDetails> = None;
        if truncation.truncated {
            details = Some(BashToolDetails {
                truncation: Some(truncation.clone()),
                full_output_path: snapshot.full_output_path.clone(),
            });
            let start_line = truncation.total_lines - truncation.output_lines + 1;
            let end_line = truncation.total_lines;
            // pi renders a missing path as the literal "undefined"; tell the
            // model the truth instead so it doesn't try to read a
            // nonexistent file.
            let full_output_ref = match snapshot.full_output_path.as_deref() {
                Some(path) => format!("Full output: {path}"),
                None => "Full output unavailable: spill file could not be written".to_string(),
            };
            if truncation.last_line_partial {
                let last_line_size = format_size(acc_last_line_bytes);
                text.push_str(&format!(
                    "\n\n[Showing last {} of line {end_line} (line is {last_line_size}). {full_output_ref}]",
                    format_size(truncation.output_bytes)
                ));
            } else if truncation.truncated_by == Some("lines") {
                text.push_str(&format!(
                    "\n\n[Showing lines {start_line}-{end_line} of {}. {full_output_ref}]",
                    truncation.total_lines
                ));
            } else {
                text.push_str(&format!(
                    "\n\n[Showing lines {start_line}-{end_line} of {} ({} limit). {full_output_ref}]",
                    truncation.total_lines,
                    format_size(DEFAULT_MAX_BYTES)
                ));
            }
        }
        (text, details)
    };

    let append_status = |text: &str, status: &str| -> String {
        if text.is_empty() {
            status.to_string()
        } else {
            format!("{text}\n\n{status}")
        }
    };

    let snapshot = finish_output(&shared);
    let last_line_bytes = shared.accumulator.lock().unwrap().get_last_line_bytes();

    match killed {
        Some(KillReason::Timeout) => {
            let (text, _) = format_output(&snapshot, "", last_line_bytes);
            return Err(ToolError::new(append_status(
                &text,
                &format!(
                    "Command timed out after {} seconds",
                    js_num(args.timeout.unwrap_or(0.0))
                ),
            )));
        }
        Some(KillReason::MemoryBudget { observed_bytes }) => {
            // Reason first, then whatever the command managed to print: this
            // error IS the documentation of the budget — nothing else tells
            // the model the limit exists — so it leads, and the partial output
            // it kept follows as evidence.
            let (text, _) = format_output(&snapshot, "", last_line_bytes);
            return Err(ToolError::new(prepend_status(
                &memory_budget_notice(observed_bytes, options.mem_budget_mb),
                &text,
            )));
        }
        None => {}
    }

    let (output_text, details) = format_output(&snapshot, "(no output)", last_line_bytes);
    if let Some(code) = exit_code {
        if code != 0 {
            return Err(ToolError::new(append_status(
                &output_text,
                &format!("Command exited with code {code}"),
            )));
        }
    }

    Ok(ToolResult {
        content: vec![ContentBlock::text(output_text)],
        details,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The interval policy is a pure function of the recent samples, so the
    /// behavior a race-prone integration test could only sample is pinned here
    /// exactly: when the fast interval opens, and what it takes to close it.
    #[test]
    fn the_interval_tightens_near_the_budget_and_relaxes_after_a_run_of_cold_samples() {
        let budget = 1000 * BYTES_PER_MB;

        // Cold below the watermark, hot at it and above it.
        assert!(!sample_is_hot(699 * BYTES_PER_MB, budget));
        assert!(sample_is_hot(700 * BYTES_PER_MB, budget));
        assert!(sample_is_hot(2000 * BYTES_PER_MB, budget));
        // A command that has allocated nothing yet is not near anything.
        assert!(!sample_is_hot(0, budget));

        // A fresh supervisor starts relaxed, one hot sample tightens, and it
        // takes a full run of cold samples — not the first dip — to relax.
        let mut cold_run = MEM_HOT_DECAY_SAMPLES;
        assert_eq!(sample_interval(cold_run), MEM_SAMPLE_INTERVAL);
        cold_run = 0; // one hot sample
        assert_eq!(sample_interval(cold_run), MEM_SAMPLE_INTERVAL_HOT);
        for dips in 1..MEM_HOT_DECAY_SAMPLES {
            cold_run += 1;
            assert_eq!(
                sample_interval(cold_run),
                MEM_SAMPLE_INTERVAL_HOT,
                "relaxed after only {dips} cold sample(s); an oscillating tree is still near its budget"
            );
        }
        cold_run += 1;
        assert_eq!(sample_interval(cold_run), MEM_SAMPLE_INTERVAL);
    }

    /// The watermark must leave room to *use* the tighter interval — a hot
    /// window that opened at the budget itself would sample faster only after
    /// the breach it exists to catch. (That it is tighter at all, and that the
    /// watermark is below the budget, are compile-time assertions above.)
    #[test]
    fn the_hot_window_opens_with_room_left_to_act() {
        let budget = 3072 * BYTES_PER_MB;
        let watermark = budget * MEM_HOT_NUMERATOR / MEM_HOT_DENOMINATOR;
        // ~920 MB of slack at the default budget: many fast ticks even at the
        // fastest allocation rate measured (~840 MB/s single-threaded).
        assert!(budget - watermark > 512 * BYTES_PER_MB);
    }

    /// Saturating arithmetic on both sides: a sample large enough to overflow a
    /// naive multiply must still read as hot rather than wrapping to cold.
    #[test]
    fn an_absurd_sample_does_not_wrap_to_cold() {
        assert!(sample_is_hot(u64::MAX, 1024 * BYTES_PER_MB));
    }
}
