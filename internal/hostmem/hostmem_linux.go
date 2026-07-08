//go:build linux

package hostmem

import (
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/sky-ai-eng/triage-factory/internal/logging"
)

var hostmemLog = logging.Component("hostmem")

// ambiguousWarned latches the degraded-figures warning so a probe that
// polls every few seconds doesn't spam it — one line per transition into
// the ambiguous state (reset on any clean read), not one per tick. The
// warning matters because Unknown doesn't just blur the numbers: the
// dispatch memory floor treats Unknown as "no gating", so an unreadable
// cgroup file silently DISARMS a guardrail that was armed the moment
// before.
var ambiguousWarned atomic.Bool

// Cgroup v2 unified-hierarchy paths this package reads when the
// process is confined by a memory limit anywhere in its own cgroup
// ancestry. cgroupRoot is the standard mount point; the actual
// per-process cgroup underneath it is resolved via /proc/self/cgroup
// rather than assumed to be the mount root itself — under a private
// cgroup namespace (the default for Docker 20.10+/containerd/Fly
// Machines) the two coincide, so this degrades to a single read
// exactly as before. Under a shared/host cgroup namespace they don't:
// /sys/fs/cgroup is the real kernel root, which typically has no
// memory.max of its own, while the container's actual limit lives
// several levels down (e.g. /system.slice/docker-<id>.scope). Reading
// only the mount root in that shape would see "not limited" and leak
// host-wide /proc/meminfo figures — exactly the bug this package
// exists to avoid.
const (
	cgroupRoot           = "/sys/fs/cgroup"
	cgroupMemMaxFile     = "memory.max"
	cgroupMemCurrentFile = "memory.current"
	cgroupMemStatFile    = "memory.stat"
	procSelfCgroupPath   = "/proc/self/cgroup"
	procMeminfoPath      = "/proc/meminfo"
)

// readFileFunc abstracts a file read so tests can inject a fake
// filesystem (fixed content, or an error) instead of the real /proc
// and /sys/fs/cgroup trees, which vary by host and can't represent
// "unlimited", "nested under a shared namespace", or "unreadable" on
// demand.
type readFileFunc func(path string) ([]byte, error)

func availableMB() int { return availableMBFrom(os.ReadFile) }

func totalMB() int { return totalMBFrom(os.ReadFile) }

// cgroupMemLevel is one ancestor cgroup's memory.max, in bytes.
type cgroupMemLevel struct {
	dir      string
	maxBytes int64
}

// availableMBFrom is AvailableMB with an injectable reader.
func availableMBFrom(read readFileFunc) int {
	levels, limited, ambiguous := cgroupMemLimitedLevels(read)
	if ambiguous {
		return Unknown
	}
	if !limited {
		return meminfoMB(read, "MemAvailable:")
	}
	// Cgroup v2 enforces memory.max hierarchically: any ancestor
	// hitting its own ceiling throttles/OOMs the whole subtree, even
	// if our own leaf looks fine (a shared parent slice can be
	// starved by sibling cgroups). The true headroom is the tightest
	// of every limited level's own (max - used), not just the leaf's.
	minHeadroom := int64(-1)
	for _, lvl := range levels {
		current, ok := readMemoryBytes(read, lvl.dir+"/"+cgroupMemCurrentFile)
		if !ok {
			// Never fall back to /proc/meminfo once a real limit is
			// confirmed somewhere in the chain — that would silently
			// hand back the host-wide numbers this package exists to
			// avoid. Fail open to Unknown instead.
			return Unknown
		}
		inactiveFile, ok := readMemoryStatField(read, lvl.dir+"/"+cgroupMemStatFile, "inactive_file")
		if !ok {
			return Unknown
		}
		used := current - inactiveFile
		if used < 0 {
			used = 0
		}
		headroom := lvl.maxBytes - used
		if headroom < 0 {
			headroom = 0
		}
		if minHeadroom == -1 || headroom < minHeadroom {
			minHeadroom = headroom
		}
	}
	// Cross-clamp against the host: a cgroup allowance is a ceiling, not
	// a reservation — a limit set above physical RAM (systemd
	// MemoryMax=1T on a 64 GB box), or plain overcommit (siblings eating
	// the host while our slice looks roomy), leaves cgroup headroom the
	// machine cannot actually deliver, and allocating into it swaps/OOMs
	// the HOST — the exact outcome the dispatch floor exists to prevent,
	// and one the pre-cgroup meminfo path caught. The truthful figure is
	// the tighter of the two; an unreadable meminfo just skips the clamp
	// (the confirmed cgroup figure is still the best available answer).
	result := int(minHeadroom / (1024 * 1024))
	if host := meminfoMB(read, "MemAvailable:"); host != Unknown && host < result {
		result = host
	}
	return result
}

// totalMBFrom is TotalMB with an injectable reader.
func totalMBFrom(read readFileFunc) int {
	levels, limited, ambiguous := cgroupMemLimitedLevels(read)
	if ambiguous {
		return Unknown
	}
	if !limited {
		return meminfoMB(read, "MemTotal:")
	}
	minMax := levels[0].maxBytes
	for _, lvl := range levels[1:] {
		if lvl.maxBytes < minMax {
			minMax = lvl.maxBytes
		}
	}
	// Cross-clamp against physical RAM — see availableMBFrom: a limit
	// above MemTotal can never be delivered, and reporting it would
	// overstate capacity everywhere this figure flows (boot log, derived
	// capacity, the registry heartbeat, the fleet view).
	result := int(minMax / (1024 * 1024))
	if host := meminfoMB(read, "MemTotal:"); host != Unknown && host < result {
		result = host
	}
	return result
}

// cgroupMemLimitedLevels walks every cgroup from this process's own
// leaf up to the mount root and returns every level that carries a
// real memory.max (parseable, not "max"). limited is false when no
// level in the chain does — every level is either unlimited or has no
// memory.max file at all (not cgrouped: bare metal, or a cgroup
// v1-only host) — so callers fall back to /proc/meminfo.
//
// A file that genuinely doesn't exist (os.IsNotExist) is the expected
// shape for the literal mount root under a shared/host cgroup
// namespace — nothing sits above the true kernel root to limit it —
// and is skipped silently. Any OTHER failure (permission denied, a
// transient I/O error, unparseable content) is ambiguous: that level
// might carry a real, possibly tighter, limit we simply couldn't
// read, and treating it as "no limit here" could understate a real
// constraint — the same class of mistake as falling back to
// host-wide /proc/meminfo. ambiguous=true tells the caller to fail
// toward Unknown instead of trusting whatever levels DID resolve.
func cgroupMemLimitedLevels(read readFileFunc) (levels []cgroupMemLevel, limited, ambiguous bool) {
	selfPath, _ := selfCgroupPath(read)
	var ambiguousDir string
	for _, dir := range cgroupAncestorDirs(selfPath) {
		data, err := read(dir + "/" + cgroupMemMaxFile)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			ambiguous, ambiguousDir = true, dir
			continue
		}
		s := strings.TrimSpace(string(data))
		if s == "max" || s == "" {
			continue
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			ambiguous, ambiguousDir = true, dir
			continue
		}
		levels = append(levels, cgroupMemLevel{dir: dir, maxBytes: n})
	}
	if ambiguous {
		if ambiguousWarned.CompareAndSwap(false, true) {
			hostmemLog.Warn("cgroup memory.max unreadable/unparseable on an ancestor — memory figures degrade to Unknown and the dispatch memory floor is DISARMED until this resolves",
				"cgroup_dir", ambiguousDir)
		}
	} else {
		ambiguousWarned.Store(false)
	}
	return levels, len(levels) > 0, ambiguous
}

// selfCgroupPath reads this process's own cgroup v2 path from
// /proc/self/cgroup's unified-hierarchy line, which always starts
// with "0::" regardless of whether any v1 named hierarchies are also
// present (a hybrid mount). Reports ok=false when the file can't be
// read or carries no "0::" line (a cgroup v1-only host).
func selfCgroupPath(read readFileFunc) (string, bool) {
	data, err := read(procSelfCgroupPath)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

// cgroupAncestorDirs returns every cgroup directory from the mount
// root down to this process's own leaf cgroup (root first). An empty
// or "/" selfPath — a private cgroup namespace, or a self-cgroup read
// that failed — yields just [cgroupRoot], identical to reading the
// mount root directly.
func cgroupAncestorDirs(selfPath string) []string {
	selfPath = strings.Trim(selfPath, "/")
	dirs := []string{cgroupRoot}
	if selfPath == "" {
		return dirs
	}
	dir := cgroupRoot
	for _, seg := range strings.Split(selfPath, "/") {
		if seg == "" {
			continue
		}
		dir += "/" + seg
		dirs = append(dirs, dir)
	}
	return dirs
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
