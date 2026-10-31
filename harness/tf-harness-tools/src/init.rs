//! PID 1's duty for the resident tool host.
//!
//! The jail runs `serve` as its main process, which makes it PID 1, and PID 1
//! is handed every process whose parent dies: the descendants of a command a
//! timeout or the memory budget killed, and every background job that outlives
//! the shell that started it. Each stays a zombie until PID 1 waits for it,
//! holding a pid and showing up in `ps` and `pgrep` as if it were still there.
//!
//! `serve` cannot simply wait for any child. `bash` waits for its command's
//! shell by pid, and a reaper that got there first would take the shell's exit
//! status from it. So PID 1 splits in two: the parent stays PID 1 and only
//! reaps and forwards signals, and the child runs `serve`, whose shells stay
//! its own children.

/// When this process is PID 1, fork: the parent never returns, reaping every
/// process handed to it and exiting with the child's status once the child
/// exits; the child returns and carries on as the tool host. Anywhere else this
/// does nothing.
///
/// Call it before any thread starts, since only the calling thread survives a
/// fork.
#[cfg(target_os = "linux")]
pub fn fork_reaper_if_pid1() {
    if unsafe { libc::getpid() } != 1 {
        return;
    }
    unsafe {
        let mut waited: libc::sigset_t = std::mem::zeroed();
        libc::sigemptyset(&mut waited);
        libc::sigaddset(&mut waited, libc::SIGCHLD);
        for sig in FORWARDED {
            libc::sigaddset(&mut waited, sig);
        }
        // Blocked before the fork, so a signal that arrives between the fork
        // and the reaper's first wait is held for it rather than lost. PID 1
        // also drops a signal it has no handler for unless it is blocked.
        let mut previous: libc::sigset_t = std::mem::zeroed();
        libc::sigprocmask(libc::SIG_BLOCK, &waited, &mut previous);
        let child = libc::fork();
        if child > 0 {
            reap_until_child_exits(child, &waited);
        }
        // The child, or a failed fork, which leaves the tool host running as
        // PID 1 with nobody reaping its orphans.
        libc::sigprocmask(libc::SIG_SETMASK, &previous, std::ptr::null_mut());
    }
}

#[cfg(not(target_os = "linux"))]
pub fn fork_reaper_if_pid1() {}

/// The signals a stop or an operator sends PID 1, which belong to the tool
/// host.
#[cfg(target_os = "linux")]
const FORWARDED: [libc::c_int; 6] = [
    libc::SIGTERM,
    libc::SIGINT,
    libc::SIGHUP,
    libc::SIGQUIT,
    libc::SIGUSR1,
    libc::SIGUSR2,
];

#[cfg(target_os = "linux")]
unsafe fn reap_until_child_exits(child: libc::pid_t, waited: &libc::sigset_t) -> ! {
    // SIGCHLD wakes the wait; the timeout only bounds how long a lost wakeup
    // can leave a zombie standing.
    let timeout = libc::timespec {
        tv_sec: 1,
        tv_nsec: 0,
    };
    loop {
        let sig = libc::sigtimedwait(waited, std::ptr::null_mut(), &timeout);
        if sig > 0 && sig != libc::SIGCHLD {
            libc::kill(child, sig);
        }
        loop {
            let mut status = 0;
            let pid = libc::waitpid(-1, &mut status, libc::WNOHANG);
            if pid <= 0 {
                break;
            }
            if pid == child {
                let code = if libc::WIFEXITED(status) {
                    libc::WEXITSTATUS(status)
                } else {
                    128 + libc::WTERMSIG(status)
                };
                std::process::exit(code);
            }
        }
    }
}
