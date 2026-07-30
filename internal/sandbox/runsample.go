package sandbox

// RunSample is one in-flight observation of a jail's live resource use, read
// from its cgroup while the run is still going. It is the sibling of
// RunActuals: same cgroup, same kernel truth for the whole sandbox tree, but
// taken repeatedly during the run rather than once at exit.
//
// The difference is what each can answer. RunActuals gives the end-state
// scalars — the high-water peak and the final cumulative CPU — which is the
// billing-grade record and the only thing readable at teardown. A series of
// RunSamples gives the shape: when the run grew, how long it sat flat, whether
// the peak was a spike or a plateau. Neither substitutes for the other, and a
// sampled series will read slightly under the teardown peak because a
// periodic read misses sub-minute spikes — that is expected, not a bug to
// reconcile.
//
// Both fields are pointers so "not observed" stays distinguishable from a
// measured value, zero included. Absent means the file wasn't readable at that
// instant, which for a sampler is routine: a jail torn down between choosing
// what to sample and reading it yields nothing at all.
type RunSample struct {
	// MemCurrentMB is the jail's resident memory in MiB (memory.current),
	// rounded up. nil when unobserved.
	MemCurrentMB *int
	// CPUUsecCum is cumulative CPU time in microseconds (cpu.stat's
	// usage_usec), user + system across the whole jail — the same counter
	// RunActuals.CPUUsec reads at exit, so consecutive samples are
	// monotonically non-decreasing. nil when unobserved.
	CPUUsecCum *int64
}

// Observed reports whether the sample carries any measurement at all. A
// sampler uses it to decide whether the observation is worth recording: a
// sample with neither field is a jail that vanished mid-tick, not a jail that
// used nothing.
func (s RunSample) Observed() bool {
	return s.MemCurrentMB != nil || s.CPUUsecCum != nil
}

// SampleRunCgroup reads one live observation from the cgroup of the run whose
// runsc container id is containerID (Sandbox.ContainerID).
//
// Safe and cheap from the UNPRIVILEGED orchestrator: cgroup v2 stat files are
// world-readable, and the cap-broker's monopoly is on cgroup lifecycle and
// mutation — creating groups, setting limits, destroying them — never on
// observation. Nothing here needs a capability or a broker round trip.
//
// Silent and total-best-effort by contract, because the caller is on a timer:
// an unreadable or absent file yields an absent field with no log line. An
// absent group is the routine case at BOTH ends of a run's life — the group is
// created when the runtime is exec'd, so a launch that is registered but not
// yet started has none, and a finished run's is destroyed with it. Neither is
// worth a log line (one would fire once per launch and once per teardown
// forever), and neither is worth a zero: a sandbox that has not started, and
// one that has already stopped, have both consumed nothing the caller should
// record as usage.
//
// Non-Linux (and an empty container id) returns an unobserved sample.
func SampleRunCgroup(containerID string) RunSample {
	if containerID == "" {
		return RunSample{}
	}
	return sampleRunCgroup(containerID)
}
