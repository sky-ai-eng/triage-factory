//go:build !linux

package hostmem

func availableMB() int { return Unknown }

func totalMB() int { return Unknown }
