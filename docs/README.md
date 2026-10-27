# Triage Factory documentation

Setup you do inside a workspace (connecting GitHub, Jira, Linear or Slack,
mapping events to prompts, team settings) is explained in the app. These pages
cover what happens outside it: installing, deploying, and operating Triage
Factory, and working on its code.

## Run it on your machine (local mode)

- [Install](users/local/install.md) — Homebrew, a release tarball, or a source build
- [Running](users/local/running.md) — flags and subcommands
- [Running on a server](users/local/server.md) — a host with no keychain or browser
- [Configuration](users/local/configuration.md) — credentials and the Linux agent sandbox
- [Secret storage](users/local/secret-storage.md) — the keychain and the encrypted-file fallback
- [Tuning](users/local/tuning.md) — concurrency, disk, logging, tracing

## Run it for your organization (self-hosting)

- [Install](users/self-hosting/install.md) — `.env`, signing keys, `docker compose up`
- Deployment-wide integrations, each optional:
  [GitHub App](users/self-hosting/deployment-github-app.md),
  [Jira (Atlassian) app](users/self-hosting/deployment-jira-app.md),
  [Linear app](users/self-hosting/deployment-linear-app.md),
  [commit signer](users/self-hosting/deployment-commit-signer.md)
- [SSO](users/self-hosting/sso.md) and [Bedrock role mode](users/self-hosting/bedrock-role-mode.md)
- Operating: [monitoring](users/self-hosting/monitoring.md),
  [scaling](users/self-hosting/scaling.md),
  [networking](users/self-hosting/networking.md),
  [storage](users/self-hosting/storage.md),
  [secrets](users/self-hosting/secrets.md),
  [JWT key rotation](users/self-hosting/jwt-key-rotation.md),
  [fleet console](users/self-hosting/fleet-console.md)

## Evaluate its security

- [Security](users/security/README.md) — threat model, isolation tiers, privilege separation, seccomp, verifying releases

## Which "signing"?

| You want to… | Page |
|---|---|
| Have agents' commits show as Verified on GitHub | **Settings → Commit signing** in the app (one workspace), or the [deployment commit signer](users/self-hosting/deployment-commit-signer.md) (every workspace on the deployment's GitHub App) |
| Rotate the key that signs login sessions | [JWT key rotation](users/self-hosting/jwt-key-rotation.md) |
| Set up the key that signs SAML requests | [SSO](users/self-hosting/sso.md) |
| Check a release's signature | [Verifying releases](users/security/verifying-releases.md) |

## Contributing

- [CONTRIBUTING.md](../CONTRIBUTING.md) — terms and pull request expectations
- [Contributor docs](contributors/README.md) — Enterprise features, sandbox egress, seccomp, benchmarks
- [`specs/`](../specs/) — design specs, written for the coding agents that implement them
