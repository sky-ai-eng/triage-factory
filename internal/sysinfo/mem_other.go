//go:build !linux

package sysinfo

func memAvailableMB() int { return Unknown }

func memTotalMB() int { return Unknown }
