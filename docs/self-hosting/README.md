# Self-hosting (multi-mode)

Operator guides for running the multi-tenant deployment (`TF_MODE=multi`) — the
Docker Compose stack with Postgres, GoTrue, and an object store. For the
single-user binary, see [local mode](../local-mode/); for the security model a
reviewer evaluates, see [docs/security/](../security/).

## Setup

- [Self-host setup](install.md) — GitHub OAuth app, `.env`, `jwk-init`, `docker compose up`, verify the OAuth flow, register a deployment GitHub App
- [Slack app setup](slack.md) — connect a workspace via the copy-paste manifest; migrating an app to pick up engaged-thread events
- [SSO with Microsoft Entra (SAML)](sso-entra.md) — enable GoTrue SAML + register an org's connection
- [Bedrock role mode](bedrock-role-mode.md) — short-lived STS LLM credentials via a customer IAM role (no stored Bedrock secret)

## Operate

- [Fleet console & operators](fleet-console.md) — the deployment-wide admin view + granting operator access via the `operator` CLI
- [Monitoring & health checks](monitoring.md) — `/api/health`, `/readyz`, executor `/healthz`, `:9464` metrics, and traces (`TF_TRACES_ENDPOINT` + the bundled Tempo/Grafana stack)
- [Scaling out](scaling.md) — control + N executors, per-role DB pools, HA reverse proxy
- [Client IP & trusted proxies](networking.md) — `TF_TRUSTED_PROXY_CIDR` behind a load balancer
- [Durable workspace storage](storage.md) — SeaweedFS + BYO S3/R2
- [Deployment secrets](secrets.md) — supplying secrets from files (`*_FILE` / Docker & K8s secrets) and how TF handles them
- [Rotating the JWT signing key](key-rotation.md)

## Security

The threat model, privilege-separation process model, seccomp profile, and release
verification live under [docs/security/](../security/) — they span self-host and
SaaS, so they're not filed here.
