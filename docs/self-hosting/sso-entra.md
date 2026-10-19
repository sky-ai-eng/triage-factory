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
> [self-host setup](install.md) first. GitHub OAuth remains the
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
it for you (signed with the same RSA key as `GOTRUE_JWT_KEYS`) and writes it to
`.env`; the compose stack passes it to the `triagefactory` service. No extra
step beyond running `jwk-init` — just don't strip it.

> **Enabling SSO on an already-running deployment (no key rotation).** If you
> bootstrapped on GitHub-OAuth alone, your `.env` has a `GOTRUE_JWT_KEYS` but no
> `TF_GOTRUE_SERVICE_ROLE_TOKEN`. To acquire the token, just re-run
> `triagefactory jwk-init --write-env .env`. The command is **idempotent**: it
> detects the existing `GOTRUE_JWT_KEYS`, **reuses that keypair** to sign the
> token, and writes **only** the `TF_GOTRUE_SERVICE_ROLE_TOKEN` line — it does
> **not** rotate the signing key, so every active session keeps verifying and no
> one is forced to re-authenticate. Then recreate the `triagefactory` service
> (`docker compose up -d triagefactory`) to load the new token. (Pass `--rotate`
> only if you actually want to roll the signing key — that *does* force a re-auth
> for everyone; see [Rotating the signing key](key-rotation.md).)

> **Why a pre-minted token, not TF signing its own?** Under the production
> `GOTRUE_JWT_ALGORITHM=RS256` config, GoTrue **rejects** an HS256 token signed
> with `GOTRUE_JWT_SECRET` ("signing method HS256 is invalid") and accepts only
> an RS256 token carrying `role:service_role`. Signing that requires GoTrue's
> private key — which must live in exactly one process. So `jwk-init` (which
> already holds the freshly generated key) mints the token, and TF holds **only
> the token**, never the key, and contains no JWT-minting code. Re-running
> `jwk-init` reuses the existing keypair and re-mints this token against it;
> rotating the keypair (`--rotate`) regenerates the token with the new key.
> `aud`/`iss` aren't enforced by GoTrue on the admin token but are stamped for
> hygiene.
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
won't pick up new `.env` values. See the [self-host setup](install.md)
note on this foot-gun.)

> **Egress: the gotrue container needs outbound HTTPS to
> `login.microsoftonline.com`.** GoTrue fetches each org's Entra federation
> metadata server-side (step 2.5), so provider registration fails if that host
> is unreachable. If you run a default-deny egress policy — likely, given Triage
> Factory's per-run allowlist posture — add `login.microsoftonline.com` to the
> gotrue service's allowed destinations. This is the one external dependency the
> SSO path introduces.

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
| **Sign on URL** | *leave blank for now* — you'll set it in step 2.5 once you have the `provider_id` |

The **Identifier** and **Reply URL** are GoTrue's SAML SP endpoints — fixed, and
what make the federation work. Save this card with just those two; the **Sign on
URL** depends on a `provider_id` that doesn't exist until you register the
connection in Triage Factory, so Entra is a two-pass job: fill in the identifier
and reply URL now, then return in [step 2.5](#25-register-the-connection-in-triage-factory)
to set the Sign on URL.

The Sign on URL, once set, is what the My Apps tile links to:
`{TF_PUBLIC_URL}/api/auth/oauth/saml?provider_id=<your-provider_id>`. Setting it
makes the tile do an **SP-initiated** redirect into Triage Factory rather than
firing an unsolicited IdP-initiated assertion. SP-initiated binds the assertion
to a request TF made in *this* browser session, which closes the login-CSRF hole
that true IdP-initiated SAML leaves open — same one-click tile UX, safer flow.

> **You'll fill in `<your-provider_id>` after step 2.5** — it's the `provider_id`
> the registration call returns (also shown by `GET /api/sso/connection`). The
> start endpoint validates it against your org's connection and rejects an
> unknown or disabled one, so TF only ever initiates a flow for a connection you
> registered. (A forthcoming identifier-first login will also let users reach
> this endpoint by typing their email on the login screen — the bookmark tile is
> the zero-typing path.)

Leave Relay State and Logout Url blank.

### Attributes & Claims — the email claim

This is the step that most often gets missed, and it's the single most common
cause of a login that authenticates but then fails or lands the user in
onboarding: **Triage Factory's just-in-time provisioning keys off the email
Entra returns in the assertion, so the assertion must carry a real email.**

On the SAML SSO page, open the **Attributes & Claims** card → **Edit**. Confirm:

- There is an email claim — Entra ships one by default at
  `http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress`.
- Its **Source attribute is `user.mail`**, not `user.userprincipalname`. A UPN
  (`alice@contoso.onmicrosoft.com`) is a directory identifier, not necessarily a
  deliverable mailbox; JIT will create the account under whatever this claim
  carries.
- Your test user **actually has the `mail` attribute populated** in Entra
  (Entra admin center → **Users** → the user → **Contact info** → Email).
  Cloud-only and guest/external accounts created without a mailbox often have
  `mail` blank — the claim then comes through empty, and the login has no email
  to provision with. Set the user's email (or pick a test user who has one).

The **NameID** can stay at Entra's default (UPN / emailAddress format); it's the
emailaddress claim above that GoTrue reads for the account email.

## 2.3 Copy the federation metadata URL

Still on the SAML SSO page, find the **SAML Certificates** card → **App
Federation Metadata Url**. Copy it (it looks like
`https://login.microsoftonline.com/<tenant-id>/federationmetadata/2007-06/federationmetadata.xml?appid=<app-id>`).
You do **not** download or upload the signing certificate by hand — GoTrue
fetches this metadata URL server-side and reads Entra's entityID, SSO URL, and
signing certificate out of it.

## 2.4 Assign users

Entra only issues assertions for users assigned to the app. In the enterprise
app → **Users and groups** → **Add user/group** → add your test user (and later,
the group of everyone who should have access).

A user's Entra email **must be a real, verified email** — just-in-time
provisioning uses the email Entra returns in the assertion to create the user's
account.

An SSO sign-in joins an **existing** Triage Factory account (for example, a
colleague who already signs in with GitHub at the same address) only when the
email's domain is **verified for your org**: claimed with `POST /api/sso/domains`
and verified through its DNS TXT record with
`POST /api/sso/domains/{id}/verify`. Until the domain is verified, every SSO
sign-in gets a separate account that belongs to your org only. See
[Accounts and email addresses](#accounts-and-email-addresses).

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

## 2.6 Test the connection before enabling it

The common SSO failures are Entra-side — the wrong Identifier/Reply URL pasted in,
a certificate mismatch, the test user not assigned, or the assertion missing the
email claim — and none of them show up in metadata validation. They only surface
in a **live round-trip**, so verify one before you flip the connection on (you can
test a connection that is still disabled — that's the point):

- In the Configure-SSO admin screen, click **Test SSO**. It opens a real sign-in
  in a new window and renders a **pass/fail** page with the authenticated email
  and a **Matches existing accounts** row. *Yes* means the email's domain is
  verified for your organization, so a real sign-in with that address joins the
  account that already holds it. *No* means it is not, so a sign-in with that
  address gets a separate account in your organization only. The row is
  informational: the test passes either way.
- Under the hood this is the normal SP-initiated flow with a test flag in TF's
  signed state. The callback completes the GoTrue code exchange and verifies the
  returned token — proving the assertion is valid and readable — then **stops
  before any writes on the Triage Factory side**: a test creates **no session, no
  org membership, no user record, and no identity link**. Your current admin
  session is untouched, so it's safe to run anytime.
- One thing a test does leave behind: GoTrue (the auth service) records the
  sign-in as a login identity (an `auth.users` row), the same as it would for any
  assertion it processes. That's expected and inert — Triage Factory binds nothing
  to it, and the test's tokens never leave the server — so it's a harmless orphan
  that a later **real** sign-in by the same verified email simply adopts. There's
  nothing to clean up.

```http
GET /api/sso/connection/test      # org-admin only; opens in a popup
```

A failed test shows an actionable reason (e.g. *"the assertion carried no email
claim — map the user's email/UPN to the email claim"*). Fix the Entra side, retest,
and only then `PATCH { "enabled": true }`.

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

TF writes no GitHub identity row for a SAML login.

### Accounts and email addresses

GoTrue keeps every SAML login as a login identity of its own and never links it
to a GitHub login, so Triage Factory decides which account each sign-in belongs
to, by email:

- **An SSO sign-in's email is trusted only on a verified domain.** Its domain
  must be verified for the org whose connection the sign-in came through. GoTrue
  marks every SAML assertion's email as verified, and an identity provider
  asserts whatever address it is configured to, so the verified domain is the
  only proof that the address belongs to the person signing in. A domain that a
  different org verified does not count.
- **A GitHub sign-in's email is trusted when GitHub reports it verified.**
- **A trusted address always joins the account that holds it.** Someone who
  signs in with GitHub and later through your connection at the same address is
  one account with two sign-in methods. There is no setting to turn this off.
- **An untrusted address gets a separate account.** That account is
  provisioned into your org only. Its address does not count anywhere Triage
  Factory matches a person by verified email: joining other accounts, accepting
  invitations, the fleet console's operator list, Slack sender resolution, and
  break-glass designation.
- **One verified address belongs to at most one account**, and the database
  enforces it. If a sign-in method later asserts a trusted address that another
  account already holds, the sign-in still succeeds, and the address stays with
  the account that held it first.

**Known limit: address reuse.** If your directory gives an address on a
verified domain to a different person, that person's first sign-in joins the
previous holder's account. Triage Factory cannot tell the two people apart
without directory provisioning (SCIM), which it does not support yet.

## Requiring SSO

**Require SSO** (`PATCH /api/sso/connection/enforcement` with
`{"enforced": true}`, or the switch under **Settings → Single sign-on**) means
nobody uses your org except through your identity provider. It is two rules:

- **Sign-in.** A GitHub sign-in whose trusted email is on one of your verified
  domains is refused, and the person is sent back to the login page to sign in
  with SSO.
- **Org access.** Every request that acts in your org must come from a session
  or API token that was signed in through your org's own connection. That covers
  every member, whatever their email: a contractor invited with a personal
  GitHub account, and a member who changed their GitHub email, both lose access
  until they sign in through your identity provider. It applies to sessions and
  tokens that existed before you turned the requirement on, and it is checked on
  every request.

**Break-glass** principals are exempt from both rules. They are your recovery
path if the identity provider is unavailable. The first time you require SSO,
the org owner is added as break-glass if the list is empty.

What a member who is not signed in through your connection sees:

- Requests to the org answer `403` with reason `SSO_REQUIRED`. `GET /api/me`
  still answers, and lists the org with `sso_required: true` and the
  `sso_start_url` that begins your org's sign-in. The app shows that sign-in in
  place of the org's pages.
- They can switch to another org they belong to, log out, or sign in through
  your identity provider. A GitHub sign-in no longer lands them in your org by
  default.
- They cannot switch into your org, mint an API token for it, or accept an
  invitation to it. A refused invitation stays pending.
- An open websocket to your org is closed with code `4002` on the next
  revalidation sweep, every `TF_WS_SESSION_REVALIDATE_SECONDS` seconds (60 by
  default).

API tokens carry the sign-in of the credential that minted them: a token
minted from a session signed in through your connection keeps working, and one
minted from a GitHub session does not, whenever it was minted. Members who use
tokens for automation should mint new ones after signing in with SSO.

Turning the requirement on is refused with `409` until each of these holds:

| Refused when | Message |
| ------------ | ------- |
| The connection is disabled | "enable the connection before requiring SSO" |
| The connection has not passed a Test | "run a successful Test before requiring SSO" |
| No domain is verified | "verify a domain before requiring SSO" |
| The admin turning it on is neither signed in through the connection nor break-glass | "Sign in through single sign-on, or be designated break-glass, before requiring it." |

While the requirement is on, these changes are refused with `409`, because each
would leave members with no working sign-in or the org with no recovery path:

| Change | Message |
| ------ | ------- |
| Disabling the connection | "Turn off Require SSO first." |
| Removing your last verified domain | "Turn off Require SSO first." |
| Removing your last break-glass principal | "can't remove the last break-glass principal while SSO is required — stop requiring SSO first" |

Each of these checks is made under a lock on the connection, so two changes
made at the same moment cannot both pass against the state before the other.

If your SSO entitlement lapses, both rules stop applying, so a lapsed licence
can never lock everyone out.

A refused sign-in is recorded in `auth_events` as `sso_enforcement_rejected`,
and a break-glass sign-in that bypassed it as `break_glass_login`. A refused
request is not recorded, since that would be one row per request; turning the
requirement on and off is in `access_change_log` (see below).

**Known limits.**

- A session or token signed in through your connection keeps working until it
  expires (sessions last 30 days), your org's API token age cap ends it, or an
  admin removes the member. Disabling someone in your identity provider does not
  end their existing Triage Factory session; remove them from the org's People
  roster. Directory provisioning (SCIM) is not supported yet.
- Background work is not affected: delegated runs, pollers and webhooks do not
  pass through these checks, and runs started before you required SSO continue.

## What's next

Still in progress (not configured here yet):

- **Identifier-first login + a "Sign in with SSO" button** — so users reach the
  start endpoint by typing their email, not just via the tile.
- **A "Configure SSO" admin UI** for the steps in Part 2.

## Auditing SSO changes

Every admin action on this surface is recorded to `access_change_log` and shows
up under **Policy** in the Usage page's "Access & credential changes" band (org
admin + the `governance` entitlement):

| Action                          | Recorded as                                          |
| ------------------------------- | ---------------------------------------------------- |
| Register a connection           | `sso_connection_created`                             |
| Enable / disable the connection | `sso_connection_enabled` / `sso_connection_disabled` |
| Start / stop requiring SSO      | `sso_enforcement_enabled` / `sso_enforcement_disabled` |
| Claim / verify / remove a domain | `sso_domain_claimed` / `_verified` / `_removed`      |
| Add / remove a break-glass principal | `sso_break_glass_added` / `sso_break_glass_removed` |

Each row is written in the same transaction as the change it describes, so the
log can't drift from reality — the same contract JIT provisioning follows above.
A save that changes nothing (re-enabling an already-enabled connection, a
re-claim of a domain you already hold) writes no row, so the log counts real
transitions rather than button presses.

## Troubleshooting

- **`POST /api/sso/connection` returns 503 "SSO admin token not configured".**
  `TF_GOTRUE_SERVICE_ROLE_TOKEN` isn't set on the `triagefactory` service. Run
  `triagefactory jwk-init --write-env .env` — it reuses your existing
  `GOTRUE_JWT_KEYS` to mint the token (no keypair rotation, no forced re-auth)
  and writes only the token line — then recreate the container with
  `docker compose up -d triagefactory` so it picks up the new env.
- **`POST /api/sso/connection` returns 500 mentioning GoTrue.** Common causes:
  - *Provider registration failed with a metadata error* — GoTrue couldn't
    fetch the metadata URL. Check the gotrue container has **outbound** access
    to `login.microsoftonline.com`, and that you pasted the *App Federation
    Metadata Url* (not the raw IdP metadata of a different app).
  - *403 from GoTrue's admin API* — the `TF_GOTRUE_SERVICE_ROLE_TOKEN` doesn't
    match GoTrue's current signing key (its `kid` must resolve in
    `GOTRUE_JWT_KEYS`). This happens if the keypair was rotated without
    re-minting the token: re-run `triagefactory jwk-init --write-env .env`, which
    reuses the current `GOTRUE_JWT_KEYS` to re-mint the token against it (it
    upserts the line in place — no stale duplicates to hand-delete), then
    recreate the `triagefactory` container.
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
- **An SSO sign-in fails at the callback with a 500 and the user isn't logged
  in.** JIT provisioning grants the membership and writes its `access_change_log`
  audit row in **one** admin-pool transaction — the audit can't diverge from the
  grant (a SOC2 authorization-change control, deliberately not best-effort). If
  that transaction can't commit because the database is unhealthy (disk full, in
  recovery/read-only, `access_change_log` schema drift), the grant rolls back and
  the callback fails **closed**: a 500, no session, nothing half-provisioned.
  Check the `triagefactory` logs for `jit: grant membership failed` /
  `audit jit member granted` and the health of the database. The user simply
  retries once it's restored (the grant is idempotent); there is no break-glass
  that grants membership while the audit write is down — by design.
- **An SSO user authenticates fine but lands in onboarding ("Start your
  Factory"), not in their org.** Their login resolved no org to join, so the
  session was minted with a NULL active org. The usual cause is the connection
  being **disabled** — you ran the verify-before-enable test
  ([step 2.6](#26-test-the-connection-before-enabling-it)) but never flipped it on
  — or the `provider_id` → org binding being gone. The sign-in itself succeeds (no
  error), which is why it's quiet. Enable the connection; the user's next login
  JIT-provisions them. Enable it **before** pointing users at SSO: with org
  creation on, a user who lands here could create a duplicate personal org from
  that screen.
