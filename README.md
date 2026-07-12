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

It's a single Go binary that runs on infrastructure you control. One developer runs it locally — SQLite, credentials in the OS keychain (or an encrypted file when no keychain is reachable). A whole organization runs the _same_ binary self-hosted in multi-tenant mode — Postgres, GoTrue auth, per-org isolation, and, on Linux, per-run gVisor sandboxing. There's no hosted service in the loop: your code and credentials stay on your infrastructure, and the only things that leave it are API calls to GitHub, Jira, and Claude. Local mode is just the same schema and the same code path at N=1.

For a product tour and screenshots, see [triagefactory.com](https://www.triagefactory.com).

## How it works

Work flows through an automation engine drawn as a factory floor. A durable **entity** (a PR, a Jira ticket) emits **events** as things happen to it — a review lands, CI fails, a label changes. Events that match your rules become **tasks**; a task you delegate becomes a **run** — one headless Claude Code execution inside an isolated git worktree. You map event types to prompts in a visual graph, so "review requested" routes to your review prompt and "Jira assigned" routes to your implementation prompt. Agents work autonomously and stream their activity back in real time; when a run finishes, you review and approve.

## Local or self-hosted

**Local (N=1)** — one developer, on your laptop. `brew install`, SQLite, credentials in the OS keychain (or an encrypted file when none is reachable, like a container or headless host). Agent runs execute in isolated git worktrees. No DevOps.

**Self-hosted (multi-tenant)** — your whole org on a Linux host you control: Postgres, GoTrue sign-in (SSO/SAML via the [Enterprise Edition](ee/)), RLS-isolated tenants, and — on Linux — gVisor-sandboxed agent runs with a locked-down egress allowlist. Same binary, same schema, deployed with Docker Compose. See [docs/self-hosting/](docs/self-hosting/README.md).

## Install

### macOS/Linux — Homebrew (recommended)

```bash
brew update
brew tap sky-ai-eng/tap
brew install triagefactory
triagefactory
```

For direct downloads, building from source, and platform notes, see [docs/INSTALLATION.md](docs/INSTALLATION.md).

## Documentation

- [docs/INSTALLATION.md](docs/INSTALLATION.md) — install, build from source, prerequisites
- [docs/local-mode/](docs/local-mode/README.md) — local mode: CLI flags, configuration, secret storage, headless
- [docs/self-hosting/](docs/self-hosting/README.md) — multi-tenant self-hosting (install, scaling, SSO, monitoring)
- [docs/security/](docs/security/README.md) — isolation tiers, security overview, privilege separation
- [docs/concepts/tracked-events.md](docs/concepts/tracked-events.md) — the GitHub/Jira event taxonomy
- [CONTRIBUTING.md](CONTRIBUTING.md) — contribution terms

## License

The repository is source-available under the [Triage Factory License 1.0](LICENSE) — free to use, copy, and modify for your own internal business purposes, but not to redistribute or offer as a hosted service. The [`ee/`](ee/) subtree (Enterprise Edition) is covered by the same license; its features are gated behind a license key and require a commercial subscription to enable. See [CONTRIBUTING.md](CONTRIBUTING.md) for contribution terms.

## Disclaimer

Triage Factory is provided "as is", without warranty of any kind, express or implied, including but not limited to the warranties of merchantability, fitness for a particular purpose, non-infringement, and title. In no event shall the authors or copyright holders be liable for any claim, damages, or other liability, whether in an action of contract, tort, or otherwise, arising from, out of, or in connection with the software or the use or other dealings in the software.

Triage Factory delegates work to autonomous agents that read and write to your source repositories, ticket trackers, and local filesystem. You are solely responsible for reviewing what agents do on your behalf, for the credentials you configure, and for any consequences of automated actions taken against your systems or third-party services.

**Self-hosted multi-tenant deployments.** Triage Factory can be self-hosted in multi-tenant mode (e.g., via the provided Docker Compose configuration) to serve multiple organizations from a single deployment. Multi-tenant isolation in that configuration depends on the operator's infrastructure, configuration, network topology, secrets management, and patching cadence, as well as on the correctness of the upstream software itself — which, like all software, may contain isolation, sandboxing, or row-level-security defects, known or unknown. Operators who choose to host Triage Factory for third parties do so at their own risk and are solely responsible for the security, privacy, compliance, and tenant isolation of their deployment. The authors and copyright holders make no warranty that any release is free of multi-tenant isolation defects and accept no liability for cross-tenant data exposure, sandbox escape, or any other isolation failure in self-hosted deployments.
