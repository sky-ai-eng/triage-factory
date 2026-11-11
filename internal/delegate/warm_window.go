package delegate

import (
	"fmt"
	"strings"
	"time"
)

// DefaultWarmWindow is how long an engagement waits warm after a turn a
// person took part in, holding its claim so the person's next message starts
// the next turn on the live engagement instead of a new claim.
const DefaultWarmWindow = 10 * time.Minute

// ParseWarmWindow interprets the TF_CLAIM_WARM_WINDOW env value. Empty is the
// default. A Go duration of zero or more is accepted, and zero turns warm
// claims off. Anything else is an error, which refuses the boot: a typo must
// not hold every executor slot for a window nobody chose.
func ParseWarmWindow(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultWarmWindow, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid TF_CLAIM_WARM_WINDOW=%q (want a duration like \"10m\", or 0 to turn warm claims off)", raw)
	}
	return d, nil
}

// SetWarmWindow sets how long an engagement waits warm after a person's turn.
// Zero turns warm claims off. Call once at startup (internal/app resolves
// TF_CLAIM_WARM_WINDOW); an engagement reads it when its turn ends.
func (s *Spawner) SetWarmWindow(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimWarmWindow = d
}

// WarmWindow is the window SetWarmWindow set, zero when warm claims are off.
// NewSpawner starts it at DefaultWarmWindow.
func (s *Spawner) WarmWindow() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claimWarmWindow
}
