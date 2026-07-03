// sandbox-bench measures the host-load cost of concurrent gVisor agent
// sandboxes to find the practical (soft) concurrency cap on a given
// machine. The hard cap is 256 (the subnet allocator); this tool finds
// where performance degrades before that.
//
// It ramps the number of live sandboxes through a series of plateaus,
// holds each one, and records: spawn/ready latency for the sandboxes
// added at that level, a canary probe (full Wrap→run→Close of one
// extra sandbox while N are live), host memory/CPU/load, and the
// summed RSS of every sandbox process tree.
//
// Workload profiles emulate what a real delegated agent costs
// (measured against headless Claude Code runs):
//
//	agent — allocates ~400MB and burns ~5% of a core, writes a little
//	        to /work each cycle. The realistic profile.
//	idle  — sleeps. Isolates pure per-sandbox overhead (gVisor sentry,
//	        gofer, netns/iptables).
//	cpu   — busy-loops a full core. Worst-case CPU pressure.
//
// Must run as root on Linux with runsc + iptables + iproute2 present —
// in practice, inside the TF runtime container (see
// scripts/sandbox-bench.sh) so netns/iptables churn stays off the host.
//
// Guardrails: the ramp aborts (and tears everything down) if host
// MemAvailable drops below -mem-floor-mb or load1 exceeds -load-max.
// The bench never swaps the host to death; hitting a guardrail IS a
// result — it marks the soft cap for that profile.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type benchConfig struct {
	levels      []int
	hold        time.Duration
	profile     string
	rssMB       int
	dutyPct     int
	blobMB      int
	sdkDir      string
	turns       int
	jscJIT      bool
	debugStream bool
	memLimitMB  int
	burst       int
	csvPath     string
	memFloorMB  int
	loadMax     float64
	workRoot    string
}

func main() {
	var cfg benchConfig
	var levelsStr string
	flag.StringVar(&levelsStr, "levels", "4,8,16,32,64,96,128,192,256",
		"comma-separated ascending plateau sizes (live sandboxes)")
	flag.DurationVar(&cfg.hold, "hold", 45*time.Second,
		"how long to hold each plateau before measuring")
	flag.StringVar(&cfg.profile, "profile", "agent",
		"workload inside each sandbox: agent | idle | cpu | pageshare | claude")
	flag.IntVar(&cfg.blobMB, "blob-mb", 512,
		"size of the shared read-only blob for the pageshare profile (MB)")
	flag.StringVar(&cfg.sdkDir, "sdk-dir", "/opt/triagefactory/sdk",
		"host path of the agent SDK install for the claude profile (bind-mounted at /sdk)")
	flag.IntVar(&cfg.turns, "turns", 6,
		"tool-use turns the mock endpoint drives per run in the claude profile")
	flag.BoolVar(&cfg.jscJIT, "jsc-jit", false,
		"leave the engine's JSC JIT enabled in the claude profile (default off, matching production)")
	flag.IntVar(&cfg.memLimitMB, "mem-limit-mb", 0,
		"per-sandbox cgroup memory ceiling in MiB (0 = no limit), for validating TF_RUN_MEMORY_LIMIT_MB behavior")
	flag.BoolVar(&cfg.debugStream, "debug-stream", false,
		"echo each workload stdout line and mock-endpoint hit (diagnosing the claude profile)")
	flag.IntVar(&cfg.rssMB, "rss-mb", 400,
		"per-sandbox memory the agent profile allocates (MB)")
	flag.IntVar(&cfg.dutyPct, "duty-pct", 5,
		"per-sandbox CPU duty cycle for the agent profile (percent of one core)")
	flag.IntVar(&cfg.burst, "burst", 8,
		"max concurrent sandbox bring-ups (also exercises the iptables lock)")
	flag.StringVar(&cfg.csvPath, "csv", "sandbox-bench.csv",
		"where to write per-level results")
	flag.IntVar(&cfg.memFloorMB, "mem-floor-mb", 6144,
		"abort the ramp when host MemAvailable drops below this")
	flag.Float64Var(&cfg.loadMax, "load-max", 0,
		"abort the ramp when load1 exceeds this (0 = 3x num CPUs)")
	flag.StringVar(&cfg.workRoot, "work-root", "",
		"parent dir for per-sandbox worktrees (default: a temp dir)")
	flag.Parse()

	var err error
	cfg.levels, err = parseLevels(levelsStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-bench: %v\n", err)
		os.Exit(2)
	}
	switch cfg.profile {
	case "agent", "idle", "cpu", "pageshare", "claude":
	default:
		fmt.Fprintf(os.Stderr, "sandbox-bench: unknown -profile %q\n", cfg.profile)
		os.Exit(2)
	}
	if cfg.dutyPct < 0 || cfg.dutyPct > 100 {
		fmt.Fprintf(os.Stderr, "sandbox-bench: -duty-pct must be between 0 and 100, got %d\n", cfg.dutyPct)
		os.Exit(2)
	}

	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-bench: %v\n", err)
		os.Exit(1)
	}
}

func parseLevels(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	levels := make([]int, 0, len(parts))
	prev := 0
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("bad level %q: %w", p, err)
		}
		if n <= prev {
			return nil, fmt.Errorf("levels must be ascending positive ints, got %q", s)
		}
		if n > 256 {
			return nil, fmt.Errorf("level %d exceeds the 256 hard cap (subnet allocator)", n)
		}
		levels = append(levels, n)
		prev = n
	}
	if len(levels) == 0 {
		return nil, fmt.Errorf("no levels given")
	}
	return levels, nil
}
