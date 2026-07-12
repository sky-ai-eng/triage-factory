package sandbox

// defaultAllowedSyscalls is the syscall allowlist for the default
// seccomp profile. Mirrors Docker's default profile (essentially
// "everything safe a normal process needs") minus the truly
// dangerous syscalls (mount, kexec_load, reboot, init_module,
// finit_module, delete_module, swapon, swapoff, etc.) which never
// appear in this list and are therefore implicitly denied by the
// default-errno action.
//
// Reference: https://docs.docker.com/engine/security/seccomp/
//
// IMPORTANT — inert under gVisor. Validated empirically (see
// docs/for-agents/specs/playwright-chromium-sandbox/probe-seccomp.sh): runsc does
// NOT apply this OCI container seccomp profile to the sandboxed
// application. A SCMP_ACT_KILL_PROCESS default with zero allowed
// syscalls had zero effect — and KILL outranks the RET_TRAP gVisor's
// systrap uses to intercept syscalls, so it cannot be shadowed; an
// applied profile would have killed the process on its first syscall.
// The real syscall boundary is the gVisor Sentry (it services the
// app's syscalls in user space; they never reach the host kernel) plus
// gVisor's own internal seccomp confining the Sentry's host syscalls.
// This profile is retained for OCI completeness and as defense-in-depth
// for any non-gVisor runtime — NOT as an enforced in-sandbox control.
// Don't "tighten" it expecting in-sandbox effect under gVisor: it has
// none. (Customer-facing copy with the same overstatement: TFAC-299.)
var defaultAllowedSyscalls = []string{
	"accept", "accept4", "access", "adjtimex", "alarm", "bind", "brk",
	"capget", "capset", "chdir", "chmod", "chown", "chown32", "clock_adjtime",
	"clock_adjtime64", "clock_getres", "clock_getres_time64", "clock_gettime",
	"clock_gettime64", "clock_nanosleep", "clock_nanosleep_time64", "close",
	"close_range", "connect", "copy_file_range", "creat", "dup", "dup2",
	"dup3", "epoll_create", "epoll_create1", "epoll_ctl", "epoll_ctl_old",
	"epoll_pwait", "epoll_pwait2", "epoll_wait", "epoll_wait_old",
	"eventfd", "eventfd2", "execve", "execveat", "exit", "exit_group",
	"faccessat", "faccessat2", "fadvise64", "fadvise64_64", "fallocate",
	"fanotify_mark", "fchdir", "fchmod", "fchmodat", "fchmodat2", "fchown",
	"fchown32", "fchownat", "fcntl", "fcntl64", "fdatasync", "fgetxattr",
	"flistxattr", "flock", "fork", "fremovexattr", "fsetxattr", "fstat",
	"fstat64", "fstatat64", "fstatfs", "fstatfs64", "fsync", "ftruncate",
	"ftruncate64", "futex", "futex_time64", "futex_waitv", "futimesat",
	"getcpu", "getcwd", "getdents", "getdents64", "getegid", "getegid32",
	"geteuid", "geteuid32", "getgid", "getgid32", "getgroups",
	"getgroups32", "getitimer", "getpeername", "getpgid", "getpgrp",
	"getpid", "getppid", "getpriority", "getrandom", "getresgid",
	"getresgid32", "getresuid", "getresuid32", "getrlimit", "get_robust_list",
	"getrusage", "getsid", "getsockname", "getsockopt", "get_thread_area",
	"gettid", "gettimeofday", "getuid", "getuid32", "getxattr",
	"inotify_add_watch", "inotify_init", "inotify_init1", "inotify_rm_watch",
	"io_cancel", "ioctl", "io_destroy", "io_getevents", "io_pgetevents",
	"io_pgetevents_time64", "ioprio_get", "ioprio_set", "io_setup",
	"io_submit", "io_uring_enter", "io_uring_register", "io_uring_setup",
	"ipc", "kill", "landlock_add_rule", "landlock_create_ruleset",
	"landlock_restrict_self", "lchown", "lchown32", "lgetxattr", "link",
	"linkat", "listen", "listxattr", "llistxattr", "_llseek", "lremovexattr",
	"lseek", "lsetxattr", "lstat", "lstat64", "madvise", "membarrier",
	"memfd_create", "memfd_secret", "mincore", "mkdir", "mkdirat", "mknod",
	"mknodat", "mlock", "mlock2", "mlockall", "mmap", "mmap2", "mprotect",
	"mq_getsetattr", "mq_notify", "mq_open", "mq_timedreceive",
	"mq_timedreceive_time64", "mq_timedsend", "mq_timedsend_time64",
	"mq_unlink", "mremap", "msgctl", "msgget", "msgrcv", "msgsnd", "msync",
	"munlock", "munlockall", "munmap", "name_to_handle_at", "nanosleep",
	"newfstatat", "_newselect", "open", "openat", "openat2", "pause",
	"pidfd_open", "pidfd_send_signal", "pipe", "pipe2", "poll", "ppoll",
	"ppoll_time64", "prctl", "pread64", "preadv", "preadv2", "prlimit64",
	"process_mrelease", "pselect6", "pselect6_time64", "pwrite64", "pwritev",
	"pwritev2", "read", "readahead", "readlink", "readlinkat", "readv",
	"recv", "recvfrom", "recvmmsg", "recvmmsg_time64", "recvmsg",
	"remap_file_pages", "removexattr", "rename", "renameat", "renameat2",
	"restart_syscall", "rmdir", "rseq", "rt_sigaction", "rt_sigpending",
	"rt_sigprocmask", "rt_sigqueueinfo", "rt_sigreturn", "rt_sigsuspend",
	"rt_sigtimedwait", "rt_sigtimedwait_time64", "rt_tgsigqueueinfo",
	"sched_getaffinity", "sched_getattr", "sched_getparam", "sched_get_priority_max",
	"sched_get_priority_min", "sched_getscheduler", "sched_rr_get_interval",
	"sched_rr_get_interval_time64", "sched_setaffinity", "sched_setattr",
	"sched_setparam", "sched_setscheduler", "sched_yield", "semctl",
	"semget", "semop", "semtimedop", "semtimedop_time64", "send", "sendfile",
	"sendfile64", "sendmmsg", "sendmsg", "sendto", "setfsgid", "setfsgid32",
	"setfsuid", "setfsuid32", "setgid", "setgid32", "setgroups", "setgroups32",
	"setitimer", "setpgid", "setpriority", "setregid", "setregid32",
	"setresgid", "setresgid32", "setresuid", "setresuid32", "setreuid",
	"setreuid32", "setrlimit", "set_robust_list", "setsid", "setsockopt",
	"set_thread_area", "set_tid_address", "setuid", "setuid32", "setxattr",
	"shmat", "shmctl", "shmdt", "shmget", "shutdown", "sigaltstack",
	"signalfd", "signalfd4", "sigprocmask", "sigreturn", "socket",
	"socketcall", "socketpair", "splice", "stat", "stat64", "statfs",
	"statfs64", "statx", "symlink", "symlinkat", "sync", "sync_file_range",
	"syncfs", "sysinfo", "tee", "tgkill", "time", "timer_create",
	"timer_delete", "timer_getoverrun", "timer_gettime", "timer_gettime64",
	"timer_settime", "timer_settime64", "timerfd_create", "timerfd_gettime",
	"timerfd_gettime64", "timerfd_settime", "timerfd_settime64", "times",
	"tkill", "truncate", "truncate64", "ugetrlimit", "umask", "uname",
	"unlink", "unlinkat", "utime", "utimensat", "utimensat_time64", "utimes",
	"vfork", "vmsplice", "wait4", "waitid", "waitpid", "write", "writev",
}
