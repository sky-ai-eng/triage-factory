//go:build linux

package hoststat

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"
	"time"
)

func read(ctx context.Context, window time.Duration) Sample {
	var s Sample
	if v, ok := sampleCPUPct(ctx, window); ok {
		s.CPUPct = &v
	}
	if v, ok := readLoad1(); ok {
		s.Load1 = &v
	}
	return s
}

// sampleCPUPct measures whole-host CPU utilization over the window by diffing
// /proc/stat aggregate counters. Promoted from the sandbox bench.
func sampleCPUPct(ctx context.Context, window time.Duration) (float64, bool) {
	busy1, total1, ok := readCPUStat()
	if !ok {
		return 0, false
	}
	select {
	case <-ctx.Done():
	case <-time.After(window):
	}
	busy2, total2, ok := readCPUStat()
	if !ok || total2 == total1 {
		return 0, false
	}
	return 100 * float64(busy2-busy1) / float64(total2-total1), true
}

func readCPUStat() (busy, total uint64, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return 0, 0, false
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	var vals []uint64
	for _, fstr := range fields[1:] {
		v, err := strconv.ParseUint(fstr, 10, 64)
		if err != nil {
			break
		}
		vals = append(vals, v)
	}
	if len(vals) < 4 {
		return 0, 0, false
	}
	for _, v := range vals {
		total += v
	}
	idle := vals[3] // idle
	if len(vals) > 4 {
		idle += vals[4] // iowait
	}
	return total - idle, total, true
}

func readLoad1() (float64, bool) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, false
	}
	l1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return l1, true
}

// oomKills reads the cumulative oom_kill counter from cgroup v2 memory.events.
// In a container the cgroup namespace makes /sys/fs/cgroup the container's own
// cgroup root, so its memory.events is this instance's counter. Best-effort:
// any read/parse failure (cgroup v1, no controller, non-container host without
// the field) yields Unknown, which the sampler writes as NULL.
func oomKills() int {
	f, err := os.Open("/sys/fs/cgroup/memory.events")
	if err != nil {
		return Unknown
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "oom_kill" {
			n, err := strconv.Atoi(fields[1])
			if err != nil {
				return Unknown
			}
			return n
		}
	}
	return Unknown
}
