# GitHub polling

The poller tracks PRs across several categories:

- **Review requested** — PRs where your review is pending
- **Authored** — Your open PRs, including CI status from the check-runs API
- **Mentioned** — PRs where you were @mentioned
- **Reviewed** — PRs you've previously reviewed (tracks for follow-up)
- **Merged / Closed** — Terminal PRs tracked for dashboard statistics

All discovery queries filter to recent activity. The tracker diffs snapshots on
each poll cycle and emits typed events only on state transitions — see
[tracked-events.md](tracked-events.md) for the full event taxonomy.
