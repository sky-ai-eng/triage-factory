package github

// maxFetchPages bounds a REST list endpoint to at most this many 100-item
// pages (1,000 items) per resource, per call — TFAC-571. A pathological repo
// (thousands of open PRs, a PR with thousands of changed files) or an
// unusually large App installation grant must not let a paginated fetch run
// away unbounded on every poll cycle. Mirrors teams.go's teamsPageCap,
// generalized for the REST list helpers in pulls.go / events.go — one shared
// ceiling rather than a per-resource constant, same as teamsPageCap covers
// several team-listing loops.
//
// Truncation at the cap is always logged at WARN (never silent) — matches
// CLAUDE.md's "no silent caps" standing rule: an operator needs to see when
// a listing was cut short, not just get a quietly-incomplete result.
const maxFetchPages = 10

// maxRepoMirrorPages bounds the whole-account repository enumerations
// (ListUserReposComplete, ListInstallationReposComplete) at 100 pages —
// 10,000 repositories. It is deliberately an order of magnitude above
// maxFetchPages, because these walks exist to be COMPLETE: their consumers
// treat a truncated answer as unusable rather than as a smaller one, so a cap
// low enough to hit on a real account (a PAT sees every repo across every org
// its user belongs to) doesn't bound the work — it makes that account's
// listing permanently unusable. Total volume is bounded by the callers'
// cadence (the reachable-mirror TTL, the per-poll-cycle grant reconcile), so
// this cap is a runaway backstop above any plausible account, not a working
// limit. Truncation here is still logged at WARN, never silent.
const maxRepoMirrorPages = 100
