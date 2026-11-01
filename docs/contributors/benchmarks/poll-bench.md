# Poll-scale benchmark

`cmd/pollbench` measures what GitHub polling costs at large-org shape —
hundreds of tracked repos, thousands of open PRs, GHES-style request
latency. The scenario it models is the one webhooks don't cover: the
**cold-start sync** (first full poll of the tracked set) and post-outage
catch-up, where the poller must list and refresh everything. It also
measures **mergeability probing** on a large monorepo whose base branch
keeps moving: how many test merges TF makes GitHub run, and what the
branch watch's reads cost. It is the before/after measurement vehicle for
polling-path work: rate-limit backoff, refresh concurrency, budgeted and
resumable cycles, and mergeability probing. Run it with
`./scripts/poll-bench.sh` (see [Running it](#running-it)).

## What the harness runs

The benchmark drives the **real** production components — `poller.Manager`
→ `tracker.Tracker` → `internal/github.Client`, and the mergeability probe
worker, over real (in-memory SQLite) stores — against a deterministic fake
GitHub API generated from a seed. Each driven cycle is one poll followed by
one probe-worker pass. The fake serves exactly the surface the poll path
and the probe worker touch:

- `GET /repos/{o}/{r}/pulls` — the per-repo open-PR listing, with real
  ETag/`If-None-Match` conditional semantics (304s are free on the rate
  limit, matching GitHub) and >100-PR pagination,
- `POST /graphql` — the `nodes(ids:)` batch refresh with the full PR
  fragment (reviews, review requests, labels, check-run rollup, comment and
  file counts), `PRMergeability`, and `PRChangedPaths` with its paging
  query (`files` with `changeType`, `changedFiles`, `headRefOid`),
- `GET /repos/{o}/{r}/git/ref/heads/{branch}` — a base branch's tip, with
  an ETag and a free 304,
- `GET /repos/{o}/{r}/compare/{a}...{b}` — with GitHub's limits: at most 300
  `files` and no count of the rest, at most the 250 most recent `commits`
  beside `total_commits`, `previous_filename` on renames, and `status`
  `ahead` / `identical` / `behind` / `diverged`,
- `GET /repos/{o}/{r}/pulls/{n}/files` — `per_page` capped at 100, the list
  capped at 3,000 files, `previous_filename` on renames and copies,
- `GET /orgs/{owner}/teams` — the per-cycle GitHub-group reconcile read,
- primary-rate-limit `x-ratelimit-*` headers and exhaustion 403s when
  enabled, exercising the client's backoff end to end.

Every GraphQL response carries `rateLimit.cost`, computed from the query
document by GitHub's documented rule: count the requests each connection
needs, assuming each reaches its `first`/`last` limit and a nested
connection costs one request per item of its parent (each id of
`nodes(ids:)` counts as an item); divide by 100 and round; the minimum is
1. The report's `points` column sums it.

The run is a multi-mode-shaped cycle (org-credential perspective, no
local-user dashboard backfill). Out of scope: Jira polling, the HTTP
API/websocket layer, and event routing/task creation — the harness counts
events at the tracker's and probe worker's publish boundary.

### Base moves and lazy mergeability

During the warm cycles the fake's world changes before each poll:

- A **base move** lands a commit on a repository's `main`, changing a
  median of 4 and at most 30 files drawn from the repository's file tree.
  The repository's open-PR listing changes in the same cycle, as it does
  when a merged PR leaves it, so the branch watch checks the branch that
  cycle. The open set itself stays fixed, which keeps every cycle's events
  predictable.
- A **head push** moves a PR's head and `updated_at`. Its changed paths
  stay the same.

Mergeability is computed lazily, as GitHub computes it. A PR's stored
answer is for the head and base tip it was computed at. A `PRMergeability`
read once either has moved returns `UNKNOWN`, counts one **test merge**,
and computes the answer, which the next read returns. A read of a PR
whose answer is current returns it and counts nothing. Every PR starts
with a current answer, so the cold sync's probes cost no test merges.

A PR becomes `CONFLICTING` only when a base move's paths overlap its own
under the tracker's overlap rule (`tracker.Overlaps`), and then with
probability `--conflict-on-overlap`. This measures cost, not correctness:
the overlap rule's correctness was measured on real repositories and is
covered by its tests.

The fake attributes each test merge to why the PR's answer was stale: its
head was pushed, a move overlapping its paths landed, or it was read again
after an `UNKNOWN` and the base moved in between. A test merge with none of
those causes is a read TF did not need, and fails `sanity`.

PR paths are drawn from a seeded tree per repository. A PR's files cluster
in one home directory (drawn by popularity, the directory of rank r
weighing 1/√r), with 30% from any directory, and a few hot files each
appear in 5% of PRs, as `pkg/features/kube_features.go` does in
kubernetes. On `mono` that puts a move's overlap set at about 1% of the
open PRs, near the 1.1% measured on kubernetes. The overlap rate on the
other shapes' small repos is not calibrated.

## Running it

```bash
./scripts/poll-bench.sh --shape dp1                 # the large-org GHES shape
./scripts/poll-bench.sh --shape mono                # mergeability probing on one monorepo
./scripts/poll-bench.sh --shape smoke --latency 0   # fast local sanity run
./scripts/poll-bench.sh --shape dp1 --base-moves --warm-cycles 20
./scripts/poll-bench.sh --shape dp1 --rate-limit 200 --rate-limit-window 60s
./scripts/poll-bench.sh --repos 100 --prs-mean 30 --files-mean 20 --seed 7
```

Reports print to stdout and are archived under `bench-results/`. Useful
flags (see `go run ./cmd/pollbench -h` for all):

- `--latency` — injected per-request latency, default 30ms (a GHES inside
  the same VPC; raise it for cross-region estimates).
- `--rate-limit` / `--rate-limit-window` — request budget per window, off by
  default. Real GHES budgets are per hour **and are configured by the GHES
  admin** (they can be raised or disabled entirely) — short bench windows
  just make exhaustion→reset→resume observable in a single run.
- `--inflight-pct` — fraction of PRs with a still-running check; these
  force a refresh every cycle, so this sets the warm-cycle GraphQL floor.
- `--base-moves` — land base moves during the warm cycles. On by default
  for `mono` (every 2 cycles); off by default for the other shapes, where
  a move lands every 10 cycles on 10% of the repos, so pair it with
  `--warm-cycles 10` or more.
- `--conflict-on-overlap` — the chance a move makes a PR whose paths it
  overlaps conflicting, default 0.03 (1–3% was measured on real
  repositories).
- `--seed` — dataset seed. Same seed + shape ⇒ identical request counts;
  only wall time varies.
- `--check` — CI mode: runs twice and fails on any sanity violation or on
  request counts differing between the runs. The count comparison is
  skipped when a rate limit is active (retry timing varies run-to-run);
  the sanity assertions still apply to both runs.

## Standard shapes

| Shape | Repos | Open PRs (seed 42) | Warm cycles | Purpose |
| -- | -- | -- | -- | -- |
| `smoke` | 20 | ~100 | 1 | CI tripwire |
| `dp1` | 300 | ~4,600 | 1 | Large-org GHES target |
| `stress` | 600 | ~12,100 | 1 | Headroom check |
| `mono` | 1 | 1,500 (1,000 tracked) | 100 | Mergeability probing on a monorepo |

Per-repo PR counts are drawn from a mixed distribution around the shape's
mean (~70% of repos below the mean, ~25% up to 3×, ~5% pathological at
6–10×), so the tail exercises multi-page listings — at `dp1`, several repos
exceed the 100-PR page size.

`mono` is one repository shaped on kubernetes/kubernetes: 1,500 open PRs on
`main`, with changed-file counts at the measured median 3, p90 17, p99 140,
and maximum 550. Its `main` moves every 2 warm cycles, and 1% of its PRs
are pushed every warm cycle. TF's open-PR listing stops at 1,000 PRs per
repository, so it tracks 1,000 of the 1,500, and every expectation and
per-PR figure the report prints counts the tracked ones.

> The `dp1` numbers are placeholders approximating a large single-org GHES
> deployment (several hundred to a few thousand developers). Replace them with a
> real deployment's repo / open-PR counts when available, and record that
> deployment's GHES rate-limit configuration here alongside — GHES admins control
> those limits, so the `--rate-limit` value worth testing is deployment-specific.

## What the report shows

Each driven cycle is one row:

- **cold #N** — cycles until every PR has a fully refreshed snapshot. One
  cycle unless a rate limit interrupts; then the sync resumes across cycles
  (already-listed repos come back as free 304s, already-refreshed snapshots
  keep their state).
- **warm #N** — cycles after the sync. With no base moves or head pushes
  nothing changed: the conditional-request measurement. `304` vs `list` is
  the ETag hit rate; `graphql` should be 0 at `--inflight-pct 0`.
- **settle** — probe passes with no poll after the last warm cycle, until
  no probe is owed, so the last moves' answers land. Printed only when
  something was owed.
- **req/list/pages/304/graphql/teams/refs/ref304/compares/ratelimited** —
  request counts by class, from the fake server's counters. Deterministic
  per seed (except retry counts under an active rate limit, which are
  timing-dependent).
- **points** — GraphQL points, by the fake's cost arithmetic above.
- **paths/files** — `PRChangedPaths` queries (first pages and further
  pages) and REST `pulls/{n}/files` pages: the branch watch's reads of PR
  paths.
- **moves/pushes** — base moves and head pushes that landed before the
  cycle's poll.
- **probes/probes by reason** — mergeability probes the worker read this
  cycle, and their admission reasons.
- **merges** — test merges the fake counted this cycle.
- **events** — `github:*` events emitted at the tracker and probe worker
  boundary. The harness knows exactly which transitions the dataset implies
  (each completed check, each review, each conflicted PR emits once on the
  first full refresh and probe pass; a warm cycle emits a `new_commits` per
  head push and a `conflicts` per PR a move made conflicting, and nothing
  else) and fails `sanity` on any mismatch — the metrics are only
  meaningful when the run did the real work.

`sanity` also fails when:

- a `github:pr:conflicts` the dataset implies is not emitted exactly once,
  or one is emitted for a PR the dataset never made conflicting;
- a test merge has no cause (above);
- the cold cycle read more than one probe per tracked PR, or more than one
  ref per base branch (runs without `--rate-limit`);
- any request reaches an endpoint the fake doesn't model (`other` > 0
  means the poll path's call set changed — extend the fake);
- any poll-cycle error occurs other than a rate-limit interrupt on a run
  that configured `--rate-limit`.

After the table, the report prints the cold cycle's probes, path reads and
ref reads; the warm cycles' REST requests, GraphQL requests and points; the
base moves' mean overlap set; the test merges by cause and the shortfall;
and the test merges probing every tracked PR on every move would have run.
- **allocs** — Go heap allocated during the cycle; **peak RSS** is the
  process high-water mark.

## Rate-limit behavior

With `--rate-limit` low enough to exhaust mid-sync, what you observe today:

- Exhaustion inside a window ≤ the client's max wait (5 minutes): the
  client sleeps through the reset inside the cycle and the sync still
  completes in **one long cycle** — wall time absorbs the waits, and the
  `ratelimited` column shows the refused requests.
- Reset further out than the client will wait: the affected calls fail
  fast, the cycle ends partial, and the driver resumes on the next cycle
  after the window rolls — `cold sync: complete in N cycle(s)` reports the
  cycles-to-complete. Discovery progress is preserved across cycles by the
  stored ETags (free 304s on re-list); refresh progress by the persisted
  snapshots.

A budgeted, resumable poll cycle (planned follow-up work) should improve the
second case from "retry and rely on 304s" to an explicit checkpoint/cursor;
this harness is the before/after measurement for it.

## Baseline results

Recorded 2026-07-06 on an Apple M3 Pro (36 GB), current implementation,
seed 42, defaults unless noted. Request counts are exact for these configs;
wall times are indicative.

### Cold start + warm cycle, `--latency 30ms`, no rate limit

| Shape | PRs | Cold wall | Cold req (list+pages/graphql) | Cold events | Warm wall | Warm req | Warm 304 rate | Peak RSS |
| -- | -- | -- | -- | -- | -- | -- | -- | -- |
| `smoke` | 97 | 0.85s | 26 (20+0 / 5) | 526 | 0.67s | 21 | 100% | 36 MB |
| `dp1` | 4,586 | 18.3s | 538 (300+7 / 230) | 24,892 | 9.7s | 301 | 100% | 119 MB |
| `stress` | 12,088 | 42.4s | 1,237 (600+31 / 605) | 65,271 | 19.5s | 601 | 100% | 258 MB |

Observations worth keeping in mind:

- Cold-start cost is dominated by sequential round trips: at `dp1`,
  538 requests × 30ms ≈ 16s of the 18.3s wall. The per-repo sweep and the
  20-PR GraphQL batches are both serial today, so wall time scales linearly
  with latency — bounded per-repo concurrency is the lever here.
- The warm cycle's budget cost collapses (300 free 304s + 1 teams read) but
  its **wall time doesn't** (9.7s at dp1 — still one sequential conditional
  GET per repo). Conditional requests save rate limit, not latency.
- Memory is comfortable at these shapes: ~260 MB peak RSS at 12k PRs.

### Under a rate limit, `smoke` shape (`--latency 1ms`)

| Config | Behavior observed | Cycles | Cold wall |
| -- | -- | -- | -- |
| `--rate-limit 10 --rate-limit-window 5s` | exhaustion mid-sync; client sleeps through window resets in-cycle | 1 | 11.1s |
| `--rate-limit 15 --rate-limit-window 5m30s` | reset beyond client max wait; cycle ends partial, resumes after the window rolls | 2 | 5m30s |

In the second run, cycle 1 spent exactly its 15-request budget (1 teams +
14 listings, 87 of 97 PRs seeded) and then failed the remaining calls
**client-side without sending them** — the pre-flight budget check kicks in
once a response reports `x-ratelimit-remaining: 0`, so the server saw zero
refused requests. Cycle 2 re-listed the 14 already-seen repos as free 304s,
listed the remaining 6, ran the full refresh, and emitted all 526 expected
events — the sanity check holds across the split.

## Mergeability probing results

Recorded 2026-11-01 on an Intel Core i9-14900KF (32 threads, 64 GB) running
Linux, seed 42, `--latency 30ms`, no rate limit. Request, point, probe and
test-merge counts are exact for these configs; wall times are indicative.
This machine is not the one the baseline above was recorded on, so wall
times do not compare across the two sections.

### Test merges on `mono`

`./scripts/poll-bench.sh --shape mono`: 100 warm cycles, a base move every
2 cycles, 1% of the open PRs pushed every cycle. Of the 1,500 open PRs, TF
tracks the 1,000 its listing returns.

| | Count |
| -- | -- |
| Base moves | 50 |
| Overlap set per move (mean) | 9.4 PRs, 0.9% of the tracked PRs |
| Head pushes on tracked PRs | 984 |
| Test merges | 1,949 |
| … on an overlap-set PR after a base move | 465 |
| … on a head-pushed PR | 984 |
| … on a PR re-read after `UNKNOWN` | 500 |
| … unaccounted (shortfall) | 0 |
| Probing every tracked PR on every move, from base moves | 50,000 |

- Every test merge has a cause. Base moves cost 965 test merges (overlap
  sets plus re-reads), against 50,000 if every tracked PR were probed on
  every move.
- The re-reads are about half of what base moves cost. A PR that answers
  `UNKNOWN` is read again in the next cycle, and since the bench drives
  cycles back to back, the next base move lands before that read half the
  time and makes the answer stale again. With a merge every 3 minutes and
  a re-read 30 seconds after the `UNKNOWN`, about 1 in 6 would.
- The first move reads every tracked PR's paths: 63 `PRChangedPaths`
  queries and 201 REST file-list pages for the PRs with a rename or copy,
  9.3 s of the cycle at 30 ms per request. Later moves read only the PRs
  pushed since the last one: 1 to 6 queries.
- The warm cycles cost 1,644 REST requests (50 of them 304s), 365 GraphQL
  requests and 547 points in all: 5.5 points a cycle. The listing changes
  every cycle, so every cycle lists all 10 pages.
- The cold cycle read 1,000 probes for 1,000 tracked PRs, no paths (a newly
  seen branch probes every PR without reading paths), and 1 ref for the 1
  base branch: one probe per PR, no path fetches, one ref read per base
  branch. 310 points, 3.96 s.

### Warm cycles before and after

`--warm-cycles 3`, no base moves. "Before" is main at `83dc6761`, the
commit before mergeability probing moved out of the refresh, with this bench's points
arithmetic added to its fake for the run; "after" is the commit that adds
this section. Per warm cycle:

| Shape | Run | REST requests | GraphQL requests | Points | Wall |
| -- | -- | -- | -- | -- | -- |
| `dp1` | before | 301 | 0 | 0 | 3.11s |
| `dp1` | after | 301 | 0 | 0 | 3.15s |
| `stress` | before | 601 | 0 | 0 | 6.70s |
| `stress` | after | 601 | 0 | 0 | 6.74s |
| `dp1 --inflight-pct 0.1` | before | 301 | 23 | 134 | 3.78s |
| `dp1 --inflight-pct 0.1` | after | 301 | 23 | 134 | 3.81s |

The warm cycles cost the same. The cold cycle costs more after, by one ref
read per base branch and one `PRMergeability` query (1 point) per 100 PRs:

| Shape | Run | Cold requests | Cold points | Cold wall |
| -- | -- | -- | -- | -- |
| `dp1` | before | 538 | 1,376 | 20.05s |
| `dp1` | after | 874 | 1,422 | 26.28s |
| `stress` | before | 1,237 | 3,626 | 1m16s |
| `stress` | after | 1,936 | 3,747 | 1m29s |

## CI

`.github/workflows/poll-bench.yml` runs `--shape smoke --check` on changes
to the polling surface (poller, tracker, probe worker, GitHub client, the
bench itself) and on pushes to main. It is deliberately not part of the
PR-blocking `test.yml`; dispatch it manually for ad-hoc runs. `mono` is not
run in CI.
