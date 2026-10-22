//go:build !linux

package sandbox

// CheckStateRootOnDisk has nothing to check off Linux: no jail runs there.
func CheckStateRootOnDisk() error { return nil }
