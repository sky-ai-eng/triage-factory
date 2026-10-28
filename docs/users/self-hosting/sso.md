# SSO (SAML): deployment setup

SAML single sign-on runs through GoTrue, so it is multi-mode only, and it needs
an Enterprise license that includes `sso`. This page is the one-time
deployment setup. Each org then connects its own identity provider from
**Settings → Single sign-on** in the app.

A fresh deployment has no SSO: the first admin signs in with GitHub and sets up
SSO for everyone else. GitHub sign-in stays available afterwards.

## 1. Generate the SAML request-signing key

GoTrue signs its SAML requests with an RSA key. This is a different key from
the session JWT key that `jwk-init` writes (`GOTRUE_JWT_KEYS`).

```sh
openssl genpkey -algorithm rsa -pkeyopt rsa_keygen_bits:2048 -outform DER | base64 -w0
```

It must be RSA; GoTrue rejects an EC/ES256 key. Add it to `.env` with the
flag that turns SAML on:

```sh
GOTRUE_SAML_ENABLED=true
GOTRUE_SAML_PRIVATE_KEY=<the base64 output>
```

- The flag is off by default, and the key does nothing without it. With the
  flag on, GoTrue refuses to boot without a valid key.
- **Generate the key once and keep it.** Regenerating it invalidates every
  org's registered provider, and each org has to re-establish trust on its
  identity provider's side.

## 2. Make sure the service-role token is set

Registering an org's connection uses `TF_GOTRUE_SERVICE_ROLE_TOKEN`, which
`triagefactory jwk-init --write-env .env` writes alongside the JWT key. If your
`.env` predates it, run the same command again: it reuses the existing
`GOTRUE_JWT_KEYS` and writes only the token, so no sessions are invalidated.
Don't pass `--rotate` unless you mean to rotate the JWT key
([JWT key rotation](jwt-key-rotation.md)).

## 3. Apply and allow egress

```sh
docker compose up -d
```

Use `up -d`, not `start`: `start` reuses the container's old environment.

The `gotrue` container fetches each org's federation metadata itself, so it
needs outbound HTTPS to the identity provider's metadata host
(`login.microsoftonline.com` for Microsoft Entra). Add it to the gotrue
service's allowed destinations if you run a default-deny egress policy.

An org admin can now open **Settings → Single sign-on** and connect.

## Troubleshooting

| Symptom                                                                            | Fix                                                                                                                                                                                                                                                                                        |
| ---------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Connecting returns 503 `SSO admin token not configured`                            | `TF_GOTRUE_SERVICE_ROLE_TOKEN` is missing on `control`. Run `triagefactory jwk-init --write-env .env`, then `docker compose up -d control`.                                                                                                                                                |
| Connecting returns 500 with a metadata error                                       | GoTrue couldn't fetch the metadata URL. Check gotrue's outbound access to the identity provider.                                                                                                                                                                                           |
| Connecting returns 500 with a 403 from GoTrue's admin API                          | The service-role token was signed by a JWT key that has since been rotated. Run `jwk-init --write-env .env` again to re-mint it, then `docker compose up -d control`.                                                                                                                      |
| Connecting returns 404                                                             | The caller isn't an org admin in that org, or the deployment is in local mode.                                                                                                                                                                                                             |
| GoTrue won't boot after enabling SAML                                              | `GOTRUE_SAML_PRIVATE_KEY` is missing or invalid. Regenerate it (step 1) and run `docker compose up -d gotrue`.                                                                                                                                                                             |
| SSO broke after changing the SAML key                                              | Changing the key invalidated every provider registration. Keep the key stable; after a change, each org re-establishes trust with its identity provider.                                                                                                                                   |
| An org changed its identity provider's metadata, but re-connecting changed nothing | Connecting again returns the existing connection without re-reading the metadata. Replacing a connection's metadata currently takes operator action against GoTrue's admin API.                                                                                                            |
| An SSO sign-in fails at the callback with a 500                                    | Granting a new member's membership and writing its audit record happen together, and fail together if the database can't commit. Check the `control` logs for `jit: grant membership failed` or `audit jit member granted`, and the database's health. The user retries once it's healthy. |
