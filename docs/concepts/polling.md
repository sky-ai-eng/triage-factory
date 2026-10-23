# GitHub polling

The poller follows every open PR in the repositories an org tracks. Each cycle
(`Tracker.RefreshGitHub`) runs three phases. The poll interval is set in
Settings; see [Configuration](../local-mode/configuration.md).

1. **Discover.** Each tracked repository's open PRs are listed through the REST
   pulls endpoint, conditional on the ETag stored from its last listing. A 304
   means the repository is quiet this cycle. Listings run up to
   `TF_POLL_REPO_CONCURRENCY` at a time (default 4) and stop at 10 pages. On a
   rate limit, discovery stops and the next cycle resumes from the first
   repository it missed (a round-robin cursor).

   The merged/closed history the dashboard shows is the one search left: in
   local mode, a 30-day GraphQL search for the PRs the user authored or reviewed
   that runs each cycle; in multi mode, the same search run once per user
   (`BackfillDashboardHistory`).

2. **Refresh.** Active PRs are batch-read through GraphQL: open PRs with check
   runs, merged and closed PRs without. An open PR is skipped when its
   `updatedAt` and head SHA are unchanged and no CI is in flight, unless it was
   last refreshed more than 10 minutes ago. An entity with no snapshot yet (a
   stub) is seeded here without emitting events. New reviews and comments are
   then read for the PRs whose newest review or comment moved; the first rate
   limit stops those reads until the next cycle.

3. **Diff and emit.** Each new snapshot is compared against the stored one and
   the transitions are emitted as events. See [Tracked events](tracked-events.md)
   for the taxonomy.
