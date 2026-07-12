# Privilege separation: process model

This is the operator-facing view — the process split as it runs, and how to verify
it against a live deployment. For *why* it's built this way (the trust-domain
decomposition and what a compromise of each component yields), see
[security-overview.md §4](security-overview.md).

The `triagefactory` container grants `CAP_SYS_ADMIN` + `CAP_NET_ADMIN` (see
`docker-compose.yml`'s `cap_add` comment) because the per-run gVisor sandbox needs
them to set up netns/veth/iptables, cgroups, and the curated rootfs. That grant is
a *ceiling* on the container, not a promise that the whole TF process wields it:
`docker/entrypoint.sh` splits the container into two processes at boot, and only
one of them ever touches those capabilities.

This split applies **only when `TF_MODE=multi`** — the same condition that decides
whether this host sandboxes runs at all. Local mode (`TF_MODE=local`, the image's
own default — the point of `docker-compose.yml`'s "a `docker run` without any env
vars boots into a working single-tenant binary") has no sandbox and no broker to
protect anything from, so the entrypoint skips the split outright there: no uid
switch, no capability drop, no `$HOME` change. A bare `docker run` of the published
image is completely unaffected by this section.

| Process | Holds capabilities | Holds credentials | Parses hostile input | Listens for connections |
| --- | --- | --- | --- | --- |
| **cap-broker** (`tf-cap-broker`) | **Yes** — `CAP_SYS_ADMIN`, `CAP_NET_ADMIN` | No | No | Yes — but only a host-only unix socket (`/run/tf/cap-broker.sock`, mode 0600), reachable only by the orchestrator |
| **orchestrator** (`tf-orchestrator`) | **No** — empty effective set, capability bounding set cleared | Yes — GitHub/Jira tokens, the GitHub App key, DB credentials | Yes — webhook payloads, agent output, the HTTP API | Yes — the public HTTP API / websocket |
| **sandbox** (the delegated agent) | No | No | Yes — it's the source of the hostile input | No — outbound only, through the orchestrator's egress proxy |

The broker serves two families of operations over that socket, both behind
boundary validation (`internal/sandbox`'s `ValidateLaunchParams` and
`validateRunTreeRoot`) so a compromised orchestrator cannot steer them at arbitrary
host state: the sandbox infrastructure itself (netns/veth/iptables, cgroups, the
curated rootfs, the `runsc` launch), and the **run-tree ownership lifecycle** —
handing a freshly-cloned run tree to the sandbox identity (uid 10000) at run start,
running the park-time git-delta capture in a dropped-privilege child, and
destroying the tree at teardown. The lifecycle family exists because all three are
privileged once the orchestrator's capabilities are gone: changing a file's owner
needs `CAP_CHOWN`, the capture child's setuid/netns need `CAP_SETUID`/`CAP_SYS_ADMIN`,
and removing a tree the sandbox wrote into means unlinking through modes the
orchestrator can't. The chown/remove ops additionally require the target tree to
already be owned by the orchestrator or the sandbox identity — a validly-shaped
path pointing at `/etc` is refused by ownership, not just by shape. One deliberate
exception stays orchestrator-side with no capability at all: the per-run agenthost
socket is *chgrp'd* (not chowned) to the sandbox group — an owner-legal group
grant, possible because the image makes `tf-orchestrator` a member of `tf-sandbox`
(gid 10000) and the entrypoint's `setpriv` carries exactly that one supplementary
group through the drop.

**Broker lifetime.** After the entrypoint's `exec`, the broker is a child of the
orchestrator, but the orchestrator neither supervises nor restarts it (post-drop it
lacks the privilege to manage a root process anyway) — if the broker ever dies,
subsequent sandbox operations fail with a clear dial error and the fix is a
container restart; a crashed broker may also linger as one `<defunct>` zombie entry
in `ps` until then, which is cosmetic. Shutdown needs no supervision either way:
tini's `-g` signal fan-out reaches both processes directly.

## Verifying it against a running deployment

- `docker compose exec triagefactory ps aux` (or `ps -ef` inside the container's
  PID namespace) shows two `triagefactory`-derived processes named `tf-cap-broker`
  and `tf-orchestrator` — not two processes both named `triagefactory`.
- `cat /proc/<cap-broker pid>/status | grep ^Cap` and the same for the
  orchestrator's pid: the broker's `CapEff` decodes to `cap_sys_admin,cap_net_admin`
  (via `capsh --decode=<hex>` or by eye against
  `include/uapi/linux/capability.h`'s bit numbers); the orchestrator's `CapEff` is
  all zeroes, and — the stronger property — its `CapBnd` (bounding set) is all
  zeroes too, meaning it cannot regain a capability even by executing a setuid-root
  helper later.
- `cat /proc/<cap-broker pid>/status | grep ^Uid` vs. the orchestrator's:
  different real uids (broker is `0`, orchestrator is `10001` by default — see
  `TF_ORCHESTRATOR_UID` in `.env.example`). Different uids is what makes the
  orchestrator's zero capabilities meaningful: even a `CAP_SYS_PTRACE`-holding
  attacker (which the orchestrator isn't, but hypothetically) can't `ptrace` a
  different-uid process without it.

Both processes are also identifiable in the logs: every line carries a
`proc=cap-broker` or `proc=orchestrator` attribute (distinct from `component`,
which names the subsystem within a process), and each process logs exactly one boot
line reporting its own uid and effective capability set, e.g.:

```
level=INFO msg=boot component=cap-broker uid=0 CapEff=cap_net_admin,cap_sys_admin socket=/run/tf/cap-broker.sock proc=cap-broker
level=INFO msg=boot component=boot uid=10001 CapEff=(empty) proc=orchestrator
```

A `CapEff=(empty)` orchestrator boot line is the confirmation the drop applied;
anything else there is a bug, not a variant configuration.

## `$HOME`

`setpriv` switches uid/gid/capabilities but never touches environment variables, so
the entrypoint explicitly sets `HOME` to the orchestrator's own directory
(`/home/tf-orchestrator` by default, owned by uid `10001`) right before the exec —
the container's inherited `HOME=/root` would otherwise persist and become
unreadable/unwritable once the process is no longer root. This matters beyond the
obvious: a few paths (curator session state, skills import, project-bundle
export/import) deliberately resolve Claude Code SDK session state from the real
`$HOME` even in multi mode (see `internal/paths.go`), so getting this wrong breaks
those, not just an edge case.

## No rollback flag

The cap-broker is the only sandbox launch path in multi mode — it is spawned
unconditionally, with no switch to disable it and no in-process fallback. If the
broker can't start, boot fails with a clear error rather than silently running the
sandbox from a less-isolated, fully-privileged process. (Local mode never
sandboxes, so it never spawns a broker and is unaffected.)

## Upgrading an existing deployment

The orchestrator now runs as uid `10001` instead of root. Files created under the
`tf-data` (`/data`) and `tf-rootfs` (`/opt/triagefactory/sandbox`) volumes by an
earlier root-running orchestrator stay root-owned — the entrypoint only `chown`s the
top-level mount points, not existing content recursively, to keep every boot fast.
If the orchestrator logs permission errors reading or writing under `/data` after
upgrading, run a one-time recursive fix from the host:

```sh
docker compose run --rm --user root triagefactory chown -R 10001:10001 /data /opt/triagefactory/sandbox
```

A fresh deployment (empty volumes) needs no such step.

One more upgrade wrinkle, only if you persisted the container's `/root` across
upgrades (no stock volume does): Claude Code SDK session state an earlier
deployment wrote under `/root/.claude` (curator session resume, primarily) is
orphaned once `$HOME` moves to `/home/tf-orchestrator` — the orchestrator won't
find it, and parked curator sessions from before the upgrade rehydrate from their
snapshots instead of resuming warm. If keeping warm resume matters, copy
`/root/.claude` into `/home/tf-orchestrator/.claude` (and `chown -R 10001:10001`
it) before the first post-upgrade boot; otherwise nothing needs doing — the state
regenerates.
