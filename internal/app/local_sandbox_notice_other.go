//go:build !windows

package app

// noteNoLocalSandbox is a no-op off Windows: Linux decides and reports its
// sandbox in checkLocalSandbox, and an operator's explicit opt-out there needs
// no second notice.
func noteNoLocalSandbox() {}
