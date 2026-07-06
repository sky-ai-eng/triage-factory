//go:build !unix

package pollbench

// peakRSSBytes is unavailable on non-unix platforms; the report prints n/a.
func peakRSSBytes() int64 { return 0 }
