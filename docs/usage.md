# Usage

## Prerequisites

- [Go](https://go.dev/) 1.23+
- [Node.js](https://nodejs.org/) 20+
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) (for AI scoring and delegation)

## Running

```bash
# Default (port 3000, opens browser)
./todotriage

# Custom port, no browser
./todotriage --port 8080 --no-browser
```

## CLI subcommands

The binary exposes subcommands used internally by delegation agents. You don't need to call these directly — they're invoked by Claude Code during delegated runs.

```bash
# Execute GitHub commands in the context of a delegated run
./todotriage exec gh pr view --owner sky-ai-eng --repo myrepo --number 42

# Check agent run status
./todotriage status <run-id>
```

## Configuration

Config lives at `~/.todotriage/config.yaml` and can be edited via the Settings page or directly:

```yaml
github:
  base_url: "https://github.com"
  poll_interval: 1m

jira:
  base_url: "https://jira.yourcompany.com"
  poll_interval: 2m
  projects: [PROJ, INFRA]
  pickup_statuses: [Open, Ready for Development]
  in_progress_status: "In Progress"

ai:
  model: sonnet

server:
  port: 3000
```

### Jira setup

Jira uses a two-stage flow in Settings:

1. Enter your Jira URL and Personal Access Token, click **Connect**. Credentials are validated and stored immediately.
2. The card expands to reveal project selection, poll interval, and status configuration. Statuses are fetched automatically from your Jira instance.
3. **Save** is disabled until you've configured projects, pickup statuses, and an in-progress status.

### Credentials

All credentials (GitHub PAT, Jira PAT) are stored in your OS keychain, never on disk. Token fields in Settings show "leave blank to keep current" when a token is already stored.

## GitHub polling

The poller tracks PRs across several categories:

- **Review requested** — PRs where your review is pending
- **Authored** — Your open PRs, including CI status from the check-runs API
- **Mentioned** — PRs where you were @mentioned
- **Reviewed** — PRs you've previously reviewed (tracks for follow-up)
- **Merged / Closed** — Terminal PRs tracked for dashboard statistics

All discovery queries filter to recent activity. The tracker diffs snapshots on each poll cycle and emits typed events only on state transitions — see [tracked-events.md](tracked-events.md) for the full event taxonomy.

## Repo profiling

Configured repos are automatically profiled on first run using Claude Haiku. The profiler fetches README.md, CLAUDE.md, and AGENTS.md from each repo and generates a summary used by the AI scorer and delegation agents.

Profiles are cached for 3 days. The **Re-profile** button on the Repos page forces an immediate refresh regardless of TTL.