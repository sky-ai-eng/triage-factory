package sandbox

import "github.com/sky-ai-eng/triage-factory/internal/cell"

// RunSample is one in-flight observation of a jail's resource use, read from
// its cgroup while the run is still going: the gVisor backend's cell.Sample.
// It is the sibling of RunActuals, the same cgroup read once at exit.
type RunSample = cell.Sample

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
