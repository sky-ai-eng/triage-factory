// Package lease implements the Postgres-backed leader-election primitive
// for the horizontal-scaling control plane (TFAC-583, spec §3): exactly
// one control pod at a time holds the "background-brain" lease and runs
// the leader-elected subsystems (pollers, the event-queue drainer/router,
// the AI managers, sweepers, EE OnReady workers). Every other control pod
// stays a standby, ready to take over.
//
// This package owns ONLY the election mechanics — who holds the lease,
// for how long, and when a holder must self-demote. It knows nothing about
// what "the brain" actually starts or stops; that's internal/app's job,
// driven by the OnAcquire/OnDemote callbacks passed to NewManager.
//
// Local mode and TF_ROLE=all never construct a Manager at all — the
// single process always wins the lease with zero lease I/O (see
// internal/app's role switch). There is no "always-holder" Elector type
// here on purpose: a stub implementation would still tempt a caller into
// wiring lease I/O paths that must never run in that mode.
package lease

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

var leaseLog = logging.Component("lease")

// BackgroundBrainName is the one lease row this ticket's scope defines —
// name-keyed (not a singleton row/table) so per-org brain sharding (the
// epic's P4) is just more rows, not a schema change.
const BackgroundBrainName = "background-brain"

// Default knobs (spec's "Pre-made decisions — do not re-derive"): renew
// every 5s, takeover TTL 20s, self-demote deadline 15s of monotonic time
// since the last successful renewal — strictly less than the TTL, so the
// old holder is provably stopping before a successor can acquire.
const (
	DefaultRenewInterval  = 5 * time.Second
	DefaultTTL            = 20 * time.Second
	DefaultDemoteDeadline = 15 * time.Second
)

// Status is a point-in-time snapshot of one lease's election state, as
// observed by this process — read by /readyz (TFAC-583's `lease` field)
// and, later, the fleet dashboard.
type Status struct {
	Name     string
	HolderID string
	Term     int64
	IsHolder bool
}

// ParseRenewInterval parses TF_LEASE_RENEW_SEC. Empty maps to
// DefaultRenewInterval; anything else must parse as a positive integer
// second count.
func ParseRenewInterval(s string) (time.Duration, error) {
	return parseSecondsEnv(s, "TF_LEASE_RENEW_SEC", DefaultRenewInterval)
}

// ParseTTL parses TF_LEASE_TTL_SEC. Empty maps to DefaultTTL; anything
// else must parse as a positive integer second count.
func ParseTTL(s string) (time.Duration, error) {
	return parseSecondsEnv(s, "TF_LEASE_TTL_SEC", DefaultTTL)
}

// ParseDemoteDeadline parses TF_LEASE_DEMOTE_SEC. Empty maps to
// DefaultDemoteDeadline; anything else must parse as a positive integer
// second count. Callers must separately enforce demoteDeadline < ttl —
// NewManager does, at construction — since that check spans two env vars
// this function can't see in isolation.
func ParseDemoteDeadline(s string) (time.Duration, error) {
	return parseSecondsEnv(s, "TF_LEASE_DEMOTE_SEC", DefaultDemoteDeadline)
}

func parseSecondsEnv(raw, name string, def time.Duration) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid %s=%q (want a positive integer number of seconds)", name, raw)
	}
	return time.Duration(n) * time.Second, nil
}

// KnobsFromEnv resolves the three lease timing knobs from
// TF_LEASE_RENEW_SEC / TF_LEASE_TTL_SEC / TF_LEASE_DEMOTE_SEC, applying
// defaults for unset vars. Returns an error on a malformed value or on a
// demote deadline that isn't strictly less than the TTL — boot refuses to
// start with a configuration that can't guarantee a demoted holder stops
// before a successor is eligible to acquire.
func KnobsFromEnv() (renewInterval, ttl, demoteDeadline time.Duration, err error) {
	renewInterval, err = ParseRenewInterval(os.Getenv("TF_LEASE_RENEW_SEC"))
	if err != nil {
		return 0, 0, 0, err
	}
	ttl, err = ParseTTL(os.Getenv("TF_LEASE_TTL_SEC"))
	if err != nil {
		return 0, 0, 0, err
	}
	demoteDeadline, err = ParseDemoteDeadline(os.Getenv("TF_LEASE_DEMOTE_SEC"))
	if err != nil {
		return 0, 0, 0, err
	}
	if demoteDeadline >= ttl {
		return 0, 0, 0, fmt.Errorf("lease demote deadline (%s) must be strictly less than the takeover TTL (%s)", demoteDeadline, ttl)
	}
	return renewInterval, ttl, demoteDeadline, nil
}
