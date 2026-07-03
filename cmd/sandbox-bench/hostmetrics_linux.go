//go:build linux

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func numCPU() int { return runtime.NumCPU() }

// readMemAvailableMB reads host MemAvailable from /proc/meminfo.
// Inside a container this is still the host-wide figure (no cgroup
// memory limit is set on the TF service), which is exactly what the
// guardrail wants.
func readMemAvailableMB() int { return readMeminfoMB("MemAvailable:") }

func readSwapUsedMB() int {
	total := readMeminfoMB("SwapTotal:")
	free := readMeminfoMB("SwapFree:")
	if total <= 0 {
		return 0
	}
	return total - free
}

func readMeminfoMB(key string) int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return -1
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, key) {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.Atoi(fields[1])
				return kb / 1024
			}
		}
	}
	return -1
}

func readLoad1() float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return -1
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return -1
	}
	l1, _ := strconv.ParseFloat(fields[0], 64)
	return l1
}

// sampleCPUPct measures whole-host CPU utilization over the window by
// diffing /proc/stat aggregate counters.
func sampleCPUPct(ctx context.Context, window time.Duration) float64 {
	busy1, total1, ok := readCPUStat()
	if !ok {
		return -1
	}
	sleepCtx(ctx, window)
	busy2, total2, ok := readCPUStat()
	if !ok || total2 == total1 {
		return -1
	}
	return 100 * float64(busy2-busy1) / float64(total2-total1)
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

// totalTreeRSSMB sums the RSS of every live sandbox's full process
// tree (runsc parent + sentry + gofer + the sandboxed workload as
// accounted by the sentry).
//
// Caution: sum-of-RSS overstates at high N — every sentry/gofer maps
// the same static runsc binary, and RSS counts those shared text
// pages once per process. totalTreePSSMB is the honest aggregate.
func totalTreeRSSMB(live []*handle) int {
	pageSize := os.Getpagesize()
	var totalBytes int64
	for _, h := range live {
		if h.cmd.Process == nil {
			continue
		}
		for _, pid := range descendantPIDs(h.cmd.Process.Pid) {
			totalBytes += rssBytes(pid, pageSize)
		}
	}
	return int(totalBytes / (1024 * 1024))
}

// totalTreePSSMB sums PSS (proportional set size — shared pages
// divided across their mappers) over every live sandbox tree. This is
// the number to use for capacity planning.
func totalTreePSSMB(live []*handle) int {
	var totalKB int64
	for _, h := range live {
		if h.cmd.Process == nil {
			continue
		}
		for _, pid := range descendantPIDs(h.cmd.Process.Pid) {
			totalKB += pssKB(pid)
		}
	}
	return int(totalKB / 1024)
}

func pssKB(pid int) int64 {
	f, err := os.Open(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	if err != nil {
		return 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Pss:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseInt(fields[1], 10, 64)
				return kb
			}
		}
	}
	return 0
}

// descendantPIDs walks /proc/<pid>/task/*/children to collect the pid
// and all its descendants. Races with exiting processes are fine —
// missing entries are skipped.
func descendantPIDs(root int) []int {
	seen := map[int]bool{}
	stack := []int{root}
	var out []int
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		out = append(out, pid)
		taskDir := fmt.Sprintf("/proc/%d/task", pid)
		tids, err := os.ReadDir(taskDir)
		if err != nil {
			continue
		}
		for _, tid := range tids {
			data, err := os.ReadFile(filepath.Join(taskDir, tid.Name(), "children"))
			if err != nil {
				continue
			}
			for _, c := range strings.Fields(string(data)) {
				if child, err := strconv.Atoi(c); err == nil {
					stack = append(stack, child)
				}
			}
		}
	}
	return out
}

func rssBytes(pid, pageSize int) int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	raw := string(data)
	rpar := strings.LastIndexByte(raw, ')')
	if rpar < 0 {
		return 0
	}
	fields := strings.Fields(raw[rpar+2:])
	if len(fields) < 22 {
		return 0
	}
	rssPages, _ := strconv.ParseInt(fields[21], 10, 64)
	return rssPages * int64(pageSize)
}
