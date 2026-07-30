//go:build !linux

package sandbox

// sampleRunCgroup on non-Linux returns an unobserved sample. cgroups don't
// exist outside Linux, and wrap never launches a jail there anyway.
func sampleRunCgroup(_ string) RunSample { return RunSample{} }
