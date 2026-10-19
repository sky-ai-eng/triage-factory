# SPACE spike: results

Measurements of what a SPACE (Firecracker microVM) cell backend would cost
Triage Factory, taken on one KVM host before the design settled on driving
Firecracker directly (README §1.1). They answer the open questions the SPACE
mapping carried and the six spike items that went with it. The spike harness
is not committed: it is a SPACE API client, and the design takes none of
SPACE's services.

## Summary

| Question | Answer |
| --- | --- |
| Can the guest reach a sidecar on the executor? | No. SPACE's per-sandbox nftables drop new guest connections to 10/8, 172.16/12 and 192.168/16 before its proxy sees them, whatever the tenant policy says. A guest-local port forwarded over a TCP port session or a process-session stream does reach the host |
| Can a policy admit only `addr:X`? | No. Egress rules name an IP, CIDR or domain, never a port, so admitting an address admits every port on it |
| Data-plane throughput | 54.8 MB/s over a TCP port session, capped by a hard-coded 50 MiB/s per-sandbox NIC limiter; 14.3 MB/s over process-session stdio (9.9 MB/s through a CONNECT proxy); host loopback ceiling 3,354 MB/s |
| Run-tree hand-off | tar over stdin: 89 MB in 1.4–3.2 s, 525 MB in 7.0–7.6 s; out: 0.41–0.46 s and 2.3–3.6 s. Lossless both ways. The file API loses modes, symlinks and executable bits and takes 24 s and 332 s |
| Delta resync | 10 changed files and 1 deletion in a 48,744-entry tree: 0.15–0.16 s as a tar of changed paths, 0.66–0.84 s with rsync over the stream |
| Warm resume keeps the disk? | Yes, on the same node and on another node |
| Tool-call round trip (budget p50 ≤ 5 ms, p95 ≤ 25 ms) | **Pass.** TCP port session p50 0.34–0.69 ms, p95 1.2–1.6 ms; process-session stdio p50 2.2–4.2 ms, p95 7.3–7.7 ms; native unix socket p50 0.010–0.045 ms |
| Pause and suspend with state inside | **Pass.** Pause 110 ms, wake 109–119 ms, nothing runs while paused; suspend 2.4–2.5 s, full snapshot 1.14 GB for a 1 GiB guest, wake 0.94 s on the same node and 2.04 s on another; rows, processes and the tool host intact |
| Does SPACE report peak memory? | No. Its stats do not measure the guest's workload at all, and an OOM kill surfaces only as `exit_code 137, status "signaled"` |
| Idle memory per sandbox | About 104 MB PSS (119 MB host used) per idle 512 MB sandbox, against gVisor's ~24 MB per idle jail. Guest memory is not reserved up front |
| Bring-up latency | p50 1,299 ms, p95 1,596 ms from create to the tool host's first answer, against gVisor's ~170–290 ms spawn |
| Build burst | `go test`: same wall time as native, 1.33× the host CPU-seconds. Frontend build: same wall time and CPU |
| Can a create call target a specific node? | Not through the public API. The operator-only `POST /v1/sandboxes:admin-create` takes a `node_pool`, behind `SPACE_ALLOW_SANDBOX_PLACEMENT`, on the Kubernetes substrate only; the pin is not persisted, so a sandbox can wake on another node |

## Machine and versions

| | |
| --- | --- |
| CPU | Intel Core i9-14900KF, 24 cores (8 performance, 16 efficiency), 32 threads, VT-x |
| Memory | 64 GB (61 GiB usable) |
| Host kernel | 7.0.0-38-generic, Ubuntu 26.04.1 LTS, cgroup v2 |
| Disk | NVMe SSD, ext4 |
| SPACE | commit `57c7c0c47a73373a974c5b542d180599795465da`, the local Kind deployment from its installation guide |
| Firecracker | v1.15.1 (SPACE's vendor installer) |
| Guest kernel | 6.1.155 (`vmlinux-6.1.155-bpf-modsig`, SPACE's seed lineage) |
| Guest image | SPACE's `seed` (Ubuntu 26.04 minimal), plus the tool host and socat as described below |

The host was a working desktop, not a dedicated machine. The Kind deployment
is six nodes: a control plane, two sandbox workers, an infra node and two
builders, each a privileged Docker container. Bring-up took 8 m 47 s from a
cold checkout and needed no root beyond Docker: KVM was readable through an
ACL, and the apt packages SPACE wants were already installed.

### Two things on this host that the measurements work around

**No guest egress.** Every guest's outbound connection was dropped as a
martian source by the guest's sandbox namespace. The host sets
`net.ipv4.conf.all.src_valid_mark=1`, and new network namespaces inherit IPv4 settings from the host
(`net.core.devconf_inherit_init_net=0`). SPACE sets `rp_filter=0` in each
sandbox namespace but leaves `src_valid_mark` alone. With policy-routing
rules and a custom local route present (SPACE's TPROXY `fwmark 0x1 → table
100`), this kernel validates the source of a marked packet against table
100, where the guest's address resolves as local, and drops it: the kernel
logged each guest SYN as a martian source. Setting `src_valid_mark=0` in
one sandbox's namespace restored DNS and HTTPS from that guest at once. The measurements below do not depend on guest egress:
every item reaches the guest over SPACE's own streams. It does mean the
template builder could not run `apt`, so:

**The guest image is built from copies, not packages.** A template built
from `seed` carries the tool host and socat as inline copy steps. An inline
payload of 1.4 MB passed the API's body limit and then broke the transport
between node-api and the build manager (`h2 protocol error`); 1.0 MB worked,
so the tool host went in as two chunks across two chained builds. rsync,
git, the Go toolchain and Node went into individual sandboxes as tar streams
(same distribution as the host, so host binaries ran unchanged).

## 1. Can the guest reach the sidecar, and only the sidecar?

**Method.** Three host listeners: X and Y on the host's address on the Kind
bridge, Z on `0.0.0.0`. A sandbox created with
`{"egress": {"default_action": "deny", "allow": [{"ip": <bridge address>}]}}`
probed each with a TCP connect from inside the guest, plus cloud metadata, a
public IP, a second sandbox's pod address and its own node's node-api port.
A Kind node container reaching all three listeners was the positive control.

**Raw.**

| Target | Result |
| --- | --- |
| X, sidecar address (admitted) | timeout |
| Y, same address, other port | timeout |
| Z via the bridge, the LAN address and docker0 | timeout |
| `169.254.169.254:80` | timeout |
| public IP on 443 | timeout (not admitted) |
| another sandbox's pod, its own node's node-api | timeout |
| host bridge over IPv6, public IPv6 | `ENETUNREACH` (guest has link-local IPv6 only) |

The guest's address is link-local (`169.254.0.21/30`, gateway `.22`) in every
sandbox. The sandbox namespace's nftables, from the node:

```
chain pre_tproxy_guardrails {
    type filter hook prerouting priority -175; policy accept;
    ...
    iifname "tap0" ct state new ip daddr 10.0.0.0/8 drop comment "platform-egress-pre-tproxy-guardrail-rfc1918-10"
    iifname "tap0" ct state new ip daddr 172.16.0.0/12 drop comment "platform-egress-pre-tproxy-guardrail-rfc1918-172"
    iifname "tap0" ct state new ip daddr 192.168.0.0/16 drop comment "platform-egress-pre-tproxy-guardrail-rfc1918-192"
}
```

These run before the TPROXY rule that hands traffic to SPACE's egress
gateway, so tenant policy is never consulted for a private destination. The
gateway's own guardrail list (`PLATFORM_EGRESS_HARD_DENY_CIDRS`: metadata,
link-local, SPACE's slot ranges and loopback) does not include RFC 1918; the
nftables chain does. The Kind deployment adds a Kubernetes NetworkPolicy
beneath both that also excludes private and CGNAT ranges. The gateway's
counters (`tcp_total_connections`, `tcp_denied_connections`) stayed at 0
throughout, so they cannot tell a denial from an absence of traffic.

**Fallback.** A guest-local port forwarded to a host address works over
either stream: socat in the guest listens on `127.0.0.1:N` and carries the
connection over a TCP port session (or a process session's stdio) to the
executor, which splices it to the target. Item 2 measures it. It carries
one connection per forward; a backend would multiplex.

Two port-session details a backend has to handle. SPACE's guest agent binds
the guest address on the session's port and forwards to `127.0.0.1:N`, so
the guest service listens on loopback only. And the exposure applies
asynchronously: attaching before `GET /v1/sandboxes/{id}/ports` reports
`apply_state: applied` yields an `attached` frame and then an immediate
close.

**Verdict.** The guardrail denies private ranges regardless of policy, so
the sidecar cannot sit on an executor address. Separately, the policy cannot
admit a single port. The stream fallback works.

## 2. Data-plane throughput

**Method.** A 1 GiB random file served over HTTP on host loopback, and a
plain HTTP CONNECT proxy on host loopback. `curl` in the guest downloads it
through the item 1 forwarder, directly and through the proxy. The native
ceiling is the same download on the host.

**Raw** (1,073,741,824 bytes each, complete):

| Path | Direct | Through the CONNECT proxy |
| --- | --- | --- |
| Host loopback (ceiling) | 3,354 MB/s | 2,960 MB/s |
| TCP port session | 54.8 MB/s (19.6 s) | 54.8 MB/s (19.6 s) |
| Process-session stdio | 14.3 MB/s (75.0 s) | 9.9 MB/s (108.8 s) |

The port-session figure is SPACE's network rate limiter, not its protocol:
every sandbox's NIC is limited to 52,428,800 bytes and 10,000 packets per
second in each direction (`NET_RATE_LIMITER`, a constant in the sandbox
manager). Process-session stdin turns every client `input` frame into one
unary gRPC call, so its throughput follows frame size: frames as a socket
read returns them (~64 KiB) give 14 MB/s.

**Verdict.** About 55 MB/s per sandbox through the best path, fixed by a
constant TF cannot change through the API.

## 3. Run-tree hand-off

**Trees.** Small: a fresh clone of this repository, 90 MB, 3,714 entries, no
symlinks, 32 executables. Large: the same after `pnpm install` in
`frontend/`, 543 MB on disk (525 MB as tar), 48,744 entries, 1,422 symlinks,
199 executables. The large tree is about half the suggested 1 GB.

**Raw.**

| Method | Small | Large |
| --- | --- | --- |
| tar into `tar -x` on process-session stdin, 2 MiB frames, 3 runs | 1.44–3.18 s (28–62 MB/s) | 6.97–7.58 s (69–75 MB/s) |
| tar out of process-session stdout, 3 runs | 0.41–0.46 s (193–219 MB/s) | 2.26–3.56 s (147–232 MB/s) |
| File API, one `files:write` per file | 23.6 s (3.7 MB/s, 3,440 files) | 332 s (1.4 MB/s, 43,172 files) |

Copy-in throughput depends on frame size, because each frame is one unary
call downstream: 64 KiB frames gave 12.6–16.3 MB/s on the small tree, 1 MiB
gave 52–57 MB/s, 2 MiB gave 66–87 MB/s, and 3 MiB was no better than 2 MiB.

**Verification.** sha256 of every file, and type, mode and symlink target of
every entry, match the source after copy-in and after copy-out, for both
trees. Files are owned by the guest user, uid/gid 2000:2000, after
extraction; extracted on the host they belong to the extracting user. The
file API matches on content but writes every file `0644` and every directory
`0755`: all executable bits are lost, read-only files become writable, and
it has no symlink call, so the large tree's 1,422 symlinks cannot be
created. A single write is capped at 16 MiB, so larger files go as appends.

**Delta.** 10 files modified and 1 deleted in the large tree:

| Direction and method | Wall | Transferred |
| --- | --- | --- |
| Guest → host, tar of `find -newer` paths plus a deletion list | 0.16 s | 100 KB |
| Guest → host, rsync over the stream | 0.66 s | 1.25 MB (mostly the file list) |
| Host → guest, tar of the changed paths plus a guest `rm` | 0.15 s | 100 KB |
| Host → guest, rsync over the stream | 0.84 s | 1.33 MB |

All four results match the source tree exactly.

**Warm resume.** A file written before a suspend was present after waking on
the same node and after waking on a different node (item 5), so a warm
resume needs no sync.

**Verdict.** A tar stream through a process session is the hand-off; the
file API is not usable for a run tree.

Two data-plane behaviours the hand-off code has to respect. Input sent
before the `process_started` frame is dropped (`process_not_started`), so the
client waits for it. And a `close_stdin` that arrives after the process has
exited fails the data-plane bridge and loses the `process_exited` frame;
`tar -x` stops reading at its end-of-archive marker and can exit first, so
the guest command drains stdin after tar.

## 4. Tool-call round trip

**Method.** `tf-harness-tools` built from `harness/` (glibc; the guest's
distribution matches the host's) at `/opt/tf/bin`, with socat. The tool host
only dials, so socat in the guest listens on a unix socket and bridges it to
the session's stdio (`socat UNIX-LISTEN:… STDIO`) or to a loopback TCP port
reached by a TCP port session (`socat UNIX-LISTEN:… TCP-LISTEN:…`). On the
host, a Go program in this module wraps the session's WebSocket as a
`net.Conn` and drives it with `agentloop.NewToolHost`: 20 warm-up calls, 1,000
`read` calls of a small file, then 300 mixed calls (read 35%, `bash` echo
25%, grep 15%, write 15%, ls 10%). Each process-session call is one `input`
frame and one `stdout` frame. Native: the same program against the tool host
on the host over a unix socket. The gVisor column was not run: rootless
`runsc` could not re-execute itself on this host (`fork/exec /proc/self/exe:
permission denied`), and root was not used for it.

**Raw, no-op `read`, three runs each (ms):**

| Transport | p50 | p95 | p99 |
| --- | --- | --- | --- |
| Native unix socket | 0.010–0.045 | 0.011–0.057 | 0.014–0.075 |
| TCP port session | 0.34–0.69 | 1.21–1.61 | 1.59–1.91 |
| Process-session stdio | 2.17–4.20 | 7.34–7.68 | 8.05–8.48 |

**Raw, mixed (ms):**

| Transport | p50 | p95 | p99 | 300 calls |
| --- | --- | --- | --- | --- |
| Native | 0.07–0.12 | 0.96–2.16 | 1.03–2.78 | 93–165 ms |
| TCP port session | 0.69–1.24 | 3.07–3.57 | 3.64–4.15 | 288–421 ms |
| Process-session stdio | 2.08–3.30 | 2.85–9.22 | 3.14–10.49 | 634–1,361 ms |

**Verdict.** Pass on both transports against the budget fixed beforehand
(p50 ≤ 5 ms, p95 ≤ 25 ms). Both carry the socat hop, so they are upper
bounds. The bidirectional attach (`GET /v1/sessions/{id}/attach`) and port
sessions are marked Incubating in SPACE's API; its supported surface carries
command output only, over SSE.

## 5. Freeze, suspend and resume with state inside

**Method.** A 2-vCPU, 1 GiB sandbox runs a SQLite writer inserting a
timestamped row every 20 ms, the tool host behind a held process-session
connection, and a marker file on disk. Firecracker's CPU ticks are read on
the node during the pause.

**Raw.**

| Step | Result |
| --- | --- |
| Pause | 110–114 ms; the VMM's CPU ticks unchanged over 3 s; largest gap between rows equals the pause |
| Wake from pause | 109–119 ms; the held connection answers again |
| A tool call sent while paused | the stream closes within 3 ms (`EOF`); the guest's processes survive, and re-attaching the same session with its original token reaches the same tool host |
| Suspend | 2.37–2.46 s; the VMM leaves the host; one `full` snapshot, `size_bytes` 1,140,875,018 |
| Wake from suspend, same node | 0.94 s; the restored VMM starts at 48 MB RSS |
| Wake from suspend, original node cordoned | 2.04 s, on the other sandbox worker |
| After each restore | rows intact and still growing, writer at the same pid, the same tool-host process answering after a re-attach (37–41 ms), marker file present |

A session read does not return attach credentials; only the create response
carries the token, so a backend keeps it to re-attach. While one client is
attached, another attach is refused (`session_already_attached`). Closing an
attach does not stop the process: a backend's `Stop` kills explicitly.

**Verdict.** Pass, including a restore on a second node (two SPACE nodes on
one host). The rule a
backend takes from it: no call is dispatched while frozen, and thawing
re-attaches if the stream was cut.

## 6. What SPACE reports about a sandbox's cost

**Method.** A 1-vCPU, 512 MB sandbox; a guest process allocating and
touching 64 MiB every 1.5 s, while the host polls `GET
/v1/sandboxes/{id}/stats` every 0.5 s and reads the incubating
`metrics-stream`.

**Raw.** Fields of both endpoints: `cpu_max`, `memory_max`,
`memory_current`, `pids_max`, `pids_current`, `cpu_usage_usec`,
`network_tx_bytes`, `network_rx_bytes`, `net_gateway_reachable`,
`tcp_active_connections`, `tcp_total_connections`,
`tcp_denied_connections`, `udp_active_mappings` and `bytes_forwarded`.
During the growth, `memory_current` stayed at about 6 MiB (6.2 MiB at most)
across 54 samples while the process reached 428 MB; `memory_max` read 8,724,152,320
bytes for a 512 MB sandbox and `cpu_max` read two CPUs for a one-vCPU
sandbox. There is no peak field.

In the guest, commands run in `/space.slice/space-workloads.service/workloads`,
and `/space.slice` is capped at 442,400,768 bytes (512 MiB less a reserve).
The guest kernel's OOM killer killed the process at 428 MB of anonymous
memory (`constraint=CONSTRAINT_MEMCG, oom_memcg=/space.slice`). The sandbox
stayed `running` with no failure code; the process's `process_exited` frame
read `exit_code 137, status "signaled"`, the same as a `kill -9`; the
lifecycle-event stream carried nothing. The guest itself records it:
`/proc/vmstat` `oom_kill 1`, and `space-workloads.service`'s `memory.events`
`oom_kill 1` and `memory.peak` 442,650,624, besides the kernel log.

**Verdict.** Peak memory is not reported, and the reported memory is not the
guest's. One process is killed, not the VM. `OOMKilled` and actuals would
have to come from inside the guest.

## 7. Idle memory and density

**Method.** N sandboxes from the kit template, 1 vCPU and 512 MB each, each
running the tool host idle behind a loopback bridge with no stream open.
Host used memory is `MemTotal − MemAvailable`; per-process RSS and PSS were
read on the nodes for each sandbox's Firecracker, net gateway and
`spaced-host` processes, 30 s after the last create.

**Raw.**

| N | Host used delta | Firecracker RSS | Gateway RSS / PSS | `spaced-host` RSS / PSS | PSS per sandbox |
| --- | --- | --- | --- | --- | --- |
| 1 | within noise | 99.1 MB | 19.9 / 10.4 MB | 17.3 / 8.8 MB | 118 MB |
| 10 | 1,236 MB (124 MB each) | 983 MB | 199 / 40 MB | 173 / 35 MB | 106 MB |
| 50 | 5,929 MB (119 MB each) | 4,906 MB | 998 / 150 MB | 863 / 129 MB | 104 MB |

An idle guest shows 82–86 MB used of its 482 MB. Firecracker's RSS follows
the pages the guest has touched, and sandbox pods request only a network
slot (300 per node), not memory or CPU, so guest memory is overcommitted,
not reserved. The 39 sandboxes after the first eleven were created in 20 s
at six at a time.

**Verdict.** About 104 MB per idle sandbox against gVisor's ~24 MB per idle
jail: roughly four times as much, before the ~24 MB sidecar every backend
pays.

## 8. Bring-up latency

**Method.** 20 sequential bring-ups from the kit template (2 vCPU, 1 GB):
the create call, the operation and a `running` sandbox, a process session
starting socat and the tool host, then the first `bash` call answered.

**Raw (ms).**

| Phase | p50 | p95 | min | max |
| --- | --- | --- | --- | --- |
| Create to running | 951 | 1,497 | 466 | 1,632 |
| Running to attached | 11 | 20 | 5 | 22 |
| Attached to first answer | 358 | 668 | 35 | 846 |
| Total | 1,299 (1,303 by phase) | 1,596 | 1,114 | 2,112 |

**Verdict.** 4.5–7.6 times gVisor's spawn p50 of ~170–290 ms, without the
warm pools or base snapshots SPACE offers to shorten creates.

## 9. A build burst

**Method.** This repository with dependencies vendored and
`frontend/node_modules` installed, Go 1.27.2 and Node 22.23.3 on both sides.
The workload is `go test -count=1` over the 100 packages under `internal/`
whose tests do not need Docker (12 packages that use `pgtest` or
testcontainers were left out on both sides, since the guest has no Docker),
then the frontend's `build` script through `npm run build`. Each was run
once to warm the caches before measuring. Native ran pinned to one thread on
each of 8 performance cores; the guest had 8 vCPUs (it was given about 8 GB
of memory against 16 GB requested; the create response does not say).
Guest CPU is the Firecracker process's CPU time on the host.

**Raw.**

| Workload | Native wall | Native CPU | Guest wall | Guest host CPU |
| --- | --- | --- | --- | --- |
| `go test`, 91 packages with tests, all passing | 26.0–27.0 s | 54.3–56.1 s | 26.8 s | 73–74 s |
| Frontend build | 1.74–2.00 s | 4.7–6.9 s | 1.39–1.42 s | 3.7–3.8 s |

**Verdict.** A build burst costs the same wall time as native and about a
third more host CPU, against the ~14× gVisor shows under systrap
(`docs/benchmarks/gvisor-cpu-tax.md`). SPACE limits each sandbox's root disk
to 100 MiB/s and 10,000 operations per second, which bounds an I/O-heavy
build.

## Not run

- **A `git push` from the guest through the run's sidecar, with push policy
  enforced**, a week of real delegations, and the conformance suite against
  a SPACE backend: each needs the backend to exist.
- **The gVisor column of item 4**: see item 4.
- **Item 1 with the Kind NetworkPolicy relaxed**: not needed, since the
  sandbox's own nftables drop private destinations before Kubernetes policy
  applies.

## What this changes in the design

The results agree with §1.1's decision, and add four things it did not
rest on:

- SPACE's guest cannot reach any private address, so a sidecar on the
  executor is reachable only through SPACE's data plane.
- The bidirectional stream the tool host needs is an Incubating API, and it
  closes when a call is sent to a paused sandbox.
- An idle SPACE sandbox costs about four times a gVisor jail in host memory.
- SPACE's resource reporting describes neither the guest's memory nor its
  OOM kills.

For TF's own Firecracker backend, items 4, 5, 7 and 9 suggest what a VM costs
regardless of SPACE: Firecracker's RSS follows guest-touched memory (98 MB
for an idle 512 MB guest), a full snapshot is about the size of guest
memory, and a build burst costs about 1.3× native CPU.
