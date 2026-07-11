# Playwright + Chromium in the gVisor sandbox

Spec **and** prototype guide for letting a delegated agent render a project's
own frontend locally, screenshot/record it, and attach the result as a run
artifact — all inside the existing `runsc` (gVisor) sandbox.

This doc is written to be implementable without a second research pass. It
names the exact files to touch, the exact package/spec/seccomp deltas, the
container-tier model the feature needs, and a validation plan. That plan has
now been **run locally against real `runsc`** (release-20260511.0) in a
`--privileged` container with the host `runsc` bind-mounted — correcting the
earlier assumption that `runsc` needs a Fly Machine. The probe scripts live
beside this doc (`probe-browser.sh`, `probe-seccomp.sh`) and mirror
`docs/for-agents/specs/sky-254-runsc-validation/precns-test.sh`.

Status: **proposal — locally validated** (results in §8). No product code
written yet; the §5 deltas are the implementation, and the validation that
gated them now passes. Parent context: the SKY-254 sandbox epic
(`internal/sandbox`).

> This is one worked instance of the broader **sandbox fleet** model
> (`docs/for-agents/specs/sandbox-fleet/`): a "browser" profile is just one configurable
> sandbox type among many. Read that doc for the general profile/egress/resource
> framing; this one is the concrete Chromium recipe.

---

## 1. Goal and non-goals

**Goal.** A delegated agent working a frontend task can:

1. Start the project's dev server inside the sandbox (`npm run dev`, vite, etc.)
   bound to `127.0.0.1`.
2. Drive a headless Chromium via Playwright against that loopback URL.
3. Capture screenshots and/or videos into the worktree.
4. Have those land in `run_artifacts` like any other structured output.

**Non-goals (explicit, and they're what make this tractable):**

- **No external web access.** Chromium never browses the open internet. The
  only thing it loads is the project's own dev server over loopback. This is the
  single most important scoping decision — see §3.
- **No Chromium internal sandbox.** We run `--no-sandbox`. That is correct here,
  not a compromise (§4).
- **No runtime install.** The browser ships *in* the sandbox rootfs at launch.
  Nothing is downloaded per-run.

---

## 2. Why this was "basically no" before, and why it's "yes, with work" now

An earlier analysis of "can Playwright/Chromium run in our sandboxes?" found six
independent blockers. Four of them are dissolved by the non-goals above:

| Earlier blocker | Status under this scope |
| --- | --- |
| Not installed; would need runtime install | Baked into rootfs at build (§5.1) |
| Chromium's setuid/namespace sandbox can't init (zero caps, `noNewPrivileges`, non-root UID) | Moot — we run `--no-sandbox`; gVisor is the real boundary (§4) |
| Egress is proxy-only — can't reach websites or even DNS | Moot — loopback only; the dev server is *in* the sandbox (§3) |
| musl/Alpine vs Playwright's glibc browser download | Solved by the Alpine `chromium` apk + skip-download (§5.1), with one pinning gotcha |
| Seccomp profile omits `clone`/`clone3` | Non-issue — `runsc` doesn't enforce the OCI seccomp profile (§5.4, validated; TFAC-299) |
| No `/dev/shm`, low rlimits | Real, spec tuning (§5.3) |

So the remaining work is **rootfs composition + a handful of spec/allowlist
tweaks + a container-tier model**, all now backed by the local `runsc`
validation in §8. No architectural fight with the egress/credential model, and
no seccomp change.

---

## 3. The load-bearing insight: it's all loopback

Playwright's usual job is "browse the web," which is exactly what our
proxy-only egress policy (`internal/sandbox/iptables_linux.go` `applyEgressPolicy`,
SKY-395) is built to forbid. We don't need that.

A frontend dev server runs **inside the same sandbox/netns** and binds
`127.0.0.1:<port>`. Chromium connects to `http://127.0.0.1:<port>`. That traffic:

- never leaves the network namespace (loopback stays in-netns), and
- is explicitly permitted: `applyEgressPolicy` adds
  `-A OUTPUT -o lo -j ACCEPT` *before* flipping the default to DROP
  (`iptables_linux.go:99`).

So the feature needs **zero changes to the egress policy** and does not widen the
exfiltration surface the SKY-254 Property-B model closes. That is the whole
reason this is worth doing inside the existing sandbox rather than a separate
escape hatch.

> One thing to keep honest: if a future iteration ever wants Chromium to load
> *external* URLs (a staging deploy, a design system on a CDN), that crosses back
> into the egress-allowlist conversation and must be designed as a forward-proxy
> with an allowlist — not a relaxation of the default DROP. Out of scope here;
> flagged so the next person doesn't quietly punch a hole.

---

## 4. `--no-sandbox` is correct here

Chromium's multi-layer internal sandbox needs either `CAP_SYS_ADMIN` or
unprivileged user namespaces to set up its zygote. Our spec drops **all**
capabilities, sets `NoNewPrivileges: true`, and runs as non-root UID 10000
(`internal/sandbox/spec.go:78,83`), and gVisor's user-namespace support is
limited. So Chromium's inner sandbox cannot initialize.

Running `--no-sandbox --disable-setuid-sandbox` is the right answer because
**gVisor is already the security boundary.** Disabling Chromium's inner sandbox
removes a layer that couldn't function anyway; the gVisor user-mode kernel (T3/T4
in the SKY-254 threat model) is what actually contains an RCE. A bonus: with
`--no-sandbox`, Chromium never attempts `unshare`/`CLONE_NEWUSER` — moot anyway
since `runsc` doesn't enforce the OCI seccomp profile (§5.4), but it keeps the
namespace-creation surface closed regardless.

Launch flag set we expect to ship:

```
--headless=new --no-sandbox --disable-setuid-sandbox \
--disable-dev-shm-usage --disable-gpu
```

`--headless=new` uses SwiftShader software rendering, which works under gVisor
with no GPU. `--disable-dev-shm-usage` is the simple alternative to sizing a
`/dev/shm` mount (§5.3).

---

## 5. The changes (prototype guide)

Each subsection is a concrete delta against a named file.

### 5.1 Rootfs: bake the browser in (`internal/sandbox/rootfs.go`, `rootfs_linux.go`)

The rootfs is built by `installToolchain` via `chroot + apk add` into a cached
directory keyed on `(alpine sha, apkPackages)` (`rootfs_linux.go:152`; the
community repo is already enabled at `:157`). Adding packages rotates the cache
key (`rootfsCacheKeyFor`, `rootfs.go:119`) so a clean re-extraction happens
automatically — no stale-cache hazard.

The **browser** package set (kept separate from `base` — see §6) adds:

```go
// Browser-profile additions on top of the base toolchain.
var browserExtraPackages = []string{
    "chromium",        // musl-native build, community repo. Runs with --no-sandbox.
    "font-noto",       // Latin/base glyphs — without fonts, screenshots are tofu boxes.
    "font-noto-cjk",   // CJK coverage; drop if image size matters and CJK isn't needed.
    "font-noto-emoji", // emoji glyphs.
    "freetype",        // pulled by chromium but list it so the intent is explicit.
    "ffmpeg",          // ONLY needed for Playwright video recording; omit for screenshots-only.
    "nss",             // chromium TLS/cert db; usually pulled, listed for clarity.
}
```

Notes that will bite if skipped:

- **Fonts are not transitive.** The `chromium` apk does not pull a font set.
  Without `font-noto*` (or `ttf-freefont`), every screenshot of a real frontend
  renders missing-glyph boxes. This is the #1 silent failure.
- **`ffmpeg` is only for video.** Playwright shells out to ffmpeg for
  `recordVideo`; screenshots don't touch it. Gate it on whether video is a
  product requirement (it adds meaningful image size).
- **Executable path.** The Alpine apk installs `/usr/bin/chromium-browser`. We
  point Playwright at it rather than letting it download (see 5.2).

**Playwright the npm package** is JS, so it's installed once into the browser
rootfs during the chroot build, alongside the apk step in `installToolchain`:

```sh
# inside the chroot, browser profile only:
npm install -g playwright@<pinned>   # the library + CLI, NOT its browser binaries
```

— combined with the skip-download env in 5.2 so `npm install` does **not** try to
fetch the glibc Chromium it normally bundles.

> **The one real gotcha: version skew.** Playwright pins to a specific Chromium
> build/CDP-protocol revision; the Alpine apk gives whatever Alpine currently
> ships. A mismatch shows up as CDP errors ("Protocol error", missing methods) at
> launch, not at install. **Pin both** — a `playwright@X` known-compatible with
> the `chromium=Y` Alpine has at your pinned `alpineVersion` — and re-verify the
> pair whenever either moves. This is the thing most likely to break a green
> prototype, so it gets its own line in the validation plan (§8).

### 5.2 Browser executable wiring (sandbox `Config.Env`)

The browser profile injects, into the sandbox env (`sandbox.Config.Env`, set by
`internal/agentproc`):

```
PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1
PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/usr/bin/chromium-browser
```

These are plain config, not credentials — the Property-B invariant
(`internal/sandbox/doc.go`) is untouched.

### 5.3 `/dev/shm` and rlimits (`internal/sandbox/spec.go`)

Today there is no `/dev/shm` mount and `/dev` is a 64 MB tmpfs
(`spec.go:105`); `RLIMIT_NOFILE` is 1024 and `RLIMIT_NPROC` 512 (`spec.go:91`).
Chromium is FD- and process-heavy.

Two options for shm, pick one:

- **Simple (recommended for screenshots):** pass `--disable-dev-shm-usage` and
  add nothing. Chromium writes to `/tmp` instead. Reliable, no spec change.
- **Mounted shm (better for video / many tabs):** add to the `Mounts` slice in
  `buildSpec` (next to `spec.go:111`):

  ```go
  {Destination: "/dev/shm", Type: "tmpfs", Source: "tmpfs",
      Options: []string{"nosuid", "nodev", "mode=1777", "size=256m"}},
  ```

Rlimit bumps for the browser profile (leave `base` as-is):

```go
{Type: "RLIMIT_NOFILE", Hard: 8192, Soft: 8192},
{Type: "RLIMIT_NPROC",  Hard: 1024, Soft: 1024},
```

These should be **profile-conditional** (§6), not a global bump — the base
profile has no reason to carry browser-sized limits.

### 5.4 Seccomp: `clone`/`clone3` (`internal/sandbox/syscalls.go`) — RESOLVED: no change needed

`defaultAllowedSyscalls` omits `clone`/`clone3`, and Chromium fans out its
zygote → renderer/GPU/utility processes via `clone`. The open question was
whether that omission would block Chromium and force the one-line add.

**Validated (§8): it doesn't — because `runsc` does not apply the OCI seccomp
profile to the sandboxed app at all.** Of the two hypotheses, (a) is correct.
The dispositive test (`probe-seccomp.sh`): a `SCMP_ACT_KILL_PROCESS` default
with zero allowed syscalls had *zero* effect on the sandboxed process. And
`KILL_PROCESS` is the highest-precedence seccomp action, so it cannot be
shadowed by the `RET_TRAP` gVisor's systrap uses to intercept syscalls — if the
profile were applied, the process would die on its first syscall. It ran to
completion. Corroborated by gVisor's docs: gVisor's own seccomp confines the
**Sentry**'s host syscalls; the application's syscalls are serviced by the
Sentry (user-space kernel) and never filtered against our OCI profile.

So **`syscalls.go` needs no change** for this feature — `clone`/`clone3` are a
non-issue, and the profile is inert in the gVisor path regardless. (A
consequence worth its own note: the shipped profile is *not* the enforced
in-sandbox control its comments implied — `syscalls.go`/`spec.go` comments are
corrected alongside this doc, and the customer-facing security page is tracked
in TFAC-299.)

### 5.5 Tool allowlist + orchestration (`internal/agentproc/allowlist.go`)

`npx` is intentionally blocked (`allowlist.go:258`), so the agent currently has
no way to invoke Playwright. Two pieces:

1. **Expose the CLI.** Either add an explicit `Bash(playwright *)` entry, or —
   more in keeping with the codebase's "narrow CLI over the per-run socket"
   philosophy (`cmd/exec`) — add a `triagefactory exec screenshot <url> <out>`
   wrapper the agent calls. The wrapper approach is preferable: it fixes the
   flag set (`--no-sandbox` etc.), constrains output to the worktree, and keeps
   the agent from crafting arbitrary Playwright scripts. Recommend the wrapper.

2. **Background the dev server.** "Start `npm run dev`, then screenshot" needs
   the dev server running concurrently with the capture step under headless
   `claude -p`. `Bash(npm run *)` is already allowed; the orchestration
   (background + readiness-wait + teardown) is the design work, and it runs
   against `RLIMIT_NPROC` (§5.3). A `triagefactory exec render` wrapper could own
   the whole "boot server → wait for port → screenshot → kill server" dance so
   the agent issues one call.

### 5.6 Artifacts

Screenshots/videos written under `/work` (the worktree) flow into the existing
`run_artifacts` model (`is_primary` per run). No new plumbing — point the
wrapper's output at a known worktree subdir and register it like any structured
output.

---

## 6. Container tiers

Browser-enabled sandboxes are a different resource class (bigger image, more
RAM, bigger shm, higher rlimits). They should be an opt-in **profile**, not the
default everyone pays for.

### 6.1 Profile model

Introduce a sandbox **profile** selected per-run:

- **`base`** — today's toolchain (`apkPackages`). Unchanged.
- **`browser`** — `base` + `browserExtraPackages` (§5.1) + Playwright +
  browser-sized rlimits/shm (§5.3). No seccomp change (§5.4).

Implementation shape:

- `sandbox.Config` gains a `Profile` field (default `base`).
- `apkPackages` becomes a function of profile; `rootfsCacheKey()` already keys on
  the package set, so each profile **automatically** caches to its own
  `rootfs-<key>` dir (`paths.SandboxRootfsDir(cacheKey)`,
  `internal/paths/paths.go:174`) — two profiles, two cached rootfs trees, no
  collision.
- `buildSpec` branches on profile for rlimits, the `/dev/shm` mount, and the
  browser env vars.
- No `syscalls.go` change — `runsc` doesn't enforce the OCI seccomp profile
  (§5.4 / TFAC-299), so there's nothing to add for either profile.

### 6.2 Who picks the profile

Explicit, not inferred from the prompt. The cleanest trigger: a flag on the
`prompt_trigger` / task config ("needs browser") that the delegation spawner maps
to `Config.Profile = "browser"` and to a larger machine class. Keep selection
data-driven so a CI-triage task never accidentally spins a 1 GB browser sandbox.

### 6.3 Sizing → the upcharge lever

On `app.triagefactory.com`, sandboxes run as gVisor *inside* the Fly Machine, so
"a bigger sandbox" means a bigger Fly Machine (the `[[vm]]` size in `fly.toml`;
today `shared-cpu-2x` / 2 GB). Chromium wants more — budget ~1 GB headroom for a
browser + dev server + Node on top of the base footprint.

- **Hosted SaaS:** the browser profile is a paid add-on. Gate at spawn time on
  the org's plan/entitlement; browser-profile runs route to a larger Machine
  class (or a dedicated browser-capable Machine pool). This is the natural
  upcharge — you're selling the larger sandbox, metered per run or per plan.
- **Self-host (Tier 3):** the operator enables the browser profile and sets their
  own Machine/host sizing in their deployment config; they eat their own infra
  cost, no entitlement gate. Mirrors the existing self-host-configures-everything
  posture (`docs/security/isolation-tiers.md`).

### 6.4 Image-size / cold-start cost

The browser rootfs adds roughly **400–700 MB** (Chromium + fonts + ffmpeg). The
rootfs cache is per-host, so on an autoscaling Fly fleet every *new* Machine pays
a cold download+extract on first browser run. Mitigation: **pre-bake the browser
rootfs variant into the published image** (`docker/Dockerfile`) so it's present
at Machine boot instead of fetched on first use. Worth doing for the hosted
fleet; less important for a long-lived self-host host.

---

## 7. Performance note

The SKY-254 perf benchmark (`docs/for-agents/specs/sky-254-perf-benchmark/results.md`)
measured `systrap` at ~25× per-syscall overhead worst case, but concluded the
agent's normal mix is ~80% network I/O so the overhead barely shows. **Chromium
breaks that assumption**: rendering and layout are CPU- and syscall-heavy, not
network-bound, so gVisor overhead is more visible here than for a normal agent
run. Expect screenshots to be usable but not snappy. Fine for an async triage
artifact; size the run wall-clock budget accordingly.

---

## 8. Validation — run locally against real runsc

> **Correction:** the earlier claim that `runsc` "only runs on a Fly Machine" is
> wrong. The probes below ran locally under real `runsc` (release-20260511.0) in
> a `--privileged` container with the host `runsc` bind-mounted — the same
> harness the SKY-395 cross-tenant test uses. Reproduce:
>
> ```sh
> docker run --rm --privileged \
>   -v "$(git rev-parse --show-toplevel)":/src \
>   -v /usr/local/bin/runsc:/usr/local/bin/runsc:ro \
>   -w /src alpine:3.20 \
>   sh /src/docs/for-agents/specs/playwright-chromium-sandbox/probe-browser.sh
> ```

`probe-browser.sh` (beside this doc) builds the browser-profile rootfs and runs
four staged `runsc` sandboxes over it; `probe-seccomp.sh` settles the
seccomp-enforcement question. What `probe-browser.sh` does:

1. Build a browser rootfs (alpine + `chromium` + fonts + `playwright`) and an OCI
   bundle, exactly as production would (`ensureRootfs` browser profile).
2. `runsc --platform=systrap --network=sandbox run` it with the §4 flags and the
   §5.3 limits.
3. Inside the sandbox:
   - start a trivial static server on `127.0.0.1:8080` (or `vite preview`),
   - `chromium-browser --headless=new --no-sandbox --disable-dev-shm-usage \
     --screenshot=/work/shot.png http://127.0.0.1:8080`,
   - then the same via the Playwright CLI/wrapper.
4. Assert `/work/shot.png` exists, is non-trivial in size, and (eyeball) renders
   text with real glyphs, not boxes.

**Acceptance results** (local runsc release-20260511.0, Alpine 3.20 → Chromium
131.0.6778.108, Playwright pinned 1.49.1):

| Question | Result |
| --- | --- |
| Does the OCI seccomp even enforce? Is `clone`/`clone3` needed? | **No / no.** `runsc` does not apply the OCI seccomp profile to the app — dispositive `SCMP_ACT_KILL_PROCESS` test had zero effect (KILL outranks systrap's `RET_TRAP`, can't be shadowed). §5.4 is moot; `syscalls.go` needs no change. |
| apk Chromium + pinned Playwright (no CDP skew)? | ✅ **with the pin.** Playwright 1.49.x ↔ Chromium 131 launched via `executablePath` + screenshotted cleanly. npm-latest 1.60.0 *would* skew — §5.1's "pin both" is mandatory. |
| Fonts render? | ✅ crisp `font-noto` glyphs, no tofu (eyeballed). |
| `/dev/shm` strategy holds? | ✅ `--disable-dev-shm-usage` suffices; no mount needed. |
| Loopback premise (§3)? | ✅ — but the netns must bring `lo` up (production does, `netns_linux.go`). A self-created empty netns leaves `lo` down → `Network unreachable`. |
| Memory headroom / rlimits? | ✅ base `NOFILE 1024`/`NPROC 512` sufficed for a single screenshot. The §5.3 bump is for video/many-tabs (untested). |
| GPU under gVisor? | ✅ Vulkan/EGL init errors are cosmetic; `--headless=new` falls back to SwiftShader and renders. |
| Video path (`recordVideo` + ffmpeg)? | ⏳ not exercised (screenshots-only probe). |

The §5 deltas are cleared to implement: **strike §5.4** (no seccomp change), the
`/dev/shm` mount is optional, and the rlimit bump is optional (base sufficed for
screenshots; revisit for video/many-tabs).

---

## 9. Security review notes

- **No new egress.** Loopback only; the default-DROP policy and Property-B
  credential model are untouched (§3).
- **`--no-sandbox` is not a regression** — gVisor is the boundary; Chromium's
  inner sandbox couldn't run under our caps/UID posture anyway (§4).
- **The OCI seccomp profile is not a layer here** — `runsc` doesn't apply it
  (§5.4, validated; TFAC-299). Containment is the gVisor Sentry + the
  zero-caps / non-root-UID / `noNewPrivileges` posture, left fully intact. No
  seccomp change is made or needed.
- **Added attack surface is the browser binary + fonts + ffmpeg + Playwright** in
  the rootfs. These are read-only in the sandbox (`spec.go:62`) and run as the
  unprivileged worktree UID. The marginal risk is "a malicious page exploits
  Chromium" — but the only page Chromium loads is the project's own dev server,
  which the agent already has full source access to. No new trust is extended.
- **Wrapper-over-raw-CLI** (§5.5) is the safer allowlist choice: it stops the
  agent from pointing Chromium at arbitrary targets or writing artifacts outside
  the worktree.

---

## 10. File-touch summary

| File | Change |
| --- | --- |
| `internal/sandbox/rootfs.go` | `browserExtraPackages`; profile-aware package set + cache key |
| `internal/sandbox/rootfs_linux.go` | `npm i -g playwright` step in the browser chroot build |
| `internal/sandbox/sandbox.go` | `Config.Profile` field |
| `internal/sandbox/spec.go` | profile-conditional rlimits, optional `/dev/shm`, browser env vars |
| `internal/sandbox/syscalls.go` | **no change** — OCI seccomp isn't enforced under gVisor (§5.4 / TFAC-299) |
| `internal/agentproc/allowlist.go` | Playwright wrapper / CLI allow + dev-server orchestration |
| `internal/delegate/spawner.go` (+ trigger config) | select profile + machine class per task |
| `docker/Dockerfile`, `fly.toml` | pre-baked browser rootfs; larger Machine class for the browser tier |
| `docs/for-agents/specs/playwright-chromium-sandbox/probe-browser.sh` | the §8 local `runsc` validation (already run) |

---

## Related

- `docs/for-agents/specs/sky-254-runsc-validation/` — the sandbox the browser runs in;
  `precns-test.sh` is the probe this one mirrors.
- `docs/for-agents/specs/sky-254-perf-benchmark/results.md` — gVisor overhead numbers (§7).
- `docs/security/isolation-tiers.md` — the tier model the §6 upcharge slots into.
- `internal/sandbox/doc.go` — Property-A/B invariants the loopback scope preserves.
- **TFAC-299** (Done) — establishes that `runsc` does not apply the OCI seccomp
  profile; the basis for §5.4 being a non-issue.
- **TFAC-408** — the parent "configurable sandbox fleet" epic this profile is the
  first instance of.
