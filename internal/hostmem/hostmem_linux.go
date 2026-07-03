//go:build linux

package hostmem

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

func availableMB() int { return meminfoMB("MemAvailable:") }

func totalMB() int { return meminfoMB("MemTotal:") }

// meminfoMB reads one kB-denominated field out of /proc/meminfo.
// Any read/parse failure returns Unknown rather than an error — the
// callers' fail-open contract wants a sentinel, not a hard failure.
func meminfoMB(key string) int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return Unknown
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
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
