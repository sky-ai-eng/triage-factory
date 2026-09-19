package workitem

import (
	"math/rand"
	"time"
)

// JitterSource returns a value in [0,1) placing a backoff inside its jitter
// band. A nil source uses the package's own generator.
//
// A function rather than a *rand.Rand so a test can pin the exact value: 0.5
// is the band's midpoint and therefore the pre-jitter delay, which is what
// makes monotonicity assertable without a seeded generator's internals.
type JitterSource func() float64

// Backoff is the retry delay for a failed attempt: min(Cap, Base * 2^(n-1)),
// multiplied by a uniform factor in [1-Jitter, 1+Jitter].
//
// It is a pure function of its inputs, exported so an adopting kind's tests
// can pin the delays its Requeue writes rather than infer them from a stored
// timestamp.
func Backoff(spec BackoffSpec, attempt int, jitter JitterSource) time.Duration {
	s := spec.resolved()
	base := backoffPreJitter(s, attempt)
	if s.Jitter <= 0 {
		return base
	}
	u := rand.Float64()
	if jitter != nil {
		u = jitter()
	}
	if u < 0 {
		u = 0
	} else if u >= 1 {
		// Keep the band half-open at the top so the factor cannot reach
		// 1+Jitter exactly, whatever a caller's source hands back.
		u = 1 - 1e-9
	}
	factor := (1 - s.Jitter) + 2*s.Jitter*u
	return time.Duration(float64(base) * factor)
}

// backoffPreJitter is the delay before jitter, non-decreasing in attempt and
// clamped at Cap. It doubles rather than computing a power so a large attempt
// count saturates instead of overflowing.
func backoffPreJitter(s BackoffSpec, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := s.Base
	for i := 1; i < attempt; i++ {
		if d >= s.Cap {
			return s.Cap
		}
		d *= 2
	}
	if d > s.Cap {
		return s.Cap
	}
	return d
}
