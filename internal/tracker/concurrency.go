package tracker

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// DefaultRepoConcurrency is how many repos RefreshGitHub's discovery phase
// fans out to at once when TF_POLL_REPO_CONCURRENCY is unset. TFAC-570:
// bounded concurrency spreads a big tracked set's cold-start / post-outage
// recovery sync across repos instead of paying every round-trip serially,
// while staying well under GitHub's secondary (burst) rate limits.
const DefaultRepoConcurrency = 4

// MaxRepoConcurrency is the upper clamp on TF_POLL_REPO_CONCURRENCY — high
// enough to meaningfully parallelize a large tracked set, low enough that a
// misconfigured deployment can't trip GitHub's secondary rate limits with a
// burst of concurrent per-repo requests.
const MaxRepoConcurrency = 16

// ParseRepoConcurrency interprets the TF_POLL_REPO_CONCURRENCY env value.
// Empty → the default. Non-numeric or < 1 → the default plus an error the
// caller logs (a bad value must not brick a poll cycle). Values above the
// ceiling clamp to it; clamped reports whether that happened so the caller
// can log a distinct signal for "requested more than the ceiling allows"
// versus "using the plain default." n=1 reproduces the pre-TFAC-570 fully
// serial discovery sweep exactly — same requests, same order.
func ParseRepoConcurrency(raw string) (n int, clamped bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultRepoConcurrency, false, nil
	}
	n, err = strconv.Atoi(raw)
	if err != nil || n < 1 {
		return DefaultRepoConcurrency, false, fmt.Errorf(
			"invalid TF_POLL_REPO_CONCURRENCY %q (want an integer in [1,%d]); using default %d",
			raw, MaxRepoConcurrency, DefaultRepoConcurrency)
	}
	if n > MaxRepoConcurrency {
		return MaxRepoConcurrency, true, nil
	}
	return n, false, nil
}

// repoConcurrency resolves the effective per-repo fan-out width for one
// discovery sweep. Read fresh on every call rather than cached at process
// start: poll cycles are minutes apart, so re-parsing the env var costs
// nothing and lets both tests and operators change the knob without a
// restart.
func repoConcurrency() int {
	n, clamped, err := ParseRepoConcurrency(os.Getenv("TF_POLL_REPO_CONCURRENCY"))
	if err != nil {
		trackerLog.Warn("repo concurrency", "error", err)
	} else if clamped {
		trackerLog.Warn("repo concurrency requested above ceiling; clamped", "cap", n)
	}
	return n
}
