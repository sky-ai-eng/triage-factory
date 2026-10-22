//go:build !linux

package sandbox

// sampleRunCgroup on non-Linux returns an unobserved sample. cgroups don't
// exist outside Linux, and wrap never launches a jail there anyway.
func sampleRunCgroup(_ string) RunSample { return RunSample{} }

// runCgroupOOMKills on non-Linux reads nothing, for the same reason.
func runCgroupOOMKills(_ string) (int, bool) { return 0, false }
