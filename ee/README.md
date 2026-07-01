# Triage Factory — Enterprise Edition (`ee/`)

This subtree holds the **commercially-licensed** Enterprise Edition. It is
source-available for transparency and audit, but production use requires a
subscription. See [`ee/LICENSE`](./LICENSE). The rest of the repository is
BSL-1.1 (repository-root `LICENSE`); `ee/` is **not** under that grant and
does **not** convert to open source on the BSL Change Date.

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

Releases bake the Enterprise **public** key — a standard-base64 DER/SPKI
ECDSA P-256 key — via
`-ldflags "-X github.com/sky-ai-eng/triage-factory/ee.publicKeyB64=<b64>"`.
The private signing key never ships; production tokens are minted out-of-band
by the licensor's issuing service. At runtime, `TF_LICENSE=<token>` supplies a
signed token; `ee.Install()` verifies it offline against the baked public key
and registers the entitlements checker. No token, wrong key, or expired token
→ community default (every enterprise feature off).
