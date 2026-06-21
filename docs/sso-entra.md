# SSO with Microsoft Entra (SAML)

Operator + org-admin guide for wiring Triage Factory to **Microsoft Entra ID**
(formerly Azure AD) for single sign-on. Users sign in by clicking the Triage
Factory tile in Microsoft My Apps.

Triage Factory is **one multi-org build** — self-host and SaaS run the identical
image. So SSO connections are configured **per org, at runtime**: an org admin
registers their IdP by pasting Entra's metadata URL into Triage Factory; there
is **no env-driven / startup-bootstrap SSO**. This guide has two halves:

1. **Operator (deployment) setup** — enable SAML on the one GoTrue (env), done
   once per deployment.
2. **Org-admin setup** — create the Entra enterprise app and register the
   connection in Triage Factory, done once per org.

> **Prerequisites.** SSO is **multi-mode only** (`TF_MODE=multi`) — it runs
> through GoTrue, which doesn't exist in local mode. Do the
> [self-host setup](self-host-setup.md) first. GitHub OAuth remains the
> bootstrap-floor login: a fresh deployment has no SSO, so the first admin signs
> in with GitHub and configures SSO for everyone else.

> **What this page covers.** Enabling GoTrue SAML, registering an org's
> connection (the provider + the org↔role binding), and the SP-initiated login
> flow with just-in-time provisioning. **Email-domain routing** — so users can
> sign in by typing their email instead of clicking the tile — is a follow-up
> still in progress; until it lands, the My Apps tile (the Sign on URL below) is
> the way in.

## How it fits together

```
Entra enterprise app  ──(SAML federation)──►  GoTrue  ◄──(admin API)──  Triage Factory
   (IdP: entityID,                            (SP: ACS,                  (org admin registers
    SSO URL, signing cert)                     metadata, /sso)            the provider at runtime)
```

- **GoTrue is the SAML Service Provider (SP).** It exposes the SP metadata +
  Assertion Consumer Service (ACS) endpoints Entra posts assertions to, and it
  signs outbound SAML AuthnRequests with an RSA key you generate.
- **Triage Factory owns authorization.** GoTrue owns authN + the provider
  registry; TF stores the org and the default role a first-time SSO user gets in
  its own `sso_connections` table. The GoTrue `provider_id` is the single
  bridge, and it's globally unique — bound to exactly one org — which is the
  cross-org isolation boundary.
- **GoTrue fetches Entra's metadata server-side.** The org admin hands TF the
  *App Federation Metadata URL*; GoTrue itself fetches it to learn Entra's
  entityID, SSO URL, and signing certificate. **The GoTrue container therefore
  needs outbound network access to `login.microsoftonline.com`.**

---

# Part 1 — Operator (deployment) setup

Done once per deployment. Enables SAML on the one GoTrue.

## 1.1 Generate the SAML request-signing key

GoTrue signs its outbound SAML AuthnRequests with an RSA private key. **This is a
different key from the GoTrue session-JWT signing material** (`jwk-init` /
`GOTRUE_JWT_KEYS`) — that one signs user session tokens; this one signs SAML
protocol messages. It is an RSA key in DER form, base64-encoded:

```sh
openssl genpkey -algorithm rsa -pkeyopt rsa_keygen_bits:2048 -outform DER | base64 -w0
```

> **⚠️ RSA, not ES256.** GoTrue's SAML signing requires an RSA key; an
> EC/ES256 key is rejected.

Add the output to `.env`:

```sh
GOTRUE_SAML_ENABLED=true
GOTRUE_SAML_PRIVATE_KEY=<the base64 blob from above>
```

> **Persist it once.** The key lives in `.env`, which the compose stack feeds
> into GoTrue's process env, so it survives a `docker compose down && up` /
> container recreate. **Regenerating the key invalidates every registered
> provider** (GoTrue's SP signing identity changes), so generate it once and
> keep it — treat it like the rest of your signing material.
>
> **SSO is off by default.** `GOTRUE_SAML_ENABLED` defaults to `false` in the
> compose stack, so a fresh deployment boots on GitHub-OAuth login alone — the
> bootstrap floor — with no SAML key required. Enabling SSO is the deliberate
> opt-in above: set the flag to `true` **and** provide the key. GoTrue gates the
> whole SAML feature on this flag (including the `/admin/sso/providers` admin API
> TF registers providers through); it does **not** infer enablement from the
> key's presence, so the key alone does nothing. With the flag on, GoTrue
> **requires** a valid key and refuses to boot without one.

## 1.2 The service-role admin token

When an org admin registers a connection, TF calls GoTrue's admin SSO API
authenticated with a **pre-minted RS256 `service_role` token**,
`TF_GOTRUE_SERVICE_ROLE_TOKEN`. `triagefactory jwk-init --write-env .env` mints
it for you (signed with the same RSA key as `GOTRUE_JWT_KEYS`) and appends it to
`.env`; the compose stack passes it to the `triagefactory` service. No extra
step beyond running `jwk-init` — just don't strip it.

> **Why a pre-minted token, not TF signing its own?** Under the production
> `GOTRUE_JWT_ALGORITHM=RS256` config, GoTrue **rejects** an HS256 token signed
> with `GOTRUE_JWT_SECRET` ("signing method HS256 is invalid") and accepts only
> an RS256 token carrying `role:service_role`. Signing that requires GoTrue's
> private key — which must live in exactly one process. So `jwk-init` (which
> already holds the freshly generated key) mints the token, and TF holds **only
> the token**, never the key, and contains no JWT-minting code. Rotating the
> keypair (re-running `jwk-init`) regenerates this token with it. `aud`/`iss`
> aren't enforced by GoTrue on the admin token but are stamped for hygiene.
>
> The token is **optional but required for SSO**: a deployment that never
> registers an SSO connection needn't set it. If it's missing when an admin
> tries to register, TF returns a clear "SSO admin token not configured — run
> `triagefactory jwk-init`" error rather than a generic failure.

## 1.3 Bring up the stack

```sh
docker compose up -d
```

(Use `up -d`, not `start` — a `start` reuses the container's cached env and
won't pick up new `.env` values. See the [self-host setup](self-host-setup.md)
note on this foot-gun.)

---

# Part 2 — Org-admin setup

Done once per org by an org admin. Connect an Entra tenant and register it in
Triage Factory.

## 2.1 Find your SP values

Triage Factory's SP endpoints (GoTrue's, fixed by the deployment's public URL)
are what you paste into Entra. Substitute your deployment's `TF_PUBLIC_URL`
(e.g. `https://triagefactory.yourcompany.com`):

| Purpose | Value |
| --- | --- |
| **Entity ID** (SP identifier) | `{TF_PUBLIC_URL}/auth/v1/sso/saml/metadata` |
| **ACS** (Assertion Consumer Service) | `{TF_PUBLIC_URL}/auth/v1/sso/saml/acs` |

Triage Factory also returns these to you directly — `GET /api/sso/connection`
includes `entity_id` and `acs_url` in its response (and a forthcoming "Configure
SSO" admin screen surfaces them for copy-paste), so you never have to
hand-assemble them.

## 2.2 Create the Entra enterprise application

In the [Entra admin center](https://entra.microsoft.com) → **Enterprise
applications** → **New application** → **Create your own application** →
"Integrate any other application you don't find in the gallery" (Non-gallery).

Name it (e.g. "Triage Factory"), then open **Single sign-on** → **SAML**.

### Basic SAML Configuration

| Entra field | Value |
| --- | --- |
| **Identifier (Entity ID)** | `{TF_PUBLIC_URL}/auth/v1/sso/saml/metadata` |
| **Reply URL (Assertion Consumer Service URL)** | `{TF_PUBLIC_URL}/auth/v1/sso/saml/acs` |
| **Sign on URL** | `{TF_PUBLIC_URL}/api/auth/oauth/saml?provider_id=<your-provider_id>` — TF's SP-initiated start endpoint |

The **Identifier** and **Reply URL** are GoTrue's SAML SP endpoints — fixed, and
what make the federation work. The **Sign on URL** is what the My Apps tile
links to: setting it makes the tile do an **SP-initiated** redirect into Triage
Factory rather than firing an unsolicited IdP-initiated assertion. SP-initiated
binds the assertion to a request TF made in *this* browser session, which closes
the login-CSRF hole that true IdP-initiated SAML leaves open — same one-click
tile UX, safer flow.

> **You'll fill in `<your-provider_id>` after step 2.5** — it's the `provider_id`
> the registration call returns (also shown by `GET /api/sso/connection`). The
> start endpoint validates it against your org's connection and rejects an
> unknown or disabled one, so TF only ever initiates a flow for a connection you
> registered. (A forthcoming identifier-first login will also let users reach
> this endpoint by typing their email on the login screen — the bookmark tile is
> the zero-typing path.)

Leave Relay State and Logout Url blank.

## 2.3 Copy the federation metadata URL

Still on the SAML SSO page, find **SAML Certificates** → **App Federation
Metadata Url**. Copy it (it looks like
`https://login.microsoftonline.com/<tenant-id>/federationmetadata/2007-06/federationmetadata.xml?appid=<app-id>`).

## 2.4 Assign users

Entra only issues assertions for users assigned to the app. In the enterprise
app → **Users and groups** → **Add user/group** → add your test user (and later,
the group of everyone who should have access).

A user's Entra email **must be a real, verified email** — just-in-time
provisioning uses the email Entra returns in the assertion to create the user's
account.

## 2.5 Register the connection in Triage Factory

Hand the metadata URL from step 2.3 to Triage Factory. A forthcoming "Configure
SSO" admin screen will make this a paste-and-save form; until then, an org admin
POSTs it directly:

```http
POST /api/sso/connection
Content-Type: application/json

{ "metadata_url": "https://login.microsoftonline.com/<tenant-id>/.../federationmetadata.xml?appid=<app-id>" }
```

Triage Factory presents its service-role token, asks GoTrue to register the SAML
provider (GoTrue fetches the metadata server-side), and writes the org↔provider
binding with a default role of `member`. The response carries the new
connection (including its `provider_id`) plus your `entity_id` / `acs_url`:

```jsonc
{
  "connection": {
    "id": "…",
    "kind": "saml",
    "provider_id": "…",       // GoTrue's provider id — the bridge to TF authz
    "default_role": "member", // everyone starts member; promote via the roster
    "enabled": true,
    "created_at": "…",
    "updated_at": "…"
  },
  "entity_id": "{TF_PUBLIC_URL}/auth/v1/sso/saml/metadata",
  "acs_url": "{TF_PUBLIC_URL}/auth/v1/sso/saml/acs"
}
```

The endpoint is **idempotent**: re-registering returns the existing connection
(HTTP 200) rather than minting a second provider. **Enable/disable** the
connection without deleting it:

```http
PATCH /api/sso/connection
{ "enabled": false }
```

Only org admins can reach these endpoints (a non-admin gets a 404), and a
connection is always bound to the caller's own org — so it can never be attached
to an org you don't administer.

## The login flow

Once the connection is registered, the SP-initiated login works end to end:

1. The browser hits the **Sign on URL** above (the My Apps tile, or a direct
   link): `GET {TF_PUBLIC_URL}/api/auth/oauth/saml?provider_id=<provider_id>`.
2. TF validates the `provider_id` against your connection, then server-side POSTs
   to GoTrue's public `/sso` and forwards the 303 to Entra (carrying a PKCE
   challenge + TF's signed state — the `provider_id` rides in *TF's* state, never
   read back from the assertion).
3. Entra authenticates the user and posts the assertion to GoTrue's ACS; GoTrue
   redirects back to TF's callback with a one-time `code`.
4. TF exchanges the `code` for a verified session and, for a **first-time** SSO
   user, **JIT-provisions** them as a `member` of the org that owns the
   `provider_id` — the cross-org isolation boundary. The user lands in the app;
   no onboarding, no manual invite. Promote them later from the org's People
   roster.

A SAML login is always its own account — GoTrue does not link it to a GitHub
login even at a matching email — and TF writes no GitHub identity row for it.

## What's next

Still in progress (not configured here yet):

- **Identifier-first login + a "Sign in with SSO" button** — so users reach the
  start endpoint by typing their email, not just via the tile.
- **Email-domain claim + DNS-TXT verification** — proves your org owns a domain
  and routes that domain's logins to your connection, with verified-domain global
  uniqueness as the cross-org isolation guarantee.
- **A "Configure SSO" admin UI** for the steps in Part 2.

## Troubleshooting

- **`POST /api/sso/connection` returns 503 "SSO admin token not configured".**
  `TF_GOTRUE_SERVICE_ROLE_TOKEN` isn't set on the `triagefactory` service. Run
  `triagefactory jwk-init --write-env .env` (it appends the token) and recreate
  the container with `docker compose up -d` so it picks up the new env.
- **`POST /api/sso/connection` returns 500 mentioning GoTrue.** Common causes:
  - *Provider registration failed with a metadata error* — GoTrue couldn't
    fetch the metadata URL. Check the gotrue container has **outbound** access
    to `login.microsoftonline.com`, and that you pasted the *App Federation
    Metadata Url* (not the raw IdP metadata of a different app).
  - *403 from GoTrue's admin API* — the `TF_GOTRUE_SERVICE_ROLE_TOKEN` doesn't
    match GoTrue's current signing key (its `kid` must resolve in
    `GOTRUE_JWT_KEYS`). This happens if the keypair was rotated without
    regenerating the token: re-run `triagefactory jwk-init` so both are minted
    together, then recreate the `gotrue` and `triagefactory` containers. Note
    `--write-env` *appends* — delete the old `GOTRUE_JWT_KEYS` /
    `GOTRUE_JWT_SECRET` / `TF_GOTRUE_SERVICE_ROLE_TOKEN` lines so the new ones
    win (env files take the last value, but stale duplicates are confusing).
- **`POST /api/sso/connection` returns 404.** You're not an org admin in the
  active org (the endpoint is non-disclosing — it 404s rather than 403s), or the
  deployment is in local mode (SSO is multi-mode only).
- **GoTrue won't boot after enabling SAML.** `GOTRUE_SAML_ENABLED=true` requires
  a valid `GOTRUE_SAML_PRIVATE_KEY`. Re-generate per
  [step 1.1](#11-generate-the-saml-request-signing-key) and recreate the
  container with `docker compose up -d gotrue`.
- **I rotated the SAML key and SSO broke.** Regenerating
  `GOTRUE_SAML_PRIVATE_KEY` changes GoTrue's SP signing identity and invalidates
  the existing provider registration. Keep the key stable; if you must rotate,
  expect to re-establish the Entra-side trust.
- **I changed the Entra metadata and re-POSTed, but nothing updated.**
  Registration is idempotent — a re-POST returns the *existing* connection and
  does not re-fetch metadata. Rotating a connection's metadata is a deferred
  follow-up; for now, removing and re-registering a connection requires operator
  action against GoTrue's admin API.
