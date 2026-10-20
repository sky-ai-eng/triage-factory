# SSO end-to-end suite

Signs in to a real multi-mode stack the way people do: through GoTrue, two
SAML identity providers and GitHub (served by
[Orrery](https://github.com/sky-ai-eng/orrery)), in Chromium. The Go tests in `ee/sso` fake
GoTrue in-process and hand-build its tokens; this suite is what runs the real
thing.

## What it proves

Against Postgres, GoTrue and SeaweedFS from the repository's
`docker-compose.yml`, with the control half of the binary running on the host:

- GoTrue registers an identity provider from its metadata URL over TLS, sends a
  signed AuthnRequest, accepts the assertion at its ACS, and completes TF's PKCE
  exchange; TF verifies the token GoTrue issues for a SAML sign-in.
- The `tf_oauth_state` cookie survives the round trip from the identity
  provider back through the ACS to `/api/auth/callback` in a real browser.
- The licence path (`TF_LICENSE` through `ee.Install`) unlocks SSO and the fleet
  console.
- The SPA works against the live backend: the login page, identifier-first
  sign-in, invite acceptance, onboarding and the single-sign-on interstitial.

And it runs the attack and enforcement cases, in order, in one stack:

| # | Scenario |
| - | -------- |
| 1 | An admin connects Globex's IdP, tests it (the pass page and its "Matches existing accounts" row, before and after `globex.test` is verified) and claims the domain. |
| 2 | Identifier-first sign-in: typing `alice@globex.test` on `/login` goes to Globex's IdP and back, and provisions Alice into Globex. |
| 3 | Bob, a member through GitHub, signs in through Globex SSO and gets the same account, with two sign-in methods. |
| 4 | Require SSO turns away a GitHub sign-in on `globex.test`; the owner, break-glass, still gets in. |
| 5 | Mallory's IdP asserting `bob@globex.test` gets a new account in Mallory's org only; Bob's account is untouched. |
| 6 | Mallory's IdP asserting the operator's address does not make anyone an operator; the operator's GitHub sign-in does. |
| 7 | An invite to `carol@globex.test` cannot be accepted by Mallory's IdP asserting that address. |
| 8 | A member with an off-domain GitHub address gets `SSO_REQUIRED` in Globex, and the SPA shows the single-sign-on interstitial. |
| 9 | After Bob's GitHub address moves off the domain, his GitHub sign-in succeeds and reaches only the orgs that do not require SSO. |
| 10 | Requiring SSO closes the off-domain member's open websocket with 4002. |
| 11 | A token minted from an SSO session works in Globex; one minted from a GitHub session before Require SSO does not. |

Scenario 1 configures the connection through `/api/sso/*`, the routes the
settings screen calls, because the SPA admits an admin to Settings only once
the org's setup is complete, which takes a GitHub credential, models and a
repository. The test round trip itself runs in the browser.

Two things are done outside the product's own flows: a claimed domain is marked
verified in Postgres, because `.test` names cannot serve DNS TXT records (the
TXT check is covered by `ee/sso`'s Postgres tests), and Bob's GitHub address is
changed through Orrery's control plane, as GitHub has no API for it.

## The test servers

`fakes/` is a Go program in its own module (`e2e/go.mod`), so its dependencies
stay out of the product's. It serves `idp-globex.test:8443` and
`idp-mallory.test:8444` over HTTPS on every interface: two
[crewjam/saml](https://github.com/crewjam/saml) identity providers with the
users in `fixtures/users.json`.

GitHub is Orrery, built from a checkout and served at `github.test:8445` over
HTTP. `run.sh` creates one Orrery actor per GitHub user in `users.json`, gives
each its address, and registers the OAuth app GoTrue's GitHub provider signs in
through. Orrery's authorize page signs an actor in by username alone.

GoTrue reaches all three from its container through the host gateway
(`compose.e2e.yml`) and trusts the CA the identity providers' certificates come
from; Chromium maps `*.test` to loopback and ignores certificate errors.

`fakes license` mints an Enterprise Edition licence signed by a key made for
the run. `run.sh` builds the binary with that key's public half as
`ee.publicKeyB64`, so the test key exists only in that build and the run's state
directory.

## Running it

Needs Docker, Go, Rust, Node 22, pnpm, `jq`, and a checkout of
[sky-ai-eng/orrery](https://github.com/sky-ai-eng/orrery) (private), by default
at `../orrery` beside this repository. Ports 3000, 5432, 8333, 8443, 8444, 8445
and 9999 must be free; stop a running `scripts/multi-mode-dev.sh` stack first.

```bash
e2e/sso/run.sh                            # build, bring up, test, tear down
ORRERY_DIR=~/src/orrery e2e/sso/run.sh    # Orrery checked out elsewhere
SSO_E2E_SKIP_FRONTEND=1 e2e/sso/run.sh    # reuse an existing frontend/dist
SSO_E2E_KEEP_UP=1 e2e/sso/run.sh          # leave the stack up afterwards
```

The first run needs Playwright's Chromium:
`cd e2e/sso && pnpm install && pnpm exec playwright install chromium`.

Everything the run generates (certificates, the licence and its key, the SAML
signing key, the env file, the binary, Orrery's world and the logs `tf.log`,
`fakes.log`, `orrery.log` and `gotrue.log`) is written to `e2e/sso/.state/`,
which is gitignored and replaced on every run. The stack runs as the
`tf-multi-dev` compose project, the same one `scripts/multi-mode-dev.sh` uses.

To rerun the tests against a stack left up with `SSO_E2E_KEEP_UP=1`, run
`pnpm exec playwright test` in `e2e/sso`. The scenarios build on each other, so
they pass only against a fresh stack.

CI runs the suite in `.github/workflows/sso-e2e.yml` and uploads the report and
the four logs when it fails. It checks Orrery out with the `ORRERY_TOKEN`
secret; a pull request from a fork has no secrets, so the job skips the suite
with a notice, and the suite runs on `main` after the merge.
