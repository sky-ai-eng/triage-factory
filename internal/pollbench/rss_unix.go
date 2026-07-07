//go:build unix

package pollbench

import (
	"runtime"
	"syscall"
)

// peakRSSBytes returns the process's high-water resident set size in bytes.
// getrusage reports ru_maxrss in bytes on Darwin and kilobytes on Linux.
func peakRSSBytes() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return ru.Maxrss
	}
	return ru.Maxrss * 1024
}
