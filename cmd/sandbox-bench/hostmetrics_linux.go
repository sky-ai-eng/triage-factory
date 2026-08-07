//go:build linux

package main

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
)

func numCPU() int { return runtime.NumCPU() }

// readMemAvailableMB reads host MemAvailable from /proc/meminfo. Inside a
// container this is still the host-wide figure (no cgroup memory limit is
// set on the bench container), which is exactly what the guardrail wants.
func readMemAvailableMB() int { return readMeminfoMB("MemAvailable:") }

func readMemTotalMB() int { return readMeminfoMB("MemTotal:") }

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

// readCPUStat reads the whole-host aggregate CPU counters from /proc/stat;
// the plateau sampler diffs two reads over its window. Whole-host on
// purpose — the bench benchmarks hosts, and the per-jail attribution comes
// from each run's cgroup, not from here.
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
