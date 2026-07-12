# Rotating the JWT signing key

The current tooling supports **single-key replacement** only. Because the default
`jwk-init --write-env` reuses an existing keypair, rotation is an explicit opt-in
via `--rotate`:

1. `./triagefactory jwk-init --write-env .env --rotate` — regenerates the keypair,
   secret, and service-role token, replacing the old lines in place (no manual
   deletion needed)
2. Recreate GoTrue so it picks up the new env: `docker compose up -d gotrue`

Any `jwk-init --write-env` run (rotate or not) rewrites `.env` atomically and
normalizes its mode to `0600`, since the file holds private RSA material. If you'd
deliberately set a looser mode (e.g. `0640` for a `docker` group), re-apply it
after running.

`docker compose up -d` (without `stop`/`start`) detects the env diff against the
existing container and recreates it. `docker compose start gotrue` would reuse the
cached env from container creation and the new key would NOT be loaded — this is a
common foot-gun. The Verifier picks up the new key automatically on the next
unknown-`kid` lookup — no TF restart needed.

**Caveat:** any access tokens still in flight that were signed by the old key will
fail verification as soon as GoTrue restarts. GoTrue's default access-token
lifetime is 1 hour, so the practical impact is "users with active sessions need to
re-authenticate." For zero-downtime overlap rotation (publish both old and new
keys, switch the signing kid, wait for the old to expire, drop the old) you'd need
to maintain a multi-key `GOTRUE_JWT_KEYS` array by hand — our `jwk-init` doesn't
currently support merge semantics. Planned for a future ticket; for now, rotate
during low-traffic windows or treat each rotation as a forced re-auth event.

The SAML request-signing key is a **different** key with different rotation
semantics — see the [SSO guide](sso-entra.md).
