# Privilege separation

The `executor` container is granted `CAP_SYS_ADMIN`, `CAP_NET_ADMIN` and
`CAP_SYS_RESOURCE` (`cap_add` in `docker-compose.yml`) so it can build each
run's sandbox. That grant is a ceiling on the container, not on every process
in it: `docker/entrypoint.sh` splits the container into separate processes at
boot, and only one of them holds the capabilities. This page lists the
processes and shows how to check the split on a running executor. Why it is
built this way is in [Security overview §4](security-overview.md).

The split happens only with `TF_MODE=multi`. In local mode (the image's
default) the entrypoint skips it: no uid switch, no capability drop, no
`$HOME` change.

## The processes

| Process | Capabilities | Uid | Credentials | Listens on |
| --- | --- | --- | --- | --- |
| `tf-cap-broker` | `cap_net_admin`, `cap_sys_admin`, `cap_sys_resource` | `0` | None | `/run/tf/cap-broker.sock` (mode `0600`), reachable only by the orchestrator |
| `tf-orchestrator` | None; bounding set cleared | `10001` (`TF_ORCHESTRATOR_UID`) | Control pod: its control-plane credentials. Executor: its database and object-store connections only; the secret store is disabled there | Control: the HTTP API. Executor: a loopback health port |
| `tf-sidecar`, one per run | None | Per run, `20000+` | One run's credentials, from the sealed per-run bundle | That run's `exec`-verb socket and proxies, on the run's own veth address |
| The sandbox (agent) | None | Per run, `40000+` | None | Nothing; outbound only, through its sidecar |

The broker starts a run's sidecar at a uid derived from the run's subnet index,
and the sandbox at a uid derived from the same index. Two runs never share
either uid, so one run's sidecar cannot trace or read another's memory. A
run's tree, its socket directories under `/run/tf` and the staging it mounts
are granted to that run's sandbox identity. When the run ends, the broker
takes each grant back before the subnet index is reused; an index whose grants
cannot be taken back is held out of the pool.

What the broker does over its socket, all of it behind validation that refuses
anything outside a run's own scope:

- builds and destroys the run's network namespace, veth, firewall rules,
  cgroup and gVisor runtime;
- hands the run tree to the run's identity at each claim, and parks it when
  the run ends;
- starts the run's sidecar at its per-run uid, refusing any uid outside the
  sidecar band;
- binds port 443 on the run's veth for the sidecar and hands over the socket.
  The broker never accepts a connection on it.

The broker does not restart itself. If it dies, sandbox operations fail with a
dial error until the container restarts. There is no switch to run sandboxes
without it: if it cannot start, the executor refuses to boot.

Any host-local process can read each run's cgroup statistics (memory, CPU),
the same exposure `/proc/meminfo` has. The sandboxed agent cannot see the host
cgroup filesystem.

## Snapshots

A workspace snapshot runs git over a tree the agent wrote, and git honors
configuration in that tree (filter drivers, alternates, submodule config).
So every snapshot capture runs in a one-shot gVisor jail: no network, no
capabilities, the run tree mounted read-only, running as the identity that
owns the tree. If that jail cannot start, the snapshot fails; there is no
fallback that runs git on the host. A restore imports the snapshot with
`git index-pack --fsck-objects`. Local mode captures in-process.

## Verifying it on a running executor

- `docker compose exec executor ps aux` shows `tf-cap-broker` and
  `tf-orchestrator`, plus one `tf-sidecar` per run in flight.
- `grep ^Cap /proc/<pid>/status`: the broker's `CapEff` decodes (with
  `capsh --decode=<hex>`) to `cap_net_admin,cap_sys_admin,cap_sys_resource`.
  The orchestrator's `CapEff` and `CapBnd` are all zeroes, so it cannot regain
  a capability even through a setuid-root binary. A `tf-sidecar` has an
  all-zero `CapEff`.
- `grep ^Uid /proc/<pid>/status`: the broker is `0`, the orchestrator
  `10001`, and each `tf-sidecar` a different uid in the `20000+` band.
- `grep ^Groups /proc/<pid>/status` is empty for the orchestrator and every
  `tf-sidecar`.
- `stat -c '%u:%g %a %n' /run/tf /run/tf/*`: `/run/tf` is the orchestrator's
  at `711`; each run's directories are `2750` with that run's sandbox gid
  (`40000+`) as their group.

Each process logs one boot line with its uid and effective capabilities:

```
level=INFO msg=boot component=cap-broker uid=0 CapEff=cap_net_admin,cap_sys_admin,cap_sys_resource socket=/run/tf/cap-broker.sock proc=cap-broker
level=INFO msg=boot component=boot uid=10001 CapEff=(empty) proc=orchestrator
level=INFO msg=boot component=sidecar uid=20007 CapEff=(empty) container_id=tf-<run>-7-sc proc=sidecar
```

`CapEff=(empty)` on the orchestrator and on every sidecar confirms the drop.
Anything else there is a bug. The sidecar line appears at each run's start.
Every log line carries `proc=` naming the process it came from.

The entrypoint sets `HOME=/home/tf-orchestrator` (`TF_ORCHESTRATOR_HOME`)
for the orchestrator. A custom entrypoint has to do the same, or the
orchestrator inherits `/root`, which it can no longer read.
