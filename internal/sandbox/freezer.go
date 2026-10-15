package sandbox

import "context"

// Freezer stops a launched run's processes where they stand and lets them go
// again. Pausing is the broker's, since only it supervises the runtime; a run
// launched through it implements this. Pause and Resume are idempotent, and
// both succeed on a run that has already exited, which has nothing to freeze.
type Freezer interface {
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
}
