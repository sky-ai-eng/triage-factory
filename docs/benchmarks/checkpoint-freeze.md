# Checkpoint freeze — what the capture jail costs and what a pause does to a run

A checkpoint of a live native engagement pauses the run's tool-host jail
(`runsc pause`), captures the run tree in a one-shot capture jail, and resumes.
This page records what that costs and what the agent can observe while it is
paused. The design is in `docs/security/privilege-separation.md`.

Measured with runsc `release-20260511.0`, `--platform=systrap --ignore-cgroups`,
on a single Linux host, as root. Numbers are wall-clock and vary with the host;
read the ratios and the shapes, not the absolute values.

## Fixed costs

| Step | Time |
| --- | --- |
| `runsc pause` | ~19 ms |
| `runsc resume` | ~28 ms |
| Capture jail launch plus a capture of a small tree | ~170 ms |

For a tree of a few files, a checkpoint holds the run paused for about 0.2 s.

## Large tree

The tree is a clone of a real repository with a local commit, 3,000 modified
tracked files, 3,000 untracked files, and one 200 MiB incompressible binary.
The capture produces a ~274 MB stream.

| | Host (same capture body, same uid) | Capture jail |
| --- | --- | --- |
| Whole capture, 3 runs | 6.7 s, 6.7 s, 6.7 s | 11.6 s, 11.8 s, 11.8 s |

The same operations run as a script on the host and in a run jail:

| Phase | Host | Jail |
| --- | --- | --- |
| Walk every file | 0.01 s | 0.18 s |
| Read the 200 MiB file | 0.01 s | 0.03 s |
| Write 200 MiB to the jail's memory-backed `/tmp` | 0.05 s | 1.6 s |
| `git add -A` into a throwaway index | 3.5 s | 5.3 s |
| `git write-tree` | 0.03 s | 0.11 s |

Most of the capture is hashing and compression, which cost the same in both.
The jail's added ~3.4 s is file-syscall cost under systrap, concentrated in
`git add` over many small files and in the large write. This is the same
per-syscall trap tax described in `gvisor-cpu-tax.md`; nothing in the capture
avoids it short of making fewer file operations.

Because the run is paused for the whole capture, a tree this size pauses it for
about 12 s per checkpoint. At the default five-minute interval that is about 4%
of wall time. The cost grows with the size of the uncommitted change, not with
the size of the repository.

## What a run observes during a freeze

Measured with a 12 s pause.

- **The model loop** runs in the executor process, outside the jail, and is not
  paused.
- **A tool call that starts during the capture** waits at the checkpoint
  barrier before it is dispatched. The wait counts as engagement activity, so
  the stall watchdog does not read it as a hung run. The call returns
  normally, only later.
- **A tool call already running** is paused with the jail and continues on
  resume. A `timeout 10` wrapped around 8 s of work did not fire across the
  12 s freeze, although the jail's wall clock and uptime both advanced by the
  full 12 s.
- **Wall-clock readings** inside the jail include the freeze. A tool that
  measures its own duration reports the longer figure.
- **A TCP connection** from the jail to a host listener, sending a line a
  second, delivered every line across the freeze (longest gap 12.95 s) and
  closed cleanly. gVisor does not reset or expire an idle connection during a
  pause. The far end's own idle timeout is outside the jail's control, so a
  server that drops idle connections in under the freeze length can still drop
  one.

## Not measured

- The tool host's own `bash` timeout, a Rust `recv_timeout`. The `timeout`
  result above is busybox's.
- The host-unix-socket path the `tfac exec` verbs use.
- Remote servers' idle timeouts.
- Peak memory of the capture jail on trees larger than the one above. It fit in
  the 3 GiB memory-backed `/tmp` and 4 GiB cgroup.

## Reproducing

The tests are in `internal/sandbox/capturejail_bench_integration_linux_test.go`
(build tag `integration`, root and runsc required):

- `TestIntegration_CaptureJail_LargeTreeTiming` and
  `TestIntegration_CaptureJail_PhaseCosts` need `TF_BENCH_SRC` set to a
  repository to clone as the tree.
- `TestIntegration_PauseResume_ClockAcrossTheFreeze` and
  `TestIntegration_FreezeConns` need nothing further.

Run them with `scripts/test-sandbox-linux.sh`, or build the test binary with
`go test -tags integration -c` and run it as root inside the delegated cgroup
scope that script sets up.
