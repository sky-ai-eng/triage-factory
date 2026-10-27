# Self-hosting

Guides for running the multi-tenant deployment (`TF_MODE=multi`): a Docker
Compose stack with Postgres, GoTrue and an object store, split into a control
service and one or more executors. For the single-user binary, see
[local mode](../local/README.md).

## Set up

- [Install](install.md) — GitHub OAuth app, `.env`, `jwk-init`, `docker compose up`
- [Deployment GitHub App](deployment-github-app.md) — one App every workspace without its own can connect to
- [Deployment Jira app](deployment-jira-app.md) — one Atlassian OAuth app for one-click Jira Cloud connections
- [Deployment Linear app](deployment-linear-app.md) — one Linear OAuth app every workspace can install
- [Deployment commit signer](deployment-commit-signer.md) — signed, Verified commits for workspaces on the deployment App
- [SSO](sso.md) — SAML sign-in through GoTrue
- [Bedrock role mode](bedrock-role-mode.md) — short-lived Bedrock credentials from a customer IAM role

## Operate

- [Monitoring](monitoring.md) — health endpoints, metrics, traces
- [Scaling](scaling.md) — more executors, multiple control pods, database pools
- [Networking](networking.md) — client IPs behind a load balancer
- [Storage](storage.md) — durable workspace storage
- [Secrets](secrets.md) — supplying secrets from files
- [JWT key rotation](jwt-key-rotation.md) — rotating the key that signs login sessions
- [Fleet console](fleet-console.md) — the deployment-wide admin view and operator access

Every variable is listed with a short note in
[`.env.example`](../../../.env.example). The security model is under
[security](../security/README.md).
