# SKY-254 runsc-on-Fly validation

Validation probe that proved gVisor `runsc` works on Fly.io for per-delegation
sandboxing of TF agent runs. Run 2026-05-14 against an empty Fly App
(`tf-runsc-probe-39b851`, now destroyed).

This directory is **architectural reference, not product code**. It exists so
the eventual D10 implementation has the working recipe in hand, and so future
"why did we pick Fly?" / "why gVisor and not Firecracker?" / "what's the
credential model again?" questions don't require re-doing the analysis.

The pivot context: the original SKY-242 plan targeted Railway for the hosted
deployment. Railway turned out to be a dead end for gVisor — its container
runtime applies seccomp + AppArmor profiles that block the `/proc/self/exe`
re-exec at the heart of `runsc --rootless`. Fly Machines are themselves
Firecracker microVMs with no such restrictions, so runsc runs inside them
with the standard CNI-style setup.

## What's here

| File | What it does |
| --- | --- |
| `Dockerfile` | Image that bundles runsc + the network-plumbing deps (`iptables`, `iproute2`, `procps`). What TF's production image will look like. |
| `fly.toml` | Fly App config for the probe. Mirrors what `D13` (containerization) will produce. |
| `probe.sh` | Sets up the test environment: parent-side state (env vars, fake services, secret file), the OCI bundle (alpine rootfs + `runsc spec`-generated config), then invokes `runsc run`. This is the "naive" path — the `runsc run` step fails for networking. |
| `precns-test.sh` | The **production-mode invocation**. Pre-creates the netns + veth + iptables, patches the OCI bundle to point at the netns, then invokes `runsc run`. **This is the recipe D10 implements in Go.** |

## To reproduce

```bash
fly apps create tf-runsc-probe-XXX --org personal
cd docs/for-agents/specs/sky-254-runsc-validation
# edit fly.toml: change `app = "tf-runsc-probe-39b851"` to your app name
fly deploy --remote-only --ha=false
# Once the Machine is up:
fly ssh sftp shell <<< "put precns-test.sh /tmp/precns-test.sh"
fly ssh console --command "sh /tmp/precns-test.sh"
# Expect: sandbox sees only /work + alpine rootfs; parent localhost services
# blocked; outbound HTTPS to api.github.com works.
# Cleanup:
fly apps destroy tf-runsc-probe-XXX --yes
```

## What was learned (runtime mechanics)

1. **`runsc do --network=sandbox`** (the convenience mode) works on Fly but
   inherits the host filesystem — not production-safe.
2. **`runsc run` against an OCI bundle** is the production mode. The bundle
   ships its own rootfs (alpine minirootfs, ~5MB) and bind-mounts only the
   per-run worktree at `/work`. Everything else of the host is invisible.
3. **`runsc run` does NOT auto-set-up the netns/veth** the way `runsc do`
   does. The caller owns network plumbing — this is the CNI pattern that
   containerd/Docker implement around runc/runsc.
4. **`--ignore-cgroups`** is required on Fly because Fly Machines use cgroup
   v2 unified hierarchy and the bundled runsc release defaults to v1 paths.
5. **DNS** in the sandbox needs explicit public resolvers (1.1.1.1 / 8.8.8.8).
   Fly's internal `fdaa::3` resolver isn't reachable through the sandbox's
   IPv4 NAT.
6. **Image dependencies**: `runsc` binary + `iptables` + `iproute2` + `procps`.
   The TF image bundles all four; customer's host needs no separate install.

## Acceptance criteria the probe confirmed

| Property | Method | Result |
| --- | --- | --- |
| Filesystem isolation | Sandbox attempts `cat /tmp/parent-secret.txt` | `No such file or directory` |
| Bind-mount works | Sandbox reads `/work/agent-input.md` | `worktree-content-from-host` |
| Env curation | Spec's `process.env` has only `AGENT_CURATED_KEY` | Confirmed via `env` inside sandbox |
| Loopback isolation | Sandbox tries `wget http://127.0.0.1:5432` (parent's fake-pg) | `Connection refused` (sandbox loopback ≠ host) |
| Outbound HTTPS by IP | Sandbox `wget https://1.1.1.1` | Cloudflare HTML returned |
| Outbound HTTPS + DNS | Sandbox `wget https://api.github.com/zen` | "Non-blocking is better than blocking." |
| Sandbox kernel | `uname -a` inside | `Linux tf-sandbox 4.19.0-gvisor` (gVisor's user-mode kernel) |

The "env curation" row is the load-bearing observation. It means:
**whatever we put into the OCI spec's `process.env` IS readable inside the
sandbox** via `env`, `/proc/self/environ`, or any other mechanism the process
has access to. The variable name in the probe is literally
`AGENT_CURATED_KEY=sk-ant-CURATED-VISIBLE-IN-SANDBOX`. This single fact
reshapes the credential design — see "Property A vs Property B" below.

---

## Credential model

The probe proved the runtime works. The harder question is: how do we
arrange credentials such that a malicious agent inside the sandbox can't
exfiltrate them? The next several sections answer that.

### Property A vs Property B — what's achievable

**Property A**: "The agent can't read its own env vars / memory / FDs."

**Not technically possible**, ever, with any sandbox technology. A process
must be able to access its own state to run. Linux exposes env via
`/proc/self/environ`, memory via `/proc/self/mem` (and `process_vm_readv`),
FDs via `/proc/self/fd/`. gVisor doesn't change this; Firecracker doesn't
change this; even SGX enclaves only change it for specific encrypted
regions, not for normal process state. **No design can deliver A.**

**Property B**: "Nothing the agent CAN read from its env / memory / FDs is
a credential usable to exfiltrate anything anywhere."

**Achievable.** This is the property we design against. The mechanism:
don't put credentials in the sandbox at all. The agent's process state
contains only:

- Placeholder env vars (e.g. `ANTHROPIC_API_KEY=sk-ant-PROXY-PLACEHOLDER`)
- Proxy URLs that point at host-side processes (e.g.
  `ANTHROPIC_BASE_URL=http://192.168.99.1:NNNN`)
- The agent's own program data (responses from model calls, files it's
  editing — none of which are credentials)

If a jailbroken agent dumps its entire process state and embeds it in a
PR description, commit message, or tool output, an attacker reading that
finds nothing usable.

Anthropic's own production session sandboxes deliver Property B by a
different mechanism — see "Anthropic intel summary" below. Both approaches
work; ours is the right one for the self-host constraint.

### Per-channel walk-through

For Property B to be a real guarantee, every channel that could leak a
credential must be plugged. Each row below names a channel the agent has
access to, what it could observe, and how we ensure that observation is
useless to an attacker.

| Channel | What the agent observes | Usable credential? | How we ensure |
| --- | --- | --- | --- |
| `process.env.ANTHROPIC_API_KEY` | placeholder string | No | Proxy holds the real key; agent gets a placeholder by design |
| `process.env.ANTHROPIC_BASE_URL` | `http://192.168.99.1:NNNN` | No — IP only routable from inside this sandbox's netns | gVisor network isolation + egress allowlist |
| `process.env.GITHUB_TOKEN` (or equivalent) | placeholder / unset | No | Git proxy holds short-TTL GitHub App installation token; agent's git uses `http.proxy=` config |
| `/proc/self/environ` | same as above | No | Same |
| `/proc/self/mem` (or `process_vm_readv`) | placeholder + URL + transient response data | No | Credentials live in **proxy processes' memory** on the host side, not the agent's. They share no address space. |
| `/proc/self/fd/` | stdin / stdout / stderr | No | We do not FD-pass credentials. (Anthropic does; for them the token's authority is what matters, not its visibility. Different design.) |
| Worktree filesystem | source code, no credentials | No | We never write credentials to the worktree; pre-PR test asserts this |
| Sandbox rootfs | base alpine image, our shim binaries | No | Pre-baked rootfs has no per-org state |
| Outbound HTTPS to `api.anthropic.com` directly | n/a — blocked by egress allowlist | No | gVisor egress allowlist contains only the proxy IPs, NOT direct upstream APIs |
| Outbound HTTPS to the proxies | model / git responses | No — those are application data, not auth material | Proxies add auth on outbound; responses are plain HTTP |
| `triagefactory exec` IPC over bind-mounted unix socket | RPC responses scoped to the run's identity | No — agent has no token to leak | SKY-303's socket→identity binding (the socket IS the credential; host owns the mapping) |

The critical observation across all of these: **the agent process never
has the real credential in its address space.** Not in env, not in memory,
not in an FD, not on disk. The credentials live entirely in proxy
processes on the host side, which translate "request from this run's
sandbox" into "authenticated request to the upstream API."

### Implementation invariants that deliver Property B

The architecture supports the property, but the implementation has to be
tight. If any of these five invariants is violated, Property B breaks:

1. **gVisor egress allowlist must NOT include upstream LLM/git API endpoints.**
   The sandbox can reach only the proxy IPs. If `api.anthropic.com` or
   `api.github.com` appear in the allowlist, the agent can go around the
   proxy with some-other-key (or with whatever it finds elsewhere in the
   sandbox). The allowlist must be proxy-only.

2. **Proxies bind on non-routable IPs reachable only from the specific
   sandbox.** The veth-IP approach (e.g. `192.168.99.1`) gives us this
   for free — the IP is only routable from inside the netns that has the
   matching veth interface. `internal/llmproxy.Config.AllowNonLoopback`
   gates this: the proxy refuses to bind on a non-loopback interface
   unless the caller has consciously opted in (the veth-IP case).

3. **No credential ever written to the worktree filesystem.** Code paths
   that previously wrote credentials to disk for the agent's consumption
   must be removed. Test asserts post-run worktree contains no key-shaped
   strings.

4. **No host→agent control channel that injects credentials.** If we ever
   add a control plane that pushes data into the sandbox (e.g., a websocket
   for live UI takeover), that channel must not carry credentials. Same
   rule: credentials live in proxies; control planes carry user input
   and metadata, not auth material.

5. **The proxy URL is a label, not a secret.** The placeholder env var
   value (`sk-ant-PROXY-PLACEHOLDER`) is intentionally fixed and is not
   protected. The actual security comes from the netns boundary + the
   proxy injecting real auth on outbound. A future maintainer should not
   "rotate" or "randomize" the placeholder thinking it's a credential.

A test suite that runs in CI dumps `/proc/<agent-pid>/environ`, the agent's
memory regions, and the post-run worktree filesystem, then greps for
credential-shaped strings (Anthropic key prefix `sk-ant-`, AWS access key
prefix `AKIA`, GitHub PAT prefix `ghp_` / `github_pat_`, etc.). The test
fails if any matches are found.

### What Property B does NOT cover

Property B is about exfiltration: a leaked credential should be useless
off-host. It does not cover credential **misuse** during the run window
itself:

- The agent can issue extra model calls through the proxy during its run
  (eats the org's API quota / spends the org's money on non-task work).
- The agent can ask the LLM to encode worktree contents in some indirect
  way and emit them as responses (laundering legitimate data access into
  an exfiltration channel — though the data was already in the worktree
  the agent had legitimate access to).
- The agent can submit off-task git operations within the GitHub App
  installation's scope.

These are **T2 in the SKY-254 threat model** — model-as-attacker / authority
abuse. Bounded by:

- Per-run wall-clock: the proxies die when the run ends, after which the
  agent has no credentialed channel.
- Network egress allowlist: even during the run, the agent can only reach
  the proxies, not arbitrary internet hosts.
- Future per-run policy at the proxies: quotas, allowed-models lists,
  audit logging, abuse detection. Tracked separately as a v1.x follow-up
  on the LLM proxy + git proxy work.

The full property the design eventually delivers is:
**every action the agent takes is one the host can attribute, rate-limit,
and revoke at the granularity of the run.** Property B (exfiltration) is
the v1 floor; Property B + per-run policy is the v1.x target.

---

## Threat model (T1–T4)

Mirrors the SKY-254 ticket body so they don't drift. Naming the four
threats prevents review from talking past itself.

**T1: Prompt-injected agent attempts to exfiltrate credentials.**
The credential the agent uses must never leave the run as a usable
artifact. Addressed by Property B above.

**T2: Prompt-injected agent attempts to misuse credentials it CAN reach.**
The agent can issue authenticated requests through the proxies during the
run. Bounded by wall-clock + network egress + future per-run policy. v1
floor: bounded; v1.x target: bounded + rate-limited + audited.

**T3: RCE in `claude` / Node SDK escapes the SDK process and attempts to
reach the host.** gVisor's user-mode kernel + filesystem isolation +
network namespace + egress allowlist. The escaped attacker has the same
view of the world the agent had — sandbox FS, sandbox netns, proxy URLs,
no host kernel access. **In-sandbox hardening** (non-root UID, seccomp,
cap drop, `noNewPrivileges`) is load-bearing here, not optional defense
in depth — Anthropic gets away with root + full caps inside their VMs
because they trust the KVM boundary; we don't have that boundary, so we
need the additional layer.

**T4: RCE escapes gVisor and reaches the host kernel.** gVisor's syscall
filter + user-mode kernel architecture is the load-bearing defense.
This is the reason we use gVisor at all. No additional layer on top in
v1; we trust gVisor's security model here.

**Local mode collapses T1/T2/T4.** Single-user installs have no other
tenant to protect from, and the user is trusted with their own credentials.
T3 still applies as defense in depth but isn't load-bearing. The gVisor
path is gated on `runmode.Current() == ModeMulti && runtime.GOOS == "linux"`.

---

## Anthropic intel summary (2026-05-19 architectural probe)

Sourced from a probe into Anthropic's own cloud-hosted Claude Code session
runtime. They run **Firecracker microVMs per session**, not gVisor. We
asked them a focused set of architecture questions; what follows is the
useful pattern set.

### Patterns we steal

**1. Local credential proxy for git, holding the real GitHub credential
server-side.** Their setup: two localhost listeners next to the host-side
dispatcher. Agent's git is configured with `http.proxyAuthMethod=basic`
and `CLAUDE_CODE_PROXY_RESOLVES_HOSTS=true`. The proxy holds the GitHub
credential; the agent's git just CONNECTs through localhost.

**Direct adoption**: the git proxy ticket (sibling to SKY-331) implements
the same pattern. Listener lives on the host-side veth IP; agent's git
config in the worktree's `.git/config` sets `http.proxy=` to the proxy URL.

**2. GitHub API operations via server-side dispatch (their MCP fanout, our
`cmd/exec` IPC).** Their system prompt explicitly forbids the `gh` CLI;
all GitHub API operations go through a server-side MCP endpoint. The
agent never holds a GitHub API token even for read operations.

**Already implemented**: SKY-303's `cmd/exec/gh` shim is functionally
equivalent — the host process executes the API call with org/user
identity bound to the per-run socket; the sandbox just speaks RPC.
Different mechanism, same property.

**3. Narrow CLI surface vs broad pass-through proxy.** Their pattern (and
ours):
- **Enumerable operations** (DB writes, GitHub API actions, specific
  workspace commands) → narrow CLI invoked over the per-run socket. The
  agent can only do what the CLI exposes.
- **Broad action shape** (LLM calls, generic git over HTTPS) → pass-
  through proxy with host-side auth injection.

Stronger property for the enumerable operations: the agent can't craft
arbitrary SQL / arbitrary GitHub API calls even if it tries.

**4. Commit signing via a black-box signer helper.** Their setup:
`gpg.format=ssh`, `gpg.ssh.program=/tmp/code-sign` (symlink to a binary
that only supports `-Y sign`). Private key stays on the host side; agent
shells out for signatures only.

**Pattern documented; not v1 scope.** Filed for customer-request /
v2 if needed.

### Patterns we can't replicate (and our mitigation)

**Session-scoped credentials.** Anthropic's internal session sandboxes use
OAuth tokens scoped to a single session ID against a server-side endpoint
that knows how to gate by session. A leaked token's blast radius is one
session and the server can revoke it. **They get T2 protection from this**
in a way we can't.

For us as Anthropic API customers, API keys are long-lived and tenant-wide.
We don't have an equivalent. Mitigation:

- **Credential confidentiality (T1)**: equivalent — the proxy pattern
  delivers Property B even without session-scoped credentials, because
  the credential never leaves the proxy in the first place.
- **Credential authority scope (T2)**: weaker — bounded only by the run
  window + per-run policy at the proxy, not by API-side authority limits.

The only way to close the T2 gap fully is for Anthropic to expose scoped
API keys (short TTL, model-scoped, request-shape-restricted). Some LLM
providers offer this (OpenAI's project keys, organization restrictions).
Anthropic doesn't expose it for API customers today. If we end up with
real revenue, this is a sales-leverage ask.

**Git credentials**: GitHub App installation tokens DO give us the
short-TTL property (1-hour, repo-scoped). The git proxy mints these
on-demand. Closes the equivalent T2 gap for git.

### Other footguns flagged

**In-sandbox UID 0 + full caps is wrong for gVisor.** Their VMs run agent
as root with full caps because they trust the KVM boundary; gVisor's
user-mode kernel isn't as strong, so we need additional in-sandbox
hardening (non-root UID, seccomp, cap drop, `noNewPrivileges`).
Captured in SKY-254's "In-sandbox hardening" section.

**File descriptor passing for credentials** (their
`CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=4` pattern). Not load-bearing
for us — Property B holds without it because we never put the real
credential into the sandbox at all. The FD pattern would be relevant if
we had to inject a credential and wanted to keep it out of env;
since we don't have to inject anything real, it's moot.

### What this means for SKY-254

Property B + the proxy pattern is sufficient. The Anthropic intel
**validates the approach** rather than redirecting it. The specific
operational lessons (proxy-per-credential-type, narrow CLI for
enumerable ops, in-sandbox hardening for non-KVM sandbox tech) are
folded into the SKY-254 ticket and the sibling tickets (SKY-331 LLM
proxy, git proxy TBD, SKY-303 IPC).

---

See `docs/for-agents/multi-tenant-architecture.html` §6 for the high-level
architectural framing. See the SKY-254 ticket for the implementation
plan and acceptance criteria.
