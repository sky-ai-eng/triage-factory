//go:build !linux

package host

func memAvailableMB() int { return Unknown }

func memTotalMB() int { return Unknown }
