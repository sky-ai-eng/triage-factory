//go:build !windows

package auth

// isTransientRenameErr is false off Windows: a refused rename there is a real
// failure, not a sharing conflict that a wait clears.
func isTransientRenameErr(error) bool { return false }
