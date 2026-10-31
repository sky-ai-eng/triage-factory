//go:build !linux

package host

func diskAvailableMB([]string) int { return Unknown }
