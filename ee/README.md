# Triage Factory — Enterprise Edition (`ee/`)

This subtree holds the Enterprise Edition. It is source-available for
transparency and audit, but enabling its features requires a valid license
key issued under a commercial subscription. The entire repository — this
subtree included — is governed by the repository-root
[`LICENSE`](../LICENSE) (Triage Factory License 1.0); enterprise features
are protected by the license-key / entitlement gate rather than by a
separate license.

## The one rule

**Core (`internal/*`, `cmd/*`) never imports `ee/`. Only `package main`
imports `ee/`.**

Dependencies point inward: `ee/` may import core; core may not import `ee/`.
This is what keeps a community build buildable without the enterprise tree
and keeps the licensing boundary clean. Enforced mechanically: the "ee import
boundary guard" in `scripts/lint.sh` fails CI if any package under `internal/`
or `cmd/` imports `ee/` (checked across regular, test, and external-test
imports).

## How core and `ee/` communicate

Core exposes neutral **seams**; `ee/` registers implementations into them at
startup (via a blank import from `package main`). Core asks
`entitlements.For(orgID).Has(<feature>)` and never names an enterprise type.

| Seam (core) | Purpose | ee/ side |
| --- | --- | --- |
| `internal/entitlements` | "is feature X licensed for this org?" | `ee.Install()` registers a license-backed per-org provider |
| `db` tx-store extension slot | let ee build tx-bound stores inside core transactions without core knowing their types | ee registers per-dialect store factories |
| `server` route extension | mount enterprise HTTP routes | ee registers route installers; routes always mount, gated per-request on entitlements inside the handler |
| `server` login hooks | SSO enforcement / JIT / test-callback inside the core login path | ee implements opaque hook interfaces |
| event-source seams | let an ee feature ship its own event types: schema + ownership registration, routing hooks, durable publish, entitlement dormancy | ee registers types, source hooks, and a source→feature gate at install |
| `ExtensionAPI.OnReady` | run a long-lived background worker (connection manager, poller) on exactly one pod — gated on the background-brain lease (TFAC-583) — with a ctx that cancels on shutdown OR demotion | ee registers a hook during install; core fires it every time this pod's brain starts, including re-acquisition after a demotion |
| `ExtensionAPI.OnReadyReplicaSafe` | run a worker that's genuinely safe on every pod (idempotent, no duplicating side effects), unconditionally, once at boot | ee registers a hook during install; core fires it once, regardless of brain-lease state — zero callers today, opt in only by explicit declaration |
| Agent CLI verbs | let an ee feature add delegated-agent CLI verbs executed host-side | ee registers an exec subcommand + an agenthost extension handler, entitlement-gated at dispatch |

The full recipe — placement rubric, seam catalog, install anatomy, and the
dormancy contract — is in
[`docs/ee-feature-packaging.md`](../docs/ee-feature-packaging.md).

## What lives here

- `ee/license` — ECDSA P-256 license-token verification.
- `ee/sso` — SSO/SAML/OIDC handlers, GoTrue SSO client, discovery,
  enforcement, break-glass, domains, the verify-before-enforce test flow.
- `ee/sso/store` — the SQLite + Postgres `sso_*` store implementations and
  domain types.
- Frontend SSO surface is gated at render time on the `sso` entitlement served
  by `/api/entitlements` (the `useEntitlements` hook — a single SPA bundle
  can't be split per-license, so the UI is present but inert without the
  feature).

## Licensing a build

The Enterprise **public** key — a standard-base64 DER/SPKI ECDSA P-256 key —
ships as the source default of `ee.publicKeyB64` (`ee/ee.go`), so **every**
build verifies official license tokens: the published image, a compose build
from source, a plain `go build`. The private signing key never ships;
production tokens are minted out-of-band by the licensor's issuing service.
At runtime, `TF_LICENSE=<token>` supplies a signed token; `ee.Install()`
verifies it offline against the baked public key and registers the
entitlements checker. No token, wrong key, or expired token → community
default (every enterprise feature off).

A build against a **different issuer** (a dev signing key, a fork running its
own) overrides the key at link time:

```
-ldflags "-X github.com/sky-ai-eng/triage-factory/ee.publicKeyB64=<b64>"
```

(also exposed as the `TF_LICENSE_PUBLIC_KEY` Docker build arg / compose `.env`
variable). An explicit empty override produces a build with no key at all —
`TF_LICENSE` is then ignored outright.
