# GitHub polling

The GitHub poller follows the open pull requests of the repositories an org
tracks. It does not search for pull requests by your relationship to them
(authored, review requested, mentioned); every open pull request in a tracked
repository is followed, and task rules and triggers decide which of them need
your attention. The only search left is the merged/closed backfill that fills the
personal dashboard (see [Dashboard backfill](#dashboard-backfill)).

The poll interval is set in Settings. It defaults to 5 minutes; see
[Configuration](../local-mode/configuration.md) for the values offered.

## The cycle

Each cycle reads the org's tracked repositories on its GitHub host and hands
them to the tracker (`Tracker.RefreshGitHub` in `internal/tracker`), which runs
three phases. With a GitHub App the tracker runs once per installation, over the
tracked repositories that installation's grant includes; with a personal access
token it runs once over the whole tracked set.

### Phase 1: discovery

Discovery (`discoverGitHub`) lists the open pull requests of each tracked
repository through the REST pulls endpoint
(`GET /repos/{owner}/{repo}/pulls?state=open`). Each listing is a conditional
request carrying the ETag stored on the repository's registry row from its last
listing:

- **304 Not Modified** — the repository's open pull requests are unchanged. The
  repository is marked quiet for this cycle. A 304 does not count against
  GitHub's primary rate limit.
- **200** — the listing is read, and its pull requests are stored. A pull
  request seen for the first time becomes an entity, seeded with the listing's
  snapshot; a closed entity whose pull request is open again is reactivated.

The new ETag is written only after every pull request the listing returned has
been stored. If one could not be stored, the repository keeps its old ETag, so
the next listing is a 200 that returns it again.

Listings run concurrently, up to `TF_POLL_REPO_CONCURRENCY` repositories at a
time (default 4, clamped to 1–16; `1` lists them one after another). The
conditional request covers only the first page of 100. A repository with more
open pull requests is re-listed in full whenever the first page changes, up to
10 pages (1,000 pull requests); a listing cut off at that cap is logged at WARN.

A repository GitHub answers 403 or 404 for is skipped: the credential cannot
reach it. A cycle in which every listing failed because of the connection or the
credential reports the failure and goes no further.

#### Rate limits and the resume cursor

When a listing is refused for the rate limit, discovery stops starting new
listings. The pull requests from listings that already finished are still
stored, and their ETags written. Phases 2 and 3 are skipped, since they would
be refused the same way, and the cycle reports no completed poll.

The poller keeps a round-robin cursor per org: the repository the next cycle
starts from. A cycle cut short by the rate limit sets it to the first
repository it did not list, and the next poll is scheduled for when GitHub says
the limit resets. With a GitHub App, an installation whose token or grant could
not be read also leaves the cursor at the first of its repositories. A cycle
that lists every repository clears it. Without the cursor, every cycle would
start at the head of the list, and on a large tracked set the repositories at
the end could go unlisted for as long as the limit kept running out.

#### Dashboard backfill

The personal dashboard counts merged and closed pull requests over the last 30
days, which includes history from before TF started tracking. The backfill
seeds those pull requests as closed entities, with no events and no tasks. It
searches the tracked repositories, through GitHub's GraphQL search, for pull
requests merged or closed without merging in the last 30 days that you authored
or reviewed. Each search query returns at most 50 pull requests.

- **Local mode, polling with a personal access token** — the searches run as
  part of discovery on every cycle, for the login bound to the local user.
  They are skipped in a cycle whose listings hit the rate limit or reached no
  repository.
- **Multi mode** — the poll cycle has no user to search for, so the server runs
  the same searches per user (`BackfillDashboardHistory`): when a user binds a
  GitHub identity, and when they first open the dashboard. A marker on the
  user's identity makes it run once per user and host.

### Phase 2: refresh

The refresh reads the current state of every active GitHub entity on the org's
host through GraphQL `nodes(ids: [...])`, 20 pull requests per query. Two
fragments are used:

- **Open pull requests** get the full fragment, which adds the head commit's
  check runs and the recent label, review-request and ready-for-review timeline
  events to the pull request's own fields.
- **Merged and closed pull requests** (an entity whose stored snapshot is
  terminal but which is not closed yet) get the discovery fragment, which has
  no check runs.

#### The refresh gate

An open pull request is not refreshed when nothing about it can have changed
since its last refresh. It is skipped only when all of these hold:

- it was last refreshed less than 10 minutes ago;
- its `updatedAt` and head SHA match the stored snapshot's;
- its body is unchanged;
- the stored snapshot's check runs have been read (a snapshot seeded from the
  listing has none), and none is still in progress.

The fresh values come from this cycle's listing, or, for a repository that
answered 304, from the stored snapshot itself, since a 304 means the listing
has not changed. A check run completing does not change a pull request's
`updatedAt`, which is why a pull request with CI in progress is always
refreshed. Neither does a workflow re-run on an unchanged commit, which adds a
check run after the earlier ones completed; the 10-minute floor bounds how late
its result is reported.

For a skipped pull request, the tracker still compares the stored review
requests against its open `review_requested` tasks and emits
`github:pr:review_request_removed` for each reviewer no longer requested.

#### Stubs

An entity can exist before the poller has a snapshot for it: it was created
outside the poller (an agent's `exec` verb touching a pull request), or its
snapshot was cleared when an org admin turned the source off. The tracker reads
the pull request's node id through REST and refreshes it with the others, then
stores the result as its first snapshot without diffing it, so nothing is
emitted for state that predates tracking.

#### Reviews and comments

The refresh records each pull request's newest review and newest comment, and
how many of each it has. When those differ from the stored snapshot's, the
tracker reads the new reviews or the new comments themselves, paging back
through GraphQL from the newest until it reaches the ones it has seen. A pull
request whose newest review, review count and newest comment did not move is
not read.

The first of these reads refused for the rate limit stops the rest for the
cycle: no further request is sent. A pull request that was not read keeps its
stored markers, so the next cycle reads the same reviews and comments.
Phase 3 still runs for every pull request the refresh returned.

### Phase 3: diff and emit

Each refreshed snapshot is compared against the stored one, and the transitions
between them become typed events — see [Tracked events](tracked-events.md) for
the event types and when each fires. The new snapshot and its events are written
in one transaction. If the entity's snapshot was written by another cycle in the
meantime, neither is written, and the next cycle compares against the snapshot
that won. Because the stored snapshot decides what is new, a transition is
emitted once.

A cycle that reaches Phase 2 with at least one active entity ends with a
`system:poll:completed` event, which triggers scoring and repository profiling.
