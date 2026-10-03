package suspendclock

import (
	"context"
	"time"
)

// Watch calls fn each time the system's cumulative suspended time advances by
// at least threshold, checking every interval, until ctx is done. It returns
// at once on a platform where Suspended reports ok=false.
//
// The base is the reading taken when Watch starts, so a suspend before then
// is never reported. Each check moves the base to the reading it took, so the
// skew and drift the package doc describes never accumulate toward threshold,
// and a step below zero counts as zero. fn runs on the Watch goroutine, so
// the next check waits for it to return.
func Watch(ctx context.Context, interval, threshold time.Duration, fn func(slept time.Duration)) {
	last, ok := Suspended()
	if !ok {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		seen, ok := Suspended()
		if !ok {
			continue
		}
		slept := max(seen-last, 0)
		last = seen
		if slept >= threshold {
			fn(slept)
		}
	}
}
