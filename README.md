# Triage Factory

Triage Factory watches what needs attention across your GitHub, Jira, and Linear (open PRs, review requests, CI failures, merge conflicts, assigned tickets), ranks it with AI, and lets you delegate the work to Claude Code agents that run in isolation and stream back to a live dashboard. You decide what gets automated, and you can take over any agent's run at any point.

It is a single binary that runs on infrastructure you control. One developer runs it locally. A whole organization runs the same binary self-hosted, with Postgres, per-org row-level isolation, and every agent run confined to its own gVisor sandbox.

This repository holds the releases and the issue tracker.

## Install

```bash
brew tap sky-ai-eng/tap && brew install triagefactory && triagefactory
```

Release archives, checksums and signatures are on the [releases page](https://github.com/sky-ai-eng/triage-factory/releases).

## Documentation

- [Product tour](https://www.triagefactory.com)
- [Local mode](https://docs.triagefactory.com/local/): install, run and configure the single-user binary
- [Self-hosting](https://docs.triagefactory.com/self-hosting/): deploy and operate the multi-tenant stack
- [Security](https://docs.triagefactory.com/security/): the isolation model, for evaluating a deployment
- [Verifying releases](https://docs.triagefactory.com/security/verifying-releases/)

## Reporting a problem

Open an [issue](https://github.com/sky-ai-eng/triage-factory/issues).

## License

Triage Factory is distributed under the [Triage Factory License 1.0](LICENSE): free to use for your own internal business purposes, but not to redistribute or offer as a hosted service. Enterprise features are gated behind a commercial license key.
