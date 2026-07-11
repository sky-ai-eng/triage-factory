# Self-hosting (multi-mode)

Operator guides for running the multi-tenant deployment (`TF_MODE=multi`) — the
Docker Compose stack with Postgres, GoTrue, and an object store. For the
single-user binary, see [local mode](../local-mode/); for the security model a
reviewer evaluates, see [docs/security/](../security/).

## Setup

- [Install](install.md) — GitHub OAuth app, `.env`, `jwk-init`, `docker compose up`, verify the OAuth flow
- [SSO with Microsoft Entra (SAML)](sso-entra.md) — enable GoTrue SAML + register an org's connection

## Operate

- [Monitoring & health checks](monitoring.md) — `/api/health`, `/readyz`, executor `/healthz`
- [Scaling out](scaling.md) — control + N executors, per-role DB pools, HA reverse proxy
- [Client IP & trusted proxies](networking.md) — `TF_TRUSTED_PROXY_CIDR` behind a load balancer
- [Durable workspace storage](storage.md) — SeaweedFS + BYO S3/R2
- [Rotating the JWT signing key](key-rotation.md)

## Security

The threat model, privilege-separation process model, seccomp profile, and release
verification live under [docs/security/](../security/) — they span self-host and
SaaS, so they're not filed here.
