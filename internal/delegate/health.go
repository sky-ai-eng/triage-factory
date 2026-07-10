package delegate

import "time"

// The executor healthz seam (TFAC-582). The localhost-only healthz
// listener in internal/app reports the dispatcher's liveness so a
// container HEALTHCHECK (and the drain/fence tickets that follow) can tell
// a working sandbox worker from a wedged one. These accessors are the
// read-only surface it consults; the fields they read are stamped by
// RunDispatcher (dispatcherRunning) and heartbeatOnce
// (lastHeartbeatWriteNanos), and by the drain verb (draining, TFAC-586).

// DispatcherAlive reports whether the run-queue dispatcher loop is
// currently running. False before RunDispatcher starts, after it returns
// (ctx cancel / shutdown), or when no RunQueueStore was wired.
func (s *Spawner) DispatcherAlive() bool {
	return s.dispatcherRunning.Load()
}

// LastHeartbeatWriteAge returns how long ago the last instance-registry
// heartbeat write succeeded, and whether one has ever succeeded. The
// executor healthz flips to 503 when this age exceeds 3× the heartbeat
// interval (the process is alive enough to answer HTTP but no longer
// renewing its fleet-registry liveness — a partial failure the reaper
// would otherwise have to wait out).
func (s *Spawner) LastHeartbeatWriteAge() (age time.Duration, everWritten bool) {
	ns := s.lastHeartbeatWriteNanos.Load()
	if ns == 0 {
		return 0, false
	}
	return time.Since(time.Unix(0, ns)), true
}

// ActiveRuns reports how many runs currently hold a concurrency slot — the
// off-dispatcher semaphore's occupancy. The executor healthz surfaces it
// (informational) and the drain ticket reads it to know when a draining
// executor has quiesced.
func (s *Spawner) ActiveRuns() int {
	return len(s.semaphore())
}

// Draining reports whether this executor has been asked to quiesce. Set by
// the drain verb (TFAC-586); until that lands it is always false and the
// healthz field is purely informational.
func (s *Spawner) Draining() bool {
	return s.draining.Load()
}

// SetDraining latches (or clears) the drain flag. The drain verb ticket
// owns the caller; exposed here so the field the healthz reports has a
// single writer.
func (s *Spawner) SetDraining(v bool) {
	s.draining.Store(v)
}

// SetReportCapacity toggles whether the heartbeat writes the capacity +
// admission snapshot. Control pods set it false so their registry row
// leaves those executor-only fields NULL; executor/all leave the
// NewSpawner default (true).
func (s *Spawner) SetReportCapacity(v bool) {
	s.reportCapacity.Store(v)
}
