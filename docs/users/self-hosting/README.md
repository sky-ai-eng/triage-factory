# Self-hosting

Guides for running the multi-tenant deployment (`TF_MODE=multi`). This ships
as a Docker Compose stack with Postgres, GoTrue, and an object store, split
into control and executor services, but using the docs here you should be
able to run it however you want.

This is a heavy solution for solo devs to host themselves. I've tried to make
it simple to set up but between the hardware requirements and the deployment
configuration, I recommend either using
[the hosted SaaS solution](https://triagefactory.com) (coming soon for wider
use) or the single-user binary running in [local mode](../local/README.md).
This is primarily for exceptionally security-conscious consumers needing
self-managed infrastructure. Self-hosting in this mode is also required for
GHES tenants running on a VPC — the SaaS offering has no path into those
networks at present.

## Set up

- [Install](install.md) — GitHub OAuth app, `triagefactory deploy init`, `deploy up`
- [Deployment GitHub App](deployment-github-app.md) — one App every workspace without its own can connect to
- [Deployment Jira app](deployment-jira-app.md) — one Atlassian OAuth app for one-click Jira Cloud connections
- [Deployment Linear app](deployment-linear-app.md) — one Linear OAuth app every workspace can install
- [Deployment commit signer](deployment-commit-signer.md) — signed, Verified commits for workspaces on the deployment App
- [SSO](sso.md) — SAML sign-in through GoTrue
- [Bedrock role mode](bedrock-role-mode.md) — short-lived Bedrock credentials from a customer IAM role

## Operate

- [Upgrading](upgrading.md) — moving to a newer release
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
