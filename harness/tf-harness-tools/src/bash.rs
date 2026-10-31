//! Port of pi's `core/tools/bash.ts` execute path.
//!
//! The command runs in its own session (pi's `detached: true`), stdout and
//! stderr stream into the OutputAccumulator, a timeout SIGKILLs every process
//! in the command's tree ([`kill_command_tree`]), and after exit the pipes get
//! pi's 100ms idle grace so output from detached descendants isn't cut
//! mid-write (`waitForChildProcess`).
//!
//! One divergence from pi is in how a command ends: a shell that dies of a
//! signal is an error naming the signal, where pi returns its partial output
//! as a success. In a jail the usual signal is the SIGKILL the kernel sends
//! when the jail reaches its memory ceiling, and `bash -c` execs a single
//! simple command in place of the shell, so that kill lands on the shell
//! process itself.
//!
//! # The per-command memory budget
//!
//! Another divergence rides the same supervisor: when the session was
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
//!
//! # Time limits
//!
//! pi has no default timeout and no maximum. A configured session can have
//! both: a command given no timeout gets the default, and a timeout above the
//! maximum is refused before anything runs. The maximum is what lets a caller
//! with its own deadline for a tool call count on the command ending, and
//! being answered as one that timed out, before that deadline passes.
//! Unconfigured, as the one-shot CLI and the parity corpus run, there is
//! neither.
//!
//! # Background processes that keep the output open
//!
//! A process the command starts in the background inherits its stdout and
//! stderr, so the pipes stay open after the shell exits. Read the way pi
//! reads them, a process that logs steadily holds the call open forever, and
//! one that is quiet when the call returns dies of SIGPIPE the next time it
//! writes, because the pipes are closed under it. Here the reading after exit
//! stops at [`POST_EXIT_GRACE_CAP`] at the latest, and the pipes are drained
//! rather than closed until the process closes them: it keeps running, its
//! later output is discarded, and the result says so.

use std::collections::{HashMap, HashSet};
use std::os::unix::io::AsRawFd;
use std::os::unix::process::{CommandExt, ExitStatusExt};
use std::path::Path;
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

/// The longest the output is read after the shell exits. The idle grace above
/// ends on a pipe that goes quiet; this ends it on one a background process
/// keeps writing to.
const POST_EXIT_GRACE_CAP: Duration = Duration::from_secs(2);

/// How to run something that should outlast a tool call, in a form the model
/// can copy.
const BACKGROUND_EXAMPLE: &str = "`cmd > /tmp/cmd.log 2>&1 &`";

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

/// Upper bound on ids visited in one walk of a command's tree. Under gVisor a
/// process's threads are listed beside it, so this counts threads as well as
/// processes; it backstops a pathological fan-out from turning a sampling tick
/// into an unbounded `/proc` walk.
const MAX_WALKED_IDS: usize = 4096;

/// How many times a kill re-walks the stopped tree for processes forked since
/// the last walk. A tree that is still growing after this many passes is
/// killed as far as it was walked; the jail's ceiling stays underneath.
const MAX_STOP_PASSES: usize = 8;

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
    /// Timeout, in seconds, for a command the model gives none. Zero leaves
    /// such a command unbounded, as pi does.
    pub default_timeout_s: u64,
    /// The largest timeout, in seconds, the model may give; a larger one is
    /// refused before the command runs. Zero leaves only pi's own maximum.
    pub max_timeout_s: u64,
}

/// A command's deadline, and whether the model chose it: what the model is
/// told when it fires depends on which.
#[derive(Debug, Clone, Copy, PartialEq)]
struct Deadline {
    ms: f64,
    defaulted: bool,
}

fn resolve_timeout(
    timeout: Option<f64>,
    options: &BashOptions,
) -> Result<Option<Deadline>, ToolError> {
    let Some(timeout) = timeout else {
        let mut default_s = options.default_timeout_s;
        if options.max_timeout_s > 0 {
            default_s = default_s.min(options.max_timeout_s);
        }
        return Ok((default_s > 0).then_some(Deadline {
            ms: default_s as f64 * 1000.0,
            defaulted: true,
        }));
    };
    if !timeout.is_finite() || timeout <= 0.0 {
        return Err(ToolError::new(
            "Invalid timeout: must be a finite number of seconds",
        ));
    }
    if options.max_timeout_s > 0 && timeout > options.max_timeout_s as f64 {
        return Err(ToolError::new(format!(
            "Invalid timeout: maximum is {}. To run something longer, start it in the background with its \
output in a file ({BACKGROUND_EXAMPLE}) and check on it with later commands.",
            seconds_with_minutes(options.max_timeout_s)
        )));
    }
    let timeout_ms = timeout * 1000.0;
    if timeout_ms > MAX_TIMEOUT_MS {
        return Err(ToolError::new(format!(
            "Invalid timeout: maximum is {} seconds",
            js_num(MAX_TIMEOUT_MS / 1000.0)
        )));
    }
    Ok(Some(Deadline {
        ms: timeout_ms,
        defaulted: false,
    }))
}

/// A whole number of minutes as words: "10 minutes", "1 minute". None when
/// `seconds` is not one.
fn whole_minutes(seconds: u64) -> Option<String> {
    if seconds == 0 || !seconds.is_multiple_of(60) {
        return None;
    }
    let minutes = seconds / 60;
    Some(if minutes == 1 {
        "1 minute".to_string()
    } else {
        format!("{minutes} minutes")
    })
}

/// A number of seconds as words: "1 second", "90 seconds".
fn seconds_phrase(seconds: u64) -> String {
    if seconds == 1 {
        "1 second".to_string()
    } else {
        format!("{seconds} seconds")
    }
}

/// A limit in the unit the `timeout` argument takes, with minutes beside it
/// when they are whole: "1740 seconds (29 minutes)".
fn seconds_with_minutes(seconds: u64) -> String {
    match whole_minutes(seconds) {
        Some(minutes) => format!("{} ({minutes})", seconds_phrase(seconds)),
        None => seconds_phrase(seconds),
    }
}

/// What the model is told when the default timeout ends a command it gave
/// no timeout. It names the limit, since nothing else says one exists, and
/// the two ways past it.
fn default_timeout_notice(default_s: u64, max_s: u64) -> String {
    let limit = whole_minutes(default_s).unwrap_or_else(|| seconds_phrase(default_s));
    let longer = if max_s > 0 {
        format!("pass a timeout of up to {}", seconds_with_minutes(max_s))
    } else {
        "pass a longer timeout".to_string()
    };
    format!(
        "Command timed out after {limit}, the limit for a command run without a timeout. For a command that \
needs longer, {longer}. For anything longer than that, or anything that does not exit on its own such as a \
server or a watcher, start it in the background with its output in a file ({BACKGROUND_EXAMPLE}) and check \
on it with later commands."
    )
}

/// What the model is told when a process the command started in the
/// background still holds its output as the call returns. Without it the
/// model would not know the process outlived the command, or why it will
/// never see that process's output.
fn background_holder_notice() -> String {
    format!(
        "A process this command started in the background is still attached to its output. It keeps \
running, but what it prints from now on is discarded; to keep that, redirect its output to a file \
({BACKGROUND_EXAMPLE})."
    )
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
            kill_command_tree(pid);
            return (reap(), Some(reason));
        }
    }
}

/// SIGKILL every process in the command's tree, including the ones that left
/// its process group.
///
/// The process-group kill alone misses a process that called `setsid` or
/// `setpgid`, though the budget measured it as part of the command: it keeps
/// running and keeps what it allocated, with nothing left counting it. So the
/// tree is stopped, walked again until no new process appears, and only then
/// killed. A stopped process can neither fork nor exit, so the stopped set is
/// the whole tree when the passes end; killing as it walked would hand each
/// killed parent's children to PID 1, out of the walk's reach.
pub fn kill_command_tree(pid: i32) {
    let proc = Path::new("/proc");
    let mut stopped: HashSet<i32> = HashSet::new();
    for _ in 0..MAX_STOP_PASSES {
        let found: Vec<i32> = walk_tree(proc, pid)
            .processes
            .into_iter()
            .filter(|p| !stopped.contains(p))
            .collect();
        if found.is_empty() {
            break;
        }
        for p in found {
            unsafe { libc::kill(p, libc::SIGSTOP) };
            stopped.insert(p);
        }
    }
    kill_process_tree(pid);
    for p in stopped {
        unsafe { libc::kill(p, libc::SIGKILL) };
    }
}

/// Sum resident memory, in bytes, across the process tree rooted at `pid`.
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
    walk_tree(Path::new("/proc"), pid)
        .resident_pages
        .saturating_mul(page_size())
}

/// One walk of a command's process tree.
struct Tree {
    /// Every process in the tree, by thread-group id.
    processes: Vec<i32>,
    resident_pages: u64,
}

/// Walk the process tree rooted at `root` in the procfs mounted at `proc`.
///
/// Children come from `/proc/<id>/task/<tid>/children` and the resident set
/// from field 2 of `/proc/<id>/statm` (resident pages), served by gVisor's
/// procfs for the Sentry's own tasks.
///
/// Each process counts once however many threads it has. gVisor's `children`
/// file lists a child process's threads beside the process itself, and a
/// thread's `statm` reports the whole process's resident set, so summing every
/// listed id would count each process once per thread — a multi-threaded
/// compiler reads several times its real size and is killed far under budget.
/// So every id is resolved to its process through the `Tgid:` line of its
/// `status`, and a process's size is the first nonzero `statm` among its ids:
/// one whose main thread has exited reads zero through the leader and its real
/// size through a live thread.
fn walk_tree(proc: &Path, root: i32) -> Tree {
    let mut tree = Tree {
        processes: Vec::new(),
        resident_pages: 0,
    };
    let mut pending = vec![root];
    let mut seen: HashSet<i32> = HashSet::new();
    // Per process: whether its size has been counted, and whether its children
    // have been listed.
    let mut accounted: HashMap<i32, (bool, bool)> = HashMap::new();
    while let Some(id) = pending.pop() {
        if !seen.insert(id) {
            continue;
        }
        if seen.len() > MAX_WALKED_IDS {
            break;
        }
        let tgid = thread_group(proc, id).unwrap_or(id);
        let (sized, listed) = accounted.entry(tgid).or_insert_with(|| {
            tree.processes.push(tgid);
            (false, false)
        });
        if !*sized {
            let pages = resident_pages(proc, id);
            if pages > 0 {
                tree.resident_pages = tree.resident_pages.saturating_add(pages);
                *sized = true;
            }
        }
        if !*listed {
            *listed = collect_children(proc, id, &mut pending);
        }
    }
    tree
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
fn resident_pages(proc: &Path, pid: i32) -> u64 {
    let Ok(statm) = std::fs::read_to_string(proc.join(pid.to_string()).join("statm")) else {
        return 0;
    };
    statm
        .split_whitespace()
        .nth(1)
        .and_then(|field| field.parse().ok())
        .unwrap_or(0)
}

/// The `Tgid:` line of `/proc/<id>/status`: the process a thread id belongs
/// to. None when the id is gone or its status cannot be read.
fn thread_group(proc: &Path, id: i32) -> Option<i32> {
    let status = std::fs::read_to_string(proc.join(id.to_string()).join("status")).ok()?;
    status
        .lines()
        .find_map(|line| line.strip_prefix("Tgid:"))
        .and_then(|tgid| tgid.trim().parse().ok())
}

/// Append `id`'s process's direct children to `out`, from every thread's
/// `children` file — a child is listed under the thread that forked it, not
/// under the thread group. False when the thread list cannot be read.
fn collect_children(proc: &Path, id: i32, out: &mut Vec<i32>) -> bool {
    let Ok(tasks) = std::fs::read_dir(proc.join(id.to_string()).join("task")) else {
        return false;
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
    true
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

/// Reads the blocking pipe fd until its last writer closes it, keeping
/// nothing: one read per buffer, however fast the writer is.
fn discard_until_eof(fd: i32, buf: &mut [u8]) {
    loop {
        let n = unsafe { libc::read(fd, buf.as_mut_ptr().cast(), buf.len()) };
        if n > 0 {
            continue;
        }
        if n < 0 && std::io::Error::last_os_error().raw_os_error() == Some(libc::EINTR) {
            continue;
        }
        return;
    }
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
            if !shared.accepting.load(Ordering::Relaxed) {
                // The call has returned and a background process holds the
                // pipe. What it writes is discarded, so it is read away with
                // nothing to wait on and no lock to take.
                discard_until_eof(fd, &mut buf);
                break;
            }
            let data = &buf[..n as usize];
            // Read under the accumulator's lock, so nothing is appended once
            // the call has taken its output: a pipe drained for a process
            // still running in the background keeps arriving here.
            {
                let mut acc = shared.accumulator.lock().unwrap();
                if shared.accepting.load(Ordering::Relaxed) {
                    acc.append(data);
                }
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

/// A signal as the model is told it: the name and the number. The number is
/// what lets the model match it to the `128 + N` exit code a shell reports
/// for a child that died the same way (`SIGKILL (signal 9)` and code 137).
fn describe_signal(signal: i32) -> String {
    let name = match signal {
        libc::SIGHUP => "SIGHUP",
        libc::SIGINT => "SIGINT",
        libc::SIGQUIT => "SIGQUIT",
        libc::SIGILL => "SIGILL",
        libc::SIGTRAP => "SIGTRAP",
        libc::SIGABRT => "SIGABRT",
        libc::SIGBUS => "SIGBUS",
        libc::SIGFPE => "SIGFPE",
        libc::SIGKILL => "SIGKILL",
        libc::SIGUSR1 => "SIGUSR1",
        libc::SIGSEGV => "SIGSEGV",
        libc::SIGUSR2 => "SIGUSR2",
        libc::SIGPIPE => "SIGPIPE",
        libc::SIGALRM => "SIGALRM",
        libc::SIGTERM => "SIGTERM",
        libc::SIGXCPU => "SIGXCPU",
        libc::SIGXFSZ => "SIGXFSZ",
        libc::SIGSYS => "SIGSYS",
        _ => return format!("signal {signal}"),
    };
    format!("{name} (signal {signal})")
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
    let deadline_spec = resolve_timeout(args.timeout, options)?;

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
            // pi spawns detached; a fresh session makes -pid reach every
            // process that stays in the command's group.
            if libc::setsid() == -1 {
                return Err(std::io::Error::last_os_error());
            }
            // An ignored signal stays ignored across exec, and the serving
            // tool host ignores SIGXFSZ for its own sake
            // (serve::ignore_file_size_signal); the command gets the default.
            if libc::signal(libc::SIGXFSZ, libc::SIG_DFL) == libc::SIG_ERR {
                return Err(std::io::Error::last_os_error());
            }
            // The command's orphans are handed to its shell rather than to
            // PID 1, so a double-forked daemon stays in the tree the budget
            // measures and the kill walks for as long as the shell runs.
            // Best-effort: without it only that daemon escapes, which is no
            // reason to refuse the command.
            #[cfg(target_os = "linux")]
            libc::prctl(libc::PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0);
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

    let deadline = deadline_spec.map(|d| Instant::now() + Duration::from_millis(d.ms as u64));
    let (wait_result, killed) = supervise(
        &exit_rx,
        pid,
        deadline,
        options.mem_budget_mb.saturating_mul(BYTES_PER_MB),
    );
    let _ = wait_handle.join();

    // Post-exit stdio grace: keep reading while data still arrives; release
    // after both pipes end, the grace elapses idle, or the cap is reached.
    // held is a pipe still open when it ends: something the command started
    // in the background has it.
    let held = {
        let mut state = shared.state.lock().unwrap();
        let grace_started = Instant::now();
        state.last_data_at = grace_started;
        loop {
            if state.readers_done >= 2 {
                break false;
            }
            let idle = state.last_data_at.elapsed();
            let reading = grace_started.elapsed();
            if idle >= POST_EXIT_GRACE || reading >= POST_EXIT_GRACE_CAP {
                break true;
            }
            let wait_for = (POST_EXIT_GRACE - idle).min(POST_EXIT_GRACE_CAP - reading);
            let (next, _timeout) = shared.condvar.wait_timeout(state, wait_for).unwrap();
            state = next;
        }
    };
    // Under the accumulator's lock, the lock every append takes, so no
    // reader appends after this.
    {
        let _acc = shared.accumulator.lock().unwrap();
        shared.accepting.store(false, Ordering::Relaxed);
    }
    if held {
        // The readers are left draining: closing a pipe a process still
        // writes to would kill it with SIGPIPE. Each ends at its pipe's EOF,
        // when the last process holding it exits.
        drop(reader_a);
        drop(reader_b);
    } else {
        shared.stop.store(true, Ordering::Relaxed);
        let _ = reader_a.join();
        let _ = reader_b.join();
    }
    let with_holder_note = |text: String| -> String {
        if !held {
            return text;
        }
        let note = background_holder_notice();
        if text.is_empty() {
            note
        } else {
            format!("{text}\n\n{note}")
        }
    };

    let exit_code: Option<i64> = match &wait_result {
        Ok(status) => status.code().map(i64::from),
        Err(_) => None,
    };
    let killed_by: Option<i32> = match &wait_result {
        Ok(status) => status.signal(),
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
            let status = match deadline_spec {
                Some(Deadline {
                    ms,
                    defaulted: true,
                }) => default_timeout_notice((ms / 1000.0) as u64, options.max_timeout_s),
                _ => format!(
                    "Command timed out after {} seconds",
                    js_num(args.timeout.unwrap_or(0.0))
                ),
            };
            return Err(ToolError::new(with_holder_note(append_status(
                &text, &status,
            ))));
        }
        Some(KillReason::MemoryBudget { observed_bytes }) => {
            // Reason first, then whatever the command managed to print: this
            // error IS the documentation of the budget — nothing else tells
            // the model the limit exists — so it leads, and the partial output
            // it kept follows as evidence.
            let (text, _) = format_output(&snapshot, "", last_line_bytes);
            return Err(ToolError::new(with_holder_note(prepend_status(
                &memory_budget_notice(observed_bytes, options.mem_budget_mb),
                &text,
            ))));
        }
        None => {}
    }

    let (output_text, details) = format_output(&snapshot, "(no output)", last_line_bytes);
    // pi answers a shell that died of a signal as a success, because Node
    // reports its exit code as null. Here it is an error: the command did not
    // finish, and what it printed before the signal is not its result.
    if let Some(signal) = killed_by {
        return Err(ToolError::new(append_status(
            &output_text,
            &format!("Command was killed by {}", describe_signal(signal)),
        )));
    }
    if let Some(code) = exit_code {
        if code != 0 {
            return Err(ToolError::new(with_holder_note(append_status(
                &output_text,
                &format!("Command exited with code {code}"),
            ))));
        }
    }

    Ok(ToolResult {
        content: vec![ContentBlock::text(with_holder_note(output_text))],
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

    /// A signal with no conventional name still reaches the model as its
    /// number, never as an empty or made-up name.
    #[test]
    fn a_signal_is_named_with_its_number() {
        assert_eq!(describe_signal(libc::SIGKILL), "SIGKILL (signal 9)");
        assert_eq!(describe_signal(libc::SIGSEGV), "SIGSEGV (signal 11)");
        assert_eq!(describe_signal(64), "signal 64");
    }

    /// The default applies only to a command given no timeout, never exceeds
    /// the maximum, and a timeout over the maximum is refused; unconfigured,
    /// nothing changes.
    #[test]
    fn a_session_s_time_limits_resolve_a_command_s_deadline() {
        let limits = BashOptions {
            default_timeout_s: 600,
            max_timeout_s: 1740,
            ..Default::default()
        };
        assert_eq!(
            resolve_timeout(None, &limits).unwrap(),
            Some(Deadline {
                ms: 600_000.0,
                defaulted: true
            })
        );
        assert_eq!(
            resolve_timeout(Some(1740.0), &limits).unwrap(),
            Some(Deadline {
                ms: 1_740_000.0,
                defaulted: false
            })
        );
        assert_eq!(
            resolve_timeout(Some(1741.0), &limits).unwrap_err().0,
            "Invalid timeout: maximum is 1740 seconds (29 minutes). To run something longer, start it in the \
background with its output in a file (`cmd > /tmp/cmd.log 2>&1 &`) and check on it with later commands."
        );
        let default_over_max = BashOptions {
            default_timeout_s: 600,
            max_timeout_s: 60,
            ..Default::default()
        };
        assert_eq!(
            resolve_timeout(None, &default_over_max).unwrap(),
            Some(Deadline {
                ms: 60_000.0,
                defaulted: true
            })
        );
        assert_eq!(
            resolve_timeout(None, &BashOptions::default()).unwrap(),
            None
        );
    }

    /// Saturating arithmetic on both sides: a sample large enough to overflow a
    /// naive multiply must still read as hot rather than wrapping to cold.
    #[test]
    fn an_absurd_sample_does_not_wrap_to_cold() {
        assert!(sample_is_hot(u64::MAX, 1024 * BYTES_PER_MB));
    }

    /// gVisor's procfs, as the walk meets it: the shell's `children` file lists
    /// each child process beside its threads, and every one of those ids
    /// answers with the whole process's `task/` and `statm`. Process 2 is
    /// reached through a thread first, and process 10, whose main thread has
    /// exited and reads zero, through its leader first.
    #[test]
    fn each_process_is_counted_once_whichever_id_reaches_it() {
        let proc = tempfile::tempdir().unwrap();
        let process = |tgid: u32, threads: &[(u32, u64)], children: &str| {
            for (id, rss) in threads {
                let dir = proc.path().join(id.to_string());
                for (tid, _) in threads {
                    let task = dir.join("task").join(tid.to_string());
                    std::fs::create_dir_all(&task).unwrap();
                    let listed = if *tid == tgid { children } else { "" };
                    std::fs::write(task.join("children"), listed).unwrap();
                }
                std::fs::write(dir.join("status"), format!("Name:\tx\nTgid:\t{tgid}\n")).unwrap();
                std::fs::write(dir.join("statm"), format!("9000 {rss} 0 0 0 0 0\n")).unwrap();
            }
        };
        process(1, &[(1, 10)], "2 5 6 11 10");
        process(2, &[(2, 300), (5, 300), (6, 300)], "");
        process(10, &[(10, 0), (11, 200)], "");

        let tree = walk_tree(proc.path(), 1);
        assert_eq!(tree.resident_pages, 10 + 300 + 200);
        let mut processes = tree.processes;
        processes.sort();
        assert_eq!(processes, vec![1, 2, 10]);
    }
}
