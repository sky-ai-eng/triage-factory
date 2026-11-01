package github

// maxFetchPages bounds a REST list endpoint to at most this many 100-item
// pages (1,000 items) per call. It covers the SSH signing key listing
// (ListSSHSigningKeys), where no account comes near it, so reaching it means
// a runaway pagination rather than a real listing.
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

// maxOpenPRPages bounds a repository's open-PR listing (ListOpenPRs) at 100
// pages — 10,000 open PRs. Like maxRepoMirrorPages it is a runaway backstop
// above any real repository, not a working limit: GitHub lists open PRs
// newest first, so a listing cut short drops the oldest open PRs from
// discovery, and the poll then works from a wrong open set rather than a
// smaller one. The cost it would bound is small anyway: an unchanged listing
// is one free 304 whatever its size, and a changed one costs a request per
// 100 open PRs. Truncation here is still logged at WARN, never silent.
const maxOpenPRPages = 100
