# Poll-scale benchmark

`cmd/pollbench` measures what GitHub polling costs at large-org shape —
hundreds of tracked repos, thousands of open PRs, GHES-style request
latency. The scenario it models is the one webhooks don't cover: the
**cold-start sync** (first full poll of the tracked set) and post-outage
catch-up, where the poller must list and refresh everything. It is also the
before/after measurement vehicle for polling-path work: rate-limit backoff,
refresh concurrency, and budgeted/resumable cycles.

## What the harness runs

The benchmark drives the **real** production components — `poller.Manager`
→ `tracker.Tracker` → `internal/github.Client` over real (in-memory SQLite)
stores — against a deterministic fake GitHub API generated from a seed. The
fake serves exactly the surface the poll path touches:

- `GET /repos/{o}/{r}/pulls` — the per-repo open-PR listing, with real
  ETag/`If-None-Match` conditional semantics (304s are free on the rate
  limit, matching GitHub) and >100-PR pagination,
- `POST /graphql` — the `nodes(ids:)` batch refresh with the full PR
  fragment (reviews, review requests, labels, check-run rollup, comment and
  file counts),
- `GET /orgs/{owner}/teams` — the per-cycle GitHub-group reconcile read,
- primary-rate-limit `x-ratelimit-*` headers and exhaustion 403s when
  enabled, exercising the client's backoff end to end.

The run is a multi-mode-shaped cycle (org-credential perspective, no
local-user dashboard backfill). Out of scope: Jira polling, the HTTP
API/websocket layer, and event routing/task creation — the harness counts
events at the tracker's publish boundary.

## Running it

```bash
./scripts/poll-bench.sh --shape dp1                 # the design-partner shape
./scripts/poll-bench.sh --shape smoke --latency 0   # fast local sanity run
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
- `--seed` — dataset seed. Same seed + shape ⇒ identical request counts;
  only wall time varies.
- `--check` — CI mode: runs twice and fails on any sanity violation or on
  request counts differing between the runs. The count comparison is
  skipped when a rate limit is active (retry timing varies run-to-run);
  the sanity assertions still apply to both runs.

## Standard shapes

| Shape | Repos | Open PRs (seed 42) | Purpose |
| -- | -- | -- | -- |
| `smoke` | 20 | ~100 | CI tripwire |
| `dp1` | 300 | ~4,600 | Large-org GHES target |
| `stress` | 600 | ~12,100 | Headroom check |

Per-repo PR counts are drawn from a mixed distribution around the shape's
mean (~70% of repos below the mean, ~25% up to 3×, ~5% pathological at
6–10×), so the tail exercises multi-page listings — at `dp1`, several repos
exceed the 100-PR page size.

> The `dp1` numbers are placeholders approximating a large single-org GHES
> deployment (~700 developers). Replace them with the partner's real repo /
> open-PR counts when provided, and record their GHES rate-limit
> configuration here alongside — GHES admins control those limits, so the
> `--rate-limit` value worth testing is deployment-specific.

## What the report shows

Each driven cycle is one row:

- **cold #N** — cycles until every PR has a fully refreshed snapshot. One
  cycle unless a rate limit interrupts; then the sync resumes across cycles
  (already-listed repos come back as free 304s, already-refreshed snapshots
  keep their state).
- **warm #N** — cycles after the sync where nothing changed: the
  conditional-request measurement. `304` vs `list` is the ETag hit rate;
  `graphql` should be 0 at `--inflight-pct 0`.
- **req/list/pages/304/graphql/teams/ratelimited** — request counts by
  class, from the fake server's counters. Deterministic per seed (except
  retry counts under an active rate limit, which are timing-dependent).
- **events** — `github:*` events emitted at the tracker boundary. The
  harness knows exactly which transitions the dataset implies (each
  completed check, each review, each conflicted PR emits once on the first
  full refresh; a warm cycle emits nothing) and fails `sanity` on any
  mismatch — the metrics are only meaningful when the run did the real work.
  `sanity` also fails on any request to an endpoint the fake doesn't model
  (`other` > 0 means the poll path's call set changed — extend the fake) and
  on any poll-cycle error other than a rate-limit interrupt on a run that
  configured `--rate-limit`.
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

## CI

`.github/workflows/poll-bench.yml` runs `--shape smoke --check` on changes
to the polling surface (poller, tracker, GitHub client, the bench itself)
and on pushes to main. It is deliberately not part of the PR-blocking
`test.yml`; dispatch it manually for ad-hoc runs.
