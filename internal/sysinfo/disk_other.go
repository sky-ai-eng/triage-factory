//go:build !linux

package sysinfo

func diskAvailableMB([]string) int { return Unknown }
