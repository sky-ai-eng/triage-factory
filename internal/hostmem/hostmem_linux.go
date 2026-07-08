//go:build linux

package hostmem

import (
	"os"
	"strconv"
	"strings"
)

// Cgroup v2 unified-hierarchy files this package reads when the process
// is confined by a memory limit. Read at the mount root deliberately —
// modern container runtimes (Docker 20.10+, containerd, Fly Machines)
// give each container its own cgroup namespace, so the container's
// delegated subtree already appears as the root of /sys/fs/cgroup; no
// self-cgroup path resolution (/proc/self/cgroup) is needed.
const (
	cgroupMemMaxPath     = "/sys/fs/cgroup/memory.max"
	cgroupMemCurrentPath = "/sys/fs/cgroup/memory.current"
	cgroupMemStatPath    = "/sys/fs/cgroup/memory.stat"
	procMeminfoPath      = "/proc/meminfo"
)

// readFileFunc abstracts a file read so tests can inject a fake
// filesystem (fixed content, or an error) instead of the real /proc
// and /sys/fs/cgroup trees, which vary by host and can't represent
// "unlimited" or "unreadable" on demand.
type readFileFunc func(path string) ([]byte, error)

func availableMB() int { return availableMBFrom(os.ReadFile) }

func totalMB() int { return totalMBFrom(os.ReadFile) }

// availableMBFrom is AvailableMB with an injectable reader.
func availableMBFrom(read readFileFunc) int {
	maxBytes, limited := cgroupMemMax(read)
	if !limited {
		return meminfoMB(read, "MemAvailable:")
	}
	// Cgrouped: never fall back to /proc/meminfo from here even if a
	// sibling file can't be read — that would silently hand back the
	// host-wide number this package exists to avoid. A broken read
	// inside a confirmed cgroup limit fails open to Unknown instead.
	current, ok := readMemoryBytes(read, cgroupMemCurrentPath)
	if !ok {
		return Unknown
	}
	inactiveFile, ok := readMemoryStatField(read, cgroupMemStatPath, "inactive_file")
	if !ok {
		return Unknown
	}
	used := current - inactiveFile
	if used < 0 {
		used = 0
	}
	avail := maxBytes - used
	if avail < 0 {
		avail = 0
	}
	return int(avail / (1024 * 1024))
}

// totalMBFrom is TotalMB with an injectable reader.
func totalMBFrom(read readFileFunc) int {
	maxBytes, limited := cgroupMemMax(read)
	if !limited {
		return meminfoMB(read, "MemTotal:")
	}
	return int(maxBytes / (1024 * 1024))
}

// cgroupMemMax reads memory.max and reports the limit in bytes plus
// whether this process is actually confined by it. Both "not cgrouped"
// (file missing — bare host, or a cgroup v1-only host) and "unlimited"
// (content "max") report limited=false, so callers fall back to
// /proc/meminfo exactly as the pre-cgroup-aware behavior did.
func cgroupMemMax(read readFileFunc) (limitBytes int64, limited bool) {
	data, err := read(cgroupMemMaxPath)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(data))
	if s == "max" || s == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// readMemoryBytes reads a single plain-integer-bytes cgroup file (e.g.
// memory.current).
func readMemoryBytes(read readFileFunc, path string) (int64, bool) {
	data, err := read(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// readMemoryStatField reads one "key value" line (bytes) out of a
// cgroup v2 memory.stat file.
func readMemoryStatField(read readFileFunc, path, key string) (int64, bool) {
	data, err := read(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == key {
			n, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, false
			}
			return n, true
		}
	}
	return 0, false
}

// meminfoMB reads one kB-denominated field out of /proc/meminfo. Any
// read/parse failure returns Unknown rather than an error — the
// callers' fail-open contract wants a sentinel, not a hard failure.
func meminfoMB(read readFileFunc, key string) int {
	data, err := read(procMeminfoPath)
	if err != nil {
		return Unknown
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, key) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return Unknown
		}
		kb, err := strconv.Atoi(fields[1])
		if err != nil {
			return Unknown
		}
		return kb / 1024
	}
	return Unknown
}
