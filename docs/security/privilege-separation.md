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
| **orchestrator** (`tf-orchestrator`) | **No** — empty effective set, capability bounding set cleared | Its own control-plane work only — webhook/poll/DB credentials on the control role; **never a per-run agent credential** (those live only in the run's sidecar), and on a `TF_ROLE=executor` process, none at all (the secret store is disabled there — and multi mode has no fused role that would hold the store on a sandboxing host) | Its own control-plane inputs — webhook payloads, the HTTP API; **not** the per-run agent's output, which only the sidecar sees. Hosts the capless `RelayServer` that answers the sidecar's narrow, validated policy/DB/audit relays — no credential-bearing op | Yes — the public HTTP API / websocket |
| **credential sidecar** (`tf-sidecar`, one per live run) | **No** — the setuid-from-root exec clears its effective/permitted capabilities | Yes — exactly **one** run's material, from the sealed per-run bundle (LLM key, GitHub/Jira/provider tokens), unsealed with a key the process mints for itself and that never leaves it | Yes — **that one run's** agent traffic, in both directions: the exec-verb socket the agent dials, the upstream API responses its proxies parse, and — at the `gh` injector only — the body of an outbound GraphQL request, a document the agent composed. The outbound read exists because a policy keyed on what a write *does* cannot be applied to a request whose act is stated only in its body. It is narrow by construction: one endpoint, a size cap, a grammar that collects top-level field names and skips everything beneath them uninterpreted, parse-fail refuses the request, and nothing forwarded is altered — see `internal/ghinjector` and `internal/ghwrite` | The per-run agenthost unix socket + that run's LLM/git/API proxies, bound on the run's own veth IP — reachable by the run's sandbox and the orchestrator, never off-box |
| **sandbox** (the delegated agent) | No | No | Yes — it's the source of the hostile input | No — outbound only, through the run's egress proxy |

The **credential sidecar** is not a boot process like the other two: the broker
spawns one per delegated run, at a per-run uid derived from the run's subnet
index (the reserved `SidecarUID` band, `20000+`), and it dies when the run ends.
That per-run uid is the isolation boundary between concurrent runs' credentials:
two sidecars at different uids cannot `ptrace` or read one another's memory even
though they share the host's process table, so a compromise of one run's
credential process reaches that run's material and no other's. When the run
ends the sidecar's process exits and its whole credential-bearing address space
goes with it — the eviction guarantee (`internal/sandbox`'s eviction proof reads
the per-run uid band out of `/proc` to confirm it vacates). This is why a run's
agent credentials and the parsing of that agent's own output live here rather
than in the orchestrator: no single process holds both a run's credentials and
the capabilities that build its cell.

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
path pointing at `/etc` is refused by ownership, not just by shape. A third
launch family is the per-run credential sidecar: the broker execs it at the
validated per-run uid (re-checking the `SidecarUID` band at the RPC boundary, so
a compromised orchestrator can't ask for uid 0), the sidecar analog of the runsc
launch.

That sidecar launch carries one more privileged grant: the broker binds the run's
**shared-origin listener** — port 443 on the run's own host-side veth IP — and
passes the descriptor down as the sidecar's one extra fd. That listener is the
run's fake-GHE origin, serving the GitHub API and git smart-HTTP on one address so
the agent's `gh` can resolve a repository from its worktree's remote (`gh` drops
the port when matching a remote against `GH_HOST`, so the origin has to be on the
https default, and 443 is privileged). The sidecar is capless and cannot bind it.
This does **not** put the broker in the traffic path: it binds, hands over its only
copy, and never accepts a connection or reads a byte through it — the same kind of
grant as creating the run's veth. The address is derived from the launch's subnet
index, never named by the caller, and `ValidateSidecarLaunchParams` requires that
index to be the one the sidecar's uid was derived from, so a compromised
orchestrator cannot occupy one run's sidecar slot while pointing the bind at a
sibling run's veth. A bind that fails costs the run `gh`'s repo inference and
nothing else — the sidecar falls back to an ephemeral port of its own.

**Observation is not in the broker's monopoly.** The broker owns cgroup
*lifecycle and mutation* — creating a run's group, setting its `memory.max`,
destroying it — because each of those needs `CAP_SYS_ADMIN` on a delegated
cgroup tree. Reading a group's stat files needs nothing: cgroup v2 exposes
`memory.current` / `cpu.stat` / `memory.events` world-readable, so the
zero-capability orchestrator reads them directly (that is how the resource
sampler records a live run's usage series) and no RPC is involved. The
exposure that buys is coarse resource observability to any host-local process,
which leaks no data, credentials, or content and is the same posture
`/proc/stat` and `/proc/meminfo` already have; the jailed agent cannot see the
host cgroup filesystem at all through gVisor. If cross-tenant resource side
channels ever enter the threat model, the lever is a `0750` per-run group
directory with a group grant at creation — noted so the option is
discoverable, deliberately not built.

The per-run agenthost socket is granted to the sandbox with no capability at all:
it is *chgrp'd* (not chowned) to the sandbox group — an owner-legal group grant,
possible because the granting process is a member of `tf-sandbox` (gid 10000).
That process is now the run's **sidecar**, which the broker launches carrying
that one supplementary group and which creates and serves the socket itself; on
the self-contained all/local path, where there is no sidecar, the orchestrator
does the same chgrp in-process (the entrypoint's `setpriv` carries the group
through its drop).

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
  and `tf-orchestrator` — not two processes both named `triagefactory` — plus one
  `tf-sidecar` per delegated run currently in flight (none when the box is idle).
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
  different-uid process without it. The same read on a live `tf-sidecar` shows a
  `CapEff` of all zeroes at a distinct uid in the `20000+` band — and each run's
  sidecar sits at a *different* uid in that band, which is exactly what stops one
  run's compromised credential process from `ptrace`-ing another's.

All three are identifiable in the logs: every line carries a `proc=cap-broker`,
`proc=orchestrator`, or `proc=sidecar` attribute (distinct from `component`,
which names the subsystem within a process), and each process logs exactly one boot
line reporting its own uid and effective capability set, e.g.:

```
level=INFO msg=boot component=cap-broker uid=0 CapEff=cap_net_admin,cap_sys_admin socket=/run/tf/cap-broker.sock proc=cap-broker
level=INFO msg=boot component=boot uid=10001 CapEff=(empty) proc=orchestrator
level=INFO msg=boot component=sidecar uid=20007 CapEff=(empty) container_id=tf-<run>-7-sc proc=sidecar
```

A `CapEff=(empty)` boot line is the confirmation the drop applied — for the
orchestrator and for every per-run sidecar; anything else there is a bug, not a
variant configuration. (The sidecar line appears once per run, at that run's
start, not at container boot.)

## `$HOME`

`setpriv` switches uid/gid/capabilities but never touches environment variables, so
the entrypoint explicitly sets `HOME` to the orchestrator's own directory
(`/home/tf-orchestrator` by default, owned by uid `10001`) right before the exec —
the container's inherited `HOME=/root` would otherwise persist and become
unreadable/unwritable once the process is no longer root. This matters beyond the
obvious: a few paths (skills import, the Claude Code SDK's own
`~/.claude/projects` session-cache directory for a direct, unsandboxed run)
deliberately resolve state from the real `$HOME`
even in multi mode (see `internal/paths/paths.go`), so getting this wrong breaks
those, not just an edge case.

## No rollback flag

The cap-broker is the only sandbox launch path in multi mode — it is spawned
unconditionally, with no switch to disable it and no in-process fallback. If the
broker can't start, boot fails with a clear error rather than silently running the
sandbox from a less-isolated, fully-privileged process. (Local mode never
sandboxes, so it never spawns a broker and is unaffected.)
