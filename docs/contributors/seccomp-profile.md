# Regenerating the seccomp profile

`docker/seccomp-profile.json` is the executor container's syscall allowlist
(what it is and why: [Seccomp profile](../users/security/seccomp-profile.md)).
A new `runsc` release, a new tool in the rootfs, or a new privileged operation
in `internal/sandbox` may need another syscall. Find it by running the real
sandbox lifecycle under the candidate profile. Don't guess, and don't use
`strace -f` (see below).

## Validate a candidate profile

The integration suite in `internal/sandbox` (the one
`scripts/test-sandbox-linux.sh` runs) covers sandbox spawn, agent execution
with real egress through the network namespace and egress allowlist, teardown,
orphan reaping, the rootfs toolchain bake, and the agent-host socket. Build it
and run it in a container with the executor's privilege shape and the profile
under test:

```sh
# From the repo root, with runsc on PATH and a container runtime available:
go test -tags integration -c -o /tmp/sandboxtest ./internal/sandbox/
docker run --rm \
  --cap-add SYS_ADMIN --cap-add NET_ADMIN --cap-add SYS_RESOURCE \
  --security-opt apparmor=unconfined --security-opt seccomp=docker/seccomp-profile.json \
  --cgroupns=private \
  -v /tmp/sandboxtest:/work/sandboxtest:ro \
  -v "$(command -v runsc)":/usr/local/bin/runsc:ro \
  -w /tmp nicolaka/netshoot:latest \
  sh -c 'cp /work/sandboxtest /tmp/t && chmod +x /tmp/t && /tmp/t -test.v -test.timeout=180s'
```

`nicolaka/netshoot` is used because it already has `ip`, `iptables`, `chroot`
and `sysctl`; any image with those tools works, including the `triagefactory`
image. Use the `runsc` release pinned in `docker/Dockerfile`.

A failure usually names the missing syscall as an "operation not permitted"
from `ip`, `iptables`, `chroot` or `runsc`. Add the syscall to the profile's
`syscalls` array with a `comment` saying why, and re-run. Run each candidate at
least five times: a nested or virtualized test host can fail `runsc` startup
with `cannot read client sync file: waiting for sandbox to start: EOF` for
reasons unrelated to seccomp.

## Don't wrap the run in `strace -f`

`runsc`'s systrap platform attaches to its own stub processes with `ptrace`,
and a process can have only one tracer. Under `strace -f`, `runsc` loses that
race and the sandbox fails to start with the same `waiting for sandbox to
start: EOF`, under any profile including `unconfined`. Diagnose with the
error output and `runsc --debug --debug-log=<path>` instead.

## Surveying from scratch

For a broad resurvey (for example a `runsc` platform change), set the
profile's `defaultAction` to `SCMP_ACT_LOG` instead of `SCMP_ACT_ERRNO`, run
the lifecycle, and collect the `type=SECCOMP` entries from the audit log
(`ausearch -m SECCOMP`, or `dmesg` without `auditd`). The kernel rate-limits
these messages and drops most of them under real load, so use the result to
pick candidates, then confirm each one with the validation above.
