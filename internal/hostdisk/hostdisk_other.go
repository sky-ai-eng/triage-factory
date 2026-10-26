//go:build !linux

package hostdisk

func availableMB([]string) int { return Unknown }
