// sandbox-bench measures what concurrent gVisor sandboxes cost on a host —
// memory AND per-run CPU — to find the practical concurrency cap and emit
// the capacity-model coefficients the sizing docs consume.
//
// It launches the runtime the production executor launches: the native tool
// host (`tf-harness-tools serve`) as the jail's pinned main process, brought
// up through a real cap-broker. The bench spawns that broker by re-exec'ing
// itself with the `cap-broker` subcommand — the same fallback the production
// binary uses on bare metal — so every launch passes the broker's argv pin,
// env allowlist, worktree scope, and mount validation. The bench measures
// the launch path production runs, not a lookalike.
//
// Profiles:
//
//	native       — resident tool host driven with an agent-shaped tool
//	               workload (bash/read/grep/write over a seeded worktree) at
//	               a fixed cadence. The light-triage profile.
//	native-idle  — resident tool host, no tool traffic. Isolates pure
//	               per-sandbox overhead (sentry, gofer, netns, tool host).
//	native-heavy — each jail cycles a build-shaped CPU burst (node compute,
//	               GC churn, child-process spawns — all offline), a
//	               test-shaped memory hold, and idle, per -duty. Staggered
//	               by default like a real fleet; -sync-bursts aligns every
//	               jail's burst for the worst case. The CI/build profile.
//	oomtest      — not a ramp: the blast-radius scenario. Victim jails get
//	               a small cgroup ceiling and allocate past it while
//	               neighbor jails keep working; passes when every victim is
//	               OOM-killed alone, neighbors take zero errors, and
//	               spawning stays flat. Writes a scenario JSON, no CSV.
//
// The bench ramps live sandboxes through ascending plateaus and records per
// level: spawn/ready latency, a canary (one more full network+jail+tool
// cycle while N stay live), per-jail cgroup memory and CPU
// (sandbox.SampleRunCgroup — the same kernel truth production stamps on
// claims, systrap tax included), and whole-host memory/CPU/load. Results
// land in a CSV plus a capacity-model JSON with fitted per-run marginals.
//
// Must run as root on Linux with runsc + iptables + iproute2 present and
// the tool-host binary + SDK at their trusted paths — in practice, inside
// the TF runtime container (see scripts/sandbox-bench.sh).
//
// Guardrails: the ramp aborts (and tears everything down) when host
// MemAvailable drops below -mem-floor-mb or load1 exceeds -load-max.
// Hitting a guardrail IS a result — it marks the soft cap for the profile.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/capbroker"
)

type benchConfig struct {
	levels       []int
	hold         time.Duration
	profile      string
	toolInterval time.Duration
	seedFiles    int
	memLimitMB   int
	burst        int
	csvPath      string
	jsonPath     string
	memFloorMB   int
	loadMax      float64

	// native-heavy knobs: each jail cycles build-burst → memory-hold → idle,
	// with duty the active fraction of the cycle and syncBursts choosing
	// worst-case alignment over the default staggered fleet.
	duty       float64
	burstSec   int
	holdMB     int
	holdSec    int
	syncBursts bool

	// oomtest knobs: victims get a small cgroup ceiling and allocate past
	// it while neighbors keep working — the blast-radius scenario.
	oomNeighbors     int
	oomVictims       int
	oomVictimLimitMB int
}

func main() {
	// The broker re-exec seam: when no broker is listening, capbroker.Start
	// spawns os.Executable() with the `cap-broker` subcommand, exactly as
	// the production binary's CLI dispatch does. Answering it here is what
	// makes the bench a single self-contained binary on any runsc host.
	if len(os.Args) > 1 && os.Args[1] == "cap-broker" {
		capbroker.Handle(os.Args[2:])
		return
	}

	var cfg benchConfig
	var levelsStr string
	flag.StringVar(&levelsStr, "levels", "4,8,16,24,32,48,64,96,128",
		"comma-separated ascending plateau sizes (live sandboxes)")
	flag.DurationVar(&cfg.hold, "hold", 45*time.Second,
		"how long to hold each plateau before measuring")
	flag.StringVar(&cfg.profile, "profile", "native",
		"workload inside each sandbox: native | native-idle | native-heavy")
	flag.DurationVar(&cfg.toolInterval, "tool-interval", 3*time.Second,
		"pause between tool calls per sandbox in the native profile")
	flag.Float64Var(&cfg.duty, "duty", 0.4,
		"native-heavy: active fraction of each jail's burst/hold/idle cycle (0-1]")
	flag.IntVar(&cfg.burstSec, "burst-sec", 15,
		"native-heavy: seconds of build-shaped CPU burst per cycle")
	flag.IntVar(&cfg.holdMB, "hold-mb", 300,
		"native-heavy: resident MB the test-shaped hold phase pins per jail")
	flag.IntVar(&cfg.holdSec, "hold-sec", 15,
		"native-heavy: seconds the memory hold phase lasts per cycle")
	flag.BoolVar(&cfg.syncBursts, "sync-bursts", false,
		"native-heavy: align every jail's burst (worst case) instead of staggering")
	flag.IntVar(&cfg.oomNeighbors, "oom-neighbors", 8,
		"oomtest: healthy jails running the native tool workload while victims die")
	flag.IntVar(&cfg.oomVictims, "oom-victims", 2,
		"oomtest: jails driven past their own memory ceiling")
	flag.IntVar(&cfg.oomVictimLimitMB, "oom-victim-limit-mb", 512,
		"oomtest: the victims' per-run cgroup ceiling (they allocate 2x this)")
	flag.IntVar(&cfg.seedFiles, "seed-files", 120,
		"files seeded into each worktree for the tool workload to chew on")
	flag.IntVar(&cfg.memLimitMB, "mem-limit-mb", 0,
		"per-sandbox cgroup memory ceiling in MiB (0 = the production default)")
	flag.IntVar(&cfg.burst, "burst", 8,
		"max concurrent sandbox bring-ups (also exercises the iptables lock)")
	flag.StringVar(&cfg.csvPath, "csv", "sandbox-bench.csv",
		"where to write per-level results")
	flag.StringVar(&cfg.jsonPath, "json", "sandbox-bench.json",
		"where to write the capacity-model JSON")
	flag.IntVar(&cfg.memFloorMB, "mem-floor-mb", 6144,
		"abort the ramp when host MemAvailable drops below this")
	flag.Float64Var(&cfg.loadMax, "load-max", 0,
		"abort the ramp when load1 exceeds this (0 = 3x num CPUs)")
	flag.Parse()

	var err error
	cfg.levels, err = parseLevels(levelsStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox-bench: %v\n", err)
		os.Exit(2)
	}
	switch cfg.profile {
	case "native", "native-idle", "native-heavy", "oomtest":
	default:
		fmt.Fprintf(os.Stderr, "sandbox-bench: unknown -profile %q\n", cfg.profile)
		os.Exit(2)
	}
	if cfg.toolInterval <= 0 {
		fmt.Fprintf(os.Stderr, "sandbox-bench: -tool-interval must be positive\n")
		os.Exit(2)
	}
	if cfg.duty <= 0 || cfg.duty > 1 {
		fmt.Fprintf(os.Stderr, "sandbox-bench: -duty must be in (0, 1], got %v\n", cfg.duty)
		os.Exit(2)
	}
	if cfg.burstSec <= 0 || cfg.holdSec <= 0 || cfg.holdMB <= 0 {
		fmt.Fprintf(os.Stderr, "sandbox-bench: -burst-sec, -hold-sec, and -hold-mb must be positive\n")
		os.Exit(2)
	}
	if cfg.oomNeighbors < 0 || cfg.oomVictims <= 0 || cfg.oomVictimLimitMB < 128 {
		fmt.Fprintf(os.Stderr, "sandbox-bench: -oom-victims must be positive and -oom-victim-limit-mb at least 128\n")
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
