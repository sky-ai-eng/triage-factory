# Tailored seccomp profile

The `triagefactory` service in `docker-compose.yml` runs with `security_opt:
seccomp=docker/seccomp-profile.json` — a default-deny allowlist, not
`seccomp=unconfined`. See
[security-overview.md](security-overview.md) §3/§6 vector 2 for the
threat-model framing.

This profile applies to the **sandbox-hosting** services only — the `all`
default and every `executor`. A capless control pod launches no sandbox and
so needs none of it: `docker-compose.control.yml` clears `security_opt` (and
the caps) via Compose's `!reset`, leaving the control pod on Docker's default
seccomp profile.

**Scope — self-host `docker-compose` only.** Fly.io-hosted production (`fly.toml`)
runs with unconfined seccomp inside its Fly Machine and is not covered by this
profile — the Fly Machine config format has no `security_opt`-equivalent field to
attach one to. Each Fly Machine is already its own Firecracker microVM, a
per-tenant hardware isolation boundary rather than a kernel shared with other
tenants, which is a different property from what this profile hardens on
self-host's shared Docker engine.

**Why a profile is needed at all.** Docker's own default seccomp profile blocks
several syscalls that `runsc` (gVisor, `--platform=systrap`) needs to construct its
own, far stricter, per-run sandbox — which makes `unconfined` the obvious answer and
the wrong one. It reads as "no syscall filtering at all" to a security reviewer,
which overstates what is actually required: the gap is a single syscall.

**Scope — this is the host-level profile, not the in-sandbox one.**
`docker/seccomp-profile.json` constrains the `triagefactory` **container** as seen
by the host kernel — i.e., the TF binary itself, `ip`/`iptables`/`sysctl`, the
`chroot`+`apk` rootfs bake, and `runsc` (and everything `runsc` forks). It is
unrelated to the OCI seccomp profile `internal/sandbox/spec.go` attaches to the
*sandboxed agent's* OCI spec (`internal/sandbox/syscalls.go`'s
`defaultAllowedSyscalls`) — that one is inert under gVisor (`runsc` doesn't enforce
the app-facing OCI seccomp list at all; see the validation in
`docs/for-agents/specs/playwright-chromium-sandbox/README.md` §5.4/§8). Don't
conflate the two: this profile is real host-kernel enforcement, the other is a
no-op today.

**What it allows.** `docker/seccomp-profile.json` starts from
[Docker/Moby's own default seccomp profile](https://github.com/moby/moby/blob/master/profiles/seccomp/default.json)
— a widely audited, default-deny (`SCMP_ACT_ERRNO`) allowlist, not a hand-rolled
one — and applies two changes:

1. **Removes** syscalls the executor has no legitimate use for and that are
   meaningfully dangerous if a compromised process ever reached them: `reboot`,
   `init_module`, `delete_module`, `iopl`, `ioperm`, `acct`, `quotactl`,
   `quotactl_fd`, `bpf`, `perf_event_open`, the fanotify family (`fanotify_init`,
   `fanotify_mark`), `landlock_create_ruleset`, `landlock_add_rule`,
   `landlock_restrict_self`, `lookup_dcookie`, `syslog`, `vhangup`. None of these
   are exercised anywhere in the netns/veth/iptables/cgroup/chroot+apk/runsc
   lifecycle. Removing them is defense-in-depth on top of the capability gate: the
   container's `CAP_SYS_ADMIN`/`CAP_NET_ADMIN` grant would otherwise let the
   kernel's own capability check wave some of these through if seccomp didn't block
   them first.
2. **Adds** `pivot_root` — the one syscall empirical validation found Docker's
   default profile missing for `runsc` to construct the per-run sandbox's mount
   namespace. Folded into the profile's existing `mount`/`umount2`/`unshare`/`setns`
   rule group (`includes.caps: [CAP_SYS_ADMIN]`) rather than allowed unconditionally
   — `pivot_root(2)` itself requires `CAP_SYS_ADMIN`, same as its neighbors in that
   group, so gating it the same way keeps the profile internally consistent and
   gives defense-in-depth if this container's capability grant is ever narrowed
   later. Everything else `runsc`'s systrap platform
   needs (`mount`, `umount2`, `chroot`, `ptrace`, `unshare`, `setns`,
   `clone`/`clone3`, `capset`/`capget`, `process_vm_readv`/`process_vm_writev`,
   `seccomp`, `prctl`, `personality`, `memfd_create`, …) was already present in
   Docker's default allowlist — enforced there via the same capability checks, same
   as production.

The result is ~400 allowed syscalls (vs. Docker's default ~415, vs. effectively all
~450 under `unconfined`) — a real reduction, though the primary privilege boundary
remains the two capabilities the container holds, not the syscall count.
`userfaultfd` (a syscall we speculated `runsc` might also need) was tested and found
**not** required — it stays out of the allowlist.

**Compatibility note — older Docker/libseccomp.** Because the base profile is copied
from a recent Moby release, it names a handful of newer syscalls (`cachestat`,
`mount_setattr`, `move_mount`, `fsopen`/`fsconfig`/`fsmount`/`fspick`/`open_tree` —
the new mount API, plus a few others). A sufficiently old Docker Engine /
libseccomp doesn't recognize those names when parsing the profile, which fails the
**whole container's startup** (not just that one rule) with an error naming the
unrecognized syscall — this is a real, historically-seen Docker/libseccomp
compatibility class, distinct from anything in this profile's own design. If
`docker compose up` fails this way: upgrade Docker Engine (and the bundled
libseccomp) to a current release, or — if you can't upgrade — delete the
unrecognized syscall name(s) from the offending rule's `names` array in
`docker/seccomp-profile.json` and re-run the validation procedure below; a kernel
old enough to ship a Docker/libseccomp that predates these names almost certainly
predates the syscalls themselves too, so removing the name costs you nothing on
that host.

## Validation

Validated across the full run lifecycle on real `runsc` (release-20260511, matching
the pin in `docker/Dockerfile`): sandbox spawn, agent execution (including real
network egress from inside the gVisor sandbox through the netns/veth/NAT/egress-
allowlist path — `internal/sandbox/netns_linux.go`, `iptables_linux.go`), teardown,
and orphan reap (`internal/sandbox/reaper_linux.go`), plus the `chroot`+`apk`
rootfs toolchain bake (`internal/sandbox/rootfs_linux.go`) and the agent-host IPC
socket path (`cmd/exec/agenthost/`). No browser sandbox profile exists in the
codebase yet (`docs/for-agents/specs/playwright-chromium-sandbox/` is still
proposal-stage — no `Config.Profile` field), so there was nothing browser-specific
to validate.

Harness: `go test -tags integration ./internal/sandbox/...` (the same suite
`scripts/test-sandbox-linux.sh` runs) built as a static binary and run inside a
container mirroring the compose service's exact privilege shape (`--cap-add
SYS_ADMIN --cap-add NET_ADMIN --security-opt apparmor=unconfined
--cgroupns=private`, no `--privileged`), with `--security-opt
seccomp=docker/seccomp-profile.json` in place of `unconfined`. Run repeatedly (5+
iterations per candidate) to rule out flakiness before treating a pass as signal.

## Regenerating / extending the profile

A future `runsc` release, a new packaged tool, or a new privileged-operation code
path in `internal/sandbox` may need an additional syscall. Two approaches, in order
of preference:

**1. Direct validation (the more reliable method).** Apply a
candidate profile via Docker's real enforcement and run the actual lifecycle
against it — don't guess, and don't trust `strace` here (see the gotcha below):

```sh
# From the repo root, with runsc on PATH and a container runtime available:
go test -tags integration -c -o /tmp/sandboxtest ./internal/sandbox/
docker run --rm \
  --cap-add SYS_ADMIN --cap-add NET_ADMIN \
  --security-opt apparmor=unconfined --security-opt seccomp=docker/seccomp-profile.json \
  --cgroupns=private \
  -v /tmp/sandboxtest:/work/sandboxtest:ro \
  -v "$(command -v runsc)":/usr/local/bin/runsc:ro \
  -w /tmp nicolaka/netshoot:latest \
  sh -c 'cp /work/sandboxtest /tmp/t && chmod +x /tmp/t && /tmp/t -test.v -test.timeout=180s'
```

(`nicolaka/netshoot` is a convenient off-the-shelf image that already bundles
`ip`/`iptables`/`chroot`/`sysctl` — swap in anything else with the same tools, e.g.
the `triagefactory` runtime image itself.)

If a test fails, the error is usually specific enough to point at the missing
syscall (a `mount`/`chroot`/`ptrace`-shaped "operation not permitted" from `ip`,
`iptables`, `chroot`, or `runsc`'s own stderr). Add the syscall to
`docker/seccomp-profile.json`'s `syscalls` array with a `comment` explaining why,
then re-run to confirm — including several repeats, since a nested/virtualized test
host can have its own unrelated startup flakiness (a `runsc` sentry that
occasionally fails with "cannot read client sync file: waiting for sandbox to
start: EOF" on a cold-started nested test container, unrelated to seccomp) that's
easy to misattribute to the profile if you only run once.

**Gotcha: don't wrap this in `strace -f`.** `runsc`'s systrap platform uses `ptrace`
internally to attach to its own stub processes. `strace -f` auto-attaches to every
forked child too, and Linux allows only one tracer per process — so `runsc`'s own
attach loses the race and the sandbox fails to start (the same "waiting for sandbox
to start: EOF" symptom, this time *caused* by the tracer, not by seccomp). This
reproduces regardless of the seccomp profile in effect, including under
`unconfined`, which is the tell that it's a tracer conflict, not a permissions
problem. Diagnose failures via the error output and (if needed) `runsc --debug
--debug-log=<path>`, not `strace -f` around the whole tree.

**2. Audit-mode capture**, for a broader/from-scratch resurvey (e.g., a `runsc`
platform change): apply a profile with `defaultAction: SCMP_ACT_LOG` (allow + log
every syscall) instead of `SCMP_ACT_ERRNO`, run the lifecycle, and collect the
`type=SECCOMP` entries from the kernel audit log (`ausearch -m SECCOMP`, or `dmesg`
when no `auditd` is listening). This is non-intrusive (no ptrace, no conflict with
`runsc`), but at real workload syscall volume the kernel's `printk` rate limiter can
silently drop the majority of entries — treat it as a rough survey to generate
hypotheses, then confirm each candidate syscall with approach 1 above.
