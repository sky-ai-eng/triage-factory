# Triage Factory

<p align="center">
  <img src="docs/imgs/factory-page.png" alt="Factory" width="90%" />
</p>

<p align="center">An AI-powered software factory where humans decide what gets automated and can take over at any time.</p>

<p align="center">
  <a href="https://github.com/sky-ai-eng/triage-factory/releases">
    <img src="https://img.shields.io/github/v/release/sky-ai-eng/triage-factory?sort=semver&label=latest%20release" alt="Latest release" />
  </a>
  <a href="https://github.com/sky-ai-eng/triage-factory/actions/workflows/test.yml">
    <img src="https://github.com/sky-ai-eng/triage-factory/actions/workflows/test.yml/badge.svg?branch=main" alt="Test" />
  </a>
  <a href="https://github.com/sky-ai-eng/triage-factory/actions/workflows/lint.yml">
    <img src="https://github.com/sky-ai-eng/triage-factory/actions/workflows/lint.yml/badge.svg?branch=main" alt="Lint" />
  </a>
</p>

Triage Factory watches everything that needs attention across your GitHub and Jira — open PRs, review requests, CI failures, merge conflicts, assigned tickets — scores and ranks it with AI, and lets you delegate the work to Claude Code agents that run in isolation and stream back to a live dashboard. You decide what gets automated, and you can take over any agent's run at any point.

It's a single Go binary that runs on infrastructure you control. One developer runs it locally — SQLite, credentials in the OS keychain (or an encrypted file when no keychain is reachable). A whole organization runs the _same_ binary self-hosted in multi-tenant mode — Postgres, per-org row-level isolation, and every agent run confined to its own gVisor sandbox. Your code and credentials stay on your infrastructure, and the only things that leave it are API calls to the services you connect — GitHub, Jira, and your model provider. Local mode is the fundamentally the same product, just with N=1 orgs, teams, and users.

For a product tour and screenshots, see [triagefactory.com](https://www.triagefactory.com).

## How it works

Work flows through an automation engine drawn as a factory floor. A durable **entity** (a PR, a Jira ticket) emits **events** as things happen to it — a review lands, CI fails, a label changes. Events that match your rules become **tasks**; a task you delegate becomes a **run** — one headless Claude Code execution inside an isolated git worktree. You map event types to prompts in a visual graph, so "review requested" routes to your review prompt and "Jira assigned" routes to your implementation prompt. Agents work autonomously and stream their activity back in real time; when a run finishes, you review and approve.

## Local or self-hosted

**Local (N=1)** — one developer, on your laptop. One command to install and run:

```bash
brew tap sky-ai-eng/tap && brew install triagefactory && triagefactory
```

State lives in SQLite, credentials live in the OS keychain or an encrypted file, and agent runs execute in isolated git worktrees (not secured sandboxes). No Postgres, no Docker, no DevOps. For direct downloads, building from source, and the full flag reference, see [docs/local-mode/](docs/local-mode/README.md) and [docs/INSTALLATION.md](docs/INSTALLATION.md).

**Self-hosted (multi-tenant)** — your whole org, on Linux hosts you control. The same binary and schema, deployed with Docker Compose as a control + executor split: Postgres with per-org row-level security, SSO/SAML sign-in, audit logs, and Slack ingest via the [Enterprise Edition](ee/), a `/usage` dashboard with per-team and org-wide spend caps, and every agent run confined to its own gVisor sandbox. See [docs/self-hosting/](docs/self-hosting/README.md) to stand one up.

## Security & isolation

Triage Factory runs code written by an AI agent acting on untrusted input such as repository contents, issue text, and tool output. Confining that agent, isolating tenants from one another, and keeping real credentials out of reach is the product's central design problem. The model below is the self-hosted, multi-tenant posture on Linux; local mode is a single user on their own machine, where runs execute in isolated git worktrees and most security features don't apply.

**The agent is the most confined process in the system.** In multi-tenant mode, each run executes in its own gVisor sandbox: non-root, zero ambient capabilities, a tailored seccomp allowlist, and a per-run memory ceiling. The elevated privileges Triage Factory needs from the host exist only to _build_ that sandbox — never to run the agent inside it. And no single process holds both a dangerous privilege and exposure to the agent's output: the part that can configure the kernel holds no credentials, and the part that holds credentials can't touch the kernel.

**Credentials never enter the sandbox.** The agent's environment is built from scratch with placeholders — a per-run proxy URL and a throwaway token that dies with the run. Your real keys (Anthropic, the GitHub App, the database) live only on the host and are attached on the upstream hop. Dump a live agent's environment and nothing within is sensitive and usable — no real provider key, GitHub token, or database password.

**Tenants are isolated by construction.** Data is fenced with per-org Postgres row-level security. And unlike the usual container setup — every run hanging off one shared Linux bridge, a single ARP-spoof away from its neighbors — each run gets its own point-to-point virtual link on its own private subnet, with no layer-2 path between concurrent runs. Cross-tenant snooping isn't "blocked by a rule"; there's no shared wire to snoop. Each run's egress is a fail-closed allowlist that reaches only the hosts it needs.

**Evaluating Triage Factory for your own infrastructure?** The full threat model, the privilege-separation process model, and what a compromise of each component yields — every claim anchored to something you can run against the binary — are in [docs/security/security-overview.md](docs/security/security-overview.md).

## Documentation

- [docs/INSTALLATION.md](docs/INSTALLATION.md) — install, build from source, prerequisites
- [docs/local-mode/](docs/local-mode/README.md) — local mode: CLI flags, configuration, secret storage, headless
- [docs/self-hosting/](docs/self-hosting/README.md) — multi-tenant self-hosting (install, scaling, SSO, monitoring)
- [docs/security/](docs/security/README.md) — isolation tiers, security overview, privilege separation
- [docs/concepts/tracked-events.md](docs/concepts/tracked-events.md) — the GitHub/Jira event taxonomy
- [CONTRIBUTING.md](CONTRIBUTING.md) — contribution terms

## License

Source-available under the [Triage Factory License 1.0](LICENSE): free to use, copy, and modify for your own internal business purposes, but not to redistribute or offer as a hosted service. The [`ee/`](ee/) subtree (Enterprise Edition) is under the same license, with its features gated behind a commercial license key. See [CONTRIBUTING.md](CONTRIBUTING.md) for contribution terms.

## Disclaimer

Triage Factory is provided "as is", without warranty of any kind; see the [LICENSE](LICENSE) for the full terms.

Triage Factory delegates work to autonomous agents that read and write to your source repositories, ticket trackers, and local filesystem. You are solely responsible for reviewing what agents do on your behalf, for the credentials you configure, and for the consequences of any automated actions taken against your systems or third-party services.

**Self-hosted multi-tenant deployments.** When you self-host Triage Factory to serve multiple organizations from one deployment, tenant isolation depends on your infrastructure, configuration, network topology, secrets management, and patching cadence, as well as on the correctness of the upstream software itself — which, like all software, may contain isolation, sandboxing, or row-level-security defects, known or unknown. See [docs/security/](docs/security/README.md) for the isolation model to evaluate.
