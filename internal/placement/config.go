package placement

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Placement tunables, resolved once at boot from env (internal/app). The
// defaults sit inside spec §6.2's stated windows: a ~15-30s tier-2 aging
// delay, and a liveness window a few heartbeats wide (heartbeat is ~4s).
const (
	// DefaultAging is the tier-2 spillover delay — how long a queued conversation
	// waits for its preferred owner before any executor may claim it.
	DefaultAging = 20 * time.Second

	// DefaultLiveness is the heartbeat-staleness window past which a stamped
	// preferred executor is treated as dead (immediate spillover, no aging
	// wait). Wider than one heartbeat so a single missed beat doesn't
	// prematurely abandon affinity, narrower than the reaper's 30s so a
	// genuinely dead owner's conversations spill before the reaper even
	// requeues them.
	DefaultLiveness = 12 * time.Second
)

// ConfigFromEnv resolves the placement Config from environment. Placement is
// ON by default (enabled=true) except when explicitly disabled: TF_PLACEMENT
// in {off,0,false,no,disabled} switches the whole layer off — the feature
// flag the acceptance criterion exercises ("dropping the entire placement
// layer leaves all tests green"). localMode forces it off regardless: local
// is single-process N=1, where the hash always returns self and tier-1
// always hits, so the layer is pure overhead there.
//
// TF_PLACEMENT_AGING_SEC / TF_PLACEMENT_LIVENESS_SEC override the two windows
// (positive integer seconds); empty uses the defaults. A malformed value is
// returned as an error so a typo fails boot loudly rather than silently
// reverting to a default the operator didn't intend.
func ConfigFromEnv(getenv func(string) string, localMode bool) (Config, error) {
	enabled := true
	if raw := strings.TrimSpace(getenv("TF_PLACEMENT")); raw != "" {
		on, err := parseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid TF_PLACEMENT=%q (want on/off)", raw)
		}
		enabled = on
	}
	if localMode {
		enabled = false
	}

	aging, err := parseSeconds(getenv("TF_PLACEMENT_AGING_SEC"), DefaultAging)
	if err != nil {
		return Config{}, fmt.Errorf("TF_PLACEMENT_AGING_SEC: %w", err)
	}
	liveness, err := parseSeconds(getenv("TF_PLACEMENT_LIVENESS_SEC"), DefaultLiveness)
	if err != nil {
		return Config{}, fmt.Errorf("TF_PLACEMENT_LIVENESS_SEC: %w", err)
	}
	return Config{Enabled: enabled, Aging: aging, Liveness: liveness}, nil
}

func parseBool(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "on", "1", "true", "yes", "enabled":
		return true, nil
	case "off", "0", "false", "no", "disabled":
		return false, nil
	default:
		return false, fmt.Errorf("not a boolean")
	}
}

func parseSeconds(raw string, def time.Duration) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("want a positive integer number of seconds, got %q", raw)
	}
	return time.Duration(n) * time.Second, nil
}
