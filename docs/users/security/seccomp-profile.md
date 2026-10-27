# Seccomp profile

The `executor` service in `docker-compose.yml` runs with
`seccomp=docker/seccomp-profile.json`, a default-deny syscall allowlist, rather
than `seccomp=unconfined`.

## Why the executor needs its own profile

Docker's default seccomp profile blocks `pivot_root`, which gVisor (`runsc`,
systrap platform) needs to build each run's sandbox. The usual workaround is
`unconfined`, which turns off syscall filtering entirely. The gap is one
syscall, so the profile closes only that gap.

## What it allows

`docker/seccomp-profile.json` is
[Docker's default profile](https://github.com/moby/moby/blob/master/profiles/seccomp/default.json)
with two changes:

1. **Removed**, because the executor never uses them and they are dangerous in
   a compromised process: `reboot`, `init_module`, `delete_module`, `iopl`,
   `ioperm`, `acct`, `quotactl`, `quotactl_fd`, `bpf`, `perf_event_open`,
   `fanotify_init`, `fanotify_mark`, `landlock_create_ruleset`,
   `landlock_add_rule`, `landlock_restrict_self`, `lookup_dcookie`, `syslog`,
   `vhangup`.
2. **Added:** `pivot_root`, in the same rule group as `mount`, `unshare` and
   `setns`, so it is allowed only to a process holding `CAP_SYS_ADMIN`.

That leaves about 400 allowed syscalls, against Docker's default of about 415.
The main boundary is still the capabilities the container holds; the profile
narrows what a compromised process in the container can call.

## Where it applies

- **The `executor` service only.** The `control` service launches no sandbox,
  holds no added capabilities, and runs under Docker's default profile.
- **Self-hosted Docker Compose only.** Fly.io deployments (`fly.toml`) run
  unconfined inside each Fly Machine, because the Machine config has no
  equivalent setting. Each Fly Machine is its own Firecracker microVM, so it
  does not share a kernel with other tenants.
- **The host kernel's view of the container.** This profile filters the TF
  binary, `ip`, `iptables`, `runsc` and everything `runsc` starts. It is
  separate from the seccomp list in the sandboxed agent's own OCI spec, which
  gVisor does not enforce.

## Checking it

On a running executor:

```sh
docker compose exec executor grep Seccomp /proc/1/status
```

`Seccomp: 2` means a filter is in force. `0` means the container is running
unconfined.

## If the executor fails to start with an unknown syscall name

The profile is copied from a recent Docker release and names newer syscalls
(`cachestat`, `mount_setattr`, the new mount API such as `move_mount`,
`fsopen` and `open_tree`). An older Docker Engine or libseccomp that does not
recognize one of those names refuses to start the whole container, with an
error naming the syscall. Upgrade Docker Engine. If you cannot, delete the
unrecognized names from the rule's `names` array in
`docker/seccomp-profile.json`; a host that old almost certainly predates the
syscalls themselves. Changing the profile is covered in
[Regenerating the seccomp profile](../../contributors/seccomp-profile.md).
