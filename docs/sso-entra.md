# Single-tenant SSO with Microsoft Entra (SAML)

Operator guide for wiring a self-host Triage Factory deployment to **Microsoft Entra ID** (formerly Azure AD) for single sign-on. This is the **single-tenant** slice: one statically-configured SAML connection for one Entra tenant, set up via environment variables — there is no in-app "Configure SSO" UI.

- **Multi mode only.** SSO runs through GoTrue, which only exists in the multi-mode stack (`TF_MODE=multi`). Local mode is single-user and has no IdP. Do the [self-host setup](self-host-setup.md) first.
- **SAML, not OIDC.** Entra provisions third-party apps as SAML enterprise apps; the My Apps tile is the SAML entry point.
- **SP-initiated, by design.** The tile links to TF's own SP-initiated start URL rather than firing an unsolicited IdP-initiated assertion. SP-initiated binds the assertion to a request TF made in *this* browser session (`InResponseTo` + PKCE), which closes the login-CSRF hole that true IdP-initiated SAML leaves open. Same one-click tile UX, safer flow.

> **Scope of this page.** This sets up GoTrue + the Entra side so a SAML provider is registered and ready. The actual TF login handlers (the `/sso` start endpoint, JIT provisioning, account merge) and the "Sign in with SSO" button are wired by follow-up tickets (TFAC-426 / TFAC-427). Where this guide references TF's SP-initiated start URL, treat the exact path as finalized by that work; the expected value is noted inline.

## How it fits together

```
Entra enterprise app  ──(SAML federation)──►  GoTrue  ◄──(admin API, at boot)──  Triage Factory
   (IdP: entityID,                            (SP: ACS,                          (registers the one
    SSO URL, signing cert)                     metadata, /sso)                    provider from env)
```

- **GoTrue is the SAML Service Provider (SP).** It exposes the SP metadata + Assertion Consumer Service (ACS) endpoints Entra posts assertions to, and it signs outbound SAML AuthnRequests with an RSA key you generate.
- **TF registers the provider for you.** On startup, when the Entra env vars are set, TF calls GoTrue's admin API to register the one SAML provider — idempotently, and it never overwrites an existing one. You do **not** call the admin API by hand.
- **GoTrue fetches Entra's metadata server-side.** You hand TF the *App Federation Metadata URL*; GoTrue itself fetches it to learn Entra's entityID, SSO URL, and signing certificate. **The GoTrue container therefore needs outbound network access to `login.microsoftonline.com`.**

## 1. Generate the SAML request-signing key

GoTrue signs its outbound SAML AuthnRequests with an RSA private key. **This is a different key from the GoTrue session-JWT signing material** (`jwk-init` / `GOTRUE_JWT_KEYS`) — that one signs user session tokens; this one signs SAML protocol messages. It is an RSA key in DER form, base64-encoded:

```sh
openssl genpkey -algorithm rsa -pkeyopt rsa_keygen_bits:2048 -outform DER | base64 -w0
```

Add the output to `.env` along with the enable flag:

```sh
GOTRUE_SAML_ENABLED=true
GOTRUE_SAML_PRIVATE_KEY=<the base64 blob from above>
```

> **Persist it once.** The key lives in `.env`, which the compose stack feeds into GoTrue's process env, so it survives a `docker compose down && up` / container recreate. **Regenerating the key invalidates the registered provider** (GoTrue's SP signing identity changes), so generate it once and keep it — treat it like the rest of your signing material.
>
> With `GOTRUE_SAML_ENABLED=true`, GoTrue **requires** `GOTRUE_SAML_PRIVATE_KEY` and will refuse to boot without a valid one.

## 2. Create the Entra enterprise application

In the [Entra admin center](https://entra.microsoft.com) → **Enterprise applications** → **New application** → **Create your own application** → "Integrate any other application you don't find in the gallery" (Non-gallery).

Name it (e.g. "Triage Factory"), then open **Single sign-on** → **SAML**.

### Basic SAML Configuration

Substitute your deployment's public URL for `{TF_PUBLIC_URL}` (the value of `TF_PUBLIC_URL` in `.env`, e.g. `https://triagefactory.yourcompany.com`):

| Entra field | Value |
| --- | --- |
| **Identifier (Entity ID)** | `{TF_PUBLIC_URL}/auth/v1/sso/saml/metadata` |
| **Reply URL (Assertion Consumer Service URL)** | `{TF_PUBLIC_URL}/auth/v1/sso/saml/acs` |
| **Sign on URL** | `{TF_PUBLIC_URL}/sso` — TF's SP-initiated start endpoint (path finalized by TFAC-426) |

The **Identifier** and **Reply URL** are GoTrue's SAML SP endpoints — these are fixed by GoTrue and are what make the federation work. The **Sign on URL** is what the My Apps tile links to: setting it makes the tile do an SP-initiated redirect into TF (rather than an IdP-initiated assertion), which is the secure flow described above.

Leave Relay State and Logout Url blank.

## 3. Hand TF the federation metadata URL

Still on the SAML SSO page, find the **SAML Certificates** section → **App Federation Metadata Url**. Copy it (it looks like `https://login.microsoftonline.com/<tenant-id>/federationmetadata/2007-06/federationmetadata.xml?appid=<app-id>`).

Set it in `.env`, along with your org's email domain(s):

```sh
TF_SAML_ENTRA_METADATA_URL=https://login.microsoftonline.com/<tenant-id>/federationmetadata/2007-06/federationmetadata.xml?appid=<app-id>
TF_SAML_ENTRA_DOMAINS=yourcompany.com
```

- `TF_SAML_ENTRA_METADATA_URL` is what GoTrue fetches server-side. Setting it is also the switch that tells TF to register the provider at startup.
- `TF_SAML_ENTRA_DOMAINS` is the org email domain (comma-separated if more than one). It's the key TF matches on to keep registration idempotent across restarts.

## 4. Assign the test user

Entra only issues assertions for users assigned to the app. In the enterprise app → **Users and groups** → **Add user/group** → pick your test user (and later, the group of everyone who should have access).

The test user's Entra email **must be a verified email** — account merge with an existing GitHub-OAuth login (a later ticket) keys on verified-email match, and JIT provisioning relies on the email Entra returns.

## 5. Bring up the stack and confirm registration

```sh
docker compose up -d
```

(Use `up -d`, not `start` — a `start` reuses the container's cached env and won't pick up the new `.env` values. See the [self-host setup](self-host-setup.md) note on this foot-gun.)

On boot, TF registers the provider with GoTrue. Confirm it in the TF container logs:

```sh
docker compose logs triagefactory | grep -i saml
# registered Entra SAML provider with GoTrue  provider_id=<uuid> domains=[yourcompany.com]
```

On every subsequent restart you'll instead see:

```
Entra SAML provider already registered; left untouched (idempotent)  provider_id=<uuid> ...
```

You can also list it directly from GoTrue's admin API (the `provider_id` above is the `id`):

```sh
# Mint a short-lived service_role token (same kind TF uses) and list providers.
# Substitute your GOTRUE_JWT_SECRET from .env.
docker compose exec gotrue sh -c 'wget -qO- --header="Authorization: Bearer $SERVICE_TOKEN" http://localhost:9999/admin/sso/providers'
```

> **The `provider_id` is the bridge to TF's authorization tables.** TF owns the org/role binding for an SSO login in its own `sso_connections` table (a later ticket), and that row references this GoTrue `provider_id`. It's logged on every boot and always re-derivable from the admin API by domain, so you don't have to record it by hand — but it's the value the login-flow wiring keys on.

## What's next

This page gets GoTrue SAML-ready and the provider registered. Still to come (separate tickets, not configured here):

- **The `/sso` start endpoint + callback** that drives the SP-initiated flow and consumes the assertion (TFAC-426).
- **JIT provisioning + account merge** — a first SSO login mints a `member` membership; a returning GitHub user with the same verified email merges to one account (TFAC-426).
- **The "Sign in with SSO" button** on the login page (TFAC-427).
- **Multi-tenant SAML** (multiple IdPs, email-domain routing, domain verification) is explicitly out of scope here — that's TFAC-78.

## Troubleshooting

- **`docker compose logs triagefactory | grep -i saml` shows a registration WARN, not an INFO.** TF couldn't register the provider and logged why; it never aborts boot over this. Common causes:
  - *`GOTRUE_JWT_SECRET is unset`* — the TF service needs it to authenticate to GoTrue's admin API. It's the same value `jwk-init` wrote; make sure `.env` still has it.
  - *A non-2xx from GoTrue mentioning metadata* — GoTrue couldn't fetch `TF_SAML_ENTRA_METADATA_URL`. Check the gotrue container has **outbound** access to `login.microsoftonline.com`, and that the URL is the *App Federation Metadata Url* (not the raw IdP metadata of a different app).
  - *`TF_SAML_ENTRA_DOMAINS is empty`* — set the org domain; it's required to register.
- **GoTrue won't boot after enabling SAML.** `GOTRUE_SAML_ENABLED=true` requires a valid `GOTRUE_SAML_PRIVATE_KEY`. Re-generate per [step 1](#1-generate-the-saml-request-signing-key) and recreate the container with `docker compose up -d gotrue`.
- **I rotated the SAML key and SSO broke.** Regenerating `GOTRUE_SAML_PRIVATE_KEY` changes GoTrue's SP signing identity and invalidates the existing provider registration. Keep the key stable; if you must rotate, expect to re-establish the Entra-side trust.
- **I changed the Entra metadata / domains and nothing updated.** Registration is **never-overwrite** by design — TF won't mutate an existing provider. Remove the stale provider from GoTrue's admin API first, then restart TF to re-register from the current env. (An in-app management affordance is a future ticket.)
