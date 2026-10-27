# Rotating the session JWT key

This is the RS256 key GoTrue signs sign-in tokens with (`GOTRUE_JWT_KEYS`,
written by `jwk-init`). It is not the commit-signing key or the SAML
request-signing key ([SSO](sso.md)).

Rotating it ends every active session: tokens signed with the old key stop
verifying as soon as GoTrue restarts, so everyone signs in again. Rotate in a
quiet window.

1. Regenerate the keypair, the JWT secret and the service-role token in place:

   ```sh
   ./triagefactory jwk-init --write-env .env --rotate
   ```

   Without `--rotate`, `jwk-init` reuses the existing keypair.

2. Recreate GoTrue and the control service so they load the new values:

   ```sh
   docker compose up -d
   ```

   Use `up -d`. `docker compose start` reuses the container's old environment,
   so the new key would not load. Triage Factory picks up the new public key on
   its own.

`jwk-init --write-env` rewrites `.env` with mode `0600`. If you had set a looser
mode on purpose (for example `0640` for a `docker` group), set it again.

Only one key at a time is supported: `jwk-init` can't publish the old and new
keys side by side for a no-downtime switch.
