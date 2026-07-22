//! Port of pi's `core/tools/bash.ts` execute path.
//!
//! The command runs in its own session (pi's `detached: true`), stdout and
//! stderr stream into the OutputAccumulator, a timeout SIGKILLs the process
//! group, and after exit the pipes get pi's 100ms idle grace so output from
//! detached descendants isn't cut mid-write (`waitForChildProcess`).

use std::os::unix::io::AsRawFd;
use std::os::unix::process::{CommandExt, ExitStatusExt};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::RecvTimeoutError;
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

    let mut timed_out = false;
    let wait_result = match timeout_ms {
        Some(ms) => match exit_rx.recv_timeout(Duration::from_millis(ms as u64)) {
            Ok(status) => status,
            Err(RecvTimeoutError::Timeout) => {
                timed_out = true;
                kill_process_tree(pid);
                exit_rx
                    .recv()
                    .unwrap_or_else(|_| Ok(std::process::ExitStatus::from_raw(0)))
            }
            Err(RecvTimeoutError::Disconnected) => Ok(std::process::ExitStatus::from_raw(0)),
        },
        None => exit_rx
            .recv()
            .unwrap_or_else(|_| Ok(std::process::ExitStatus::from_raw(0))),
    };
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

    if timed_out {
        let (text, _) = format_output(&snapshot, "", last_line_bytes);
        return Err(ToolError::new(append_status(
            &text,
            &format!(
                "Command timed out after {} seconds",
                js_num(args.timeout.unwrap_or(0.0))
            ),
        )));
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
