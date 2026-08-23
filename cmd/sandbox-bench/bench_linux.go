//go:build linux

package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/capbroker"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

const (
	// readyTimeout bounds jail bring-up: Wrap returning is not "ready" —
	// ready is the resident tool host having dialed in AND answered a call.
	readyTimeout = 90 * time.Second
	// toolCallTimeout bounds one workload round trip. Far above any single
	// call's real cost; it catches a wedged host, not a slow command.
	toolCallTimeout = 2 * time.Minute
	// sampleStep is the series sampler's tick: host CPU and every jail's
	// cgroup are read together each step across the whole hold, fine enough
	// to resolve a burst phase inside a jail's cycle.
	sampleStep = 2 * time.Second
	// oomPeakSlackMB is the containment check's allowance above a victim's
	// ceiling: memory.current can read marginally over memory.max (per-CPU
	// charge batching) and the sampler rounds bytes up to whole MiB. Kept
	// well under the smallest ceiling the flag accepts, so a genuinely
	// uncontained victim can never hide inside the slack.
	oomPeakSlackMB = 64
)

// handle is one live benchmark sandbox: the per-run network, the tool-host
// jail, the workload driver's controls, and everything teardown reclaims.
type handle struct {
	id             int
	conversationID string
	claimID        string
	containerID    string
	worktree       string

	net       *sandbox.RunNetwork
	jail      *agentproc.ToolHostJail
	tools     agentloop.ToolHost
	agentSock net.Listener

	stopDriver context.CancelFunc
	driverDone chan struct{}

	toolCalls atomic.Int64
	toolErrs  atomic.Int64
}

// teardown reclaims one sandbox in dependency order: the driver first (its
// in-flight call unblocks when the connection closes), then the jail, then
// the network the jail ran in, then the host-side artifacts. Tolerates a
// partially-constructed handle so failed bring-ups share the same path.
func (h *handle) teardown() {
	if h.stopDriver != nil {
		h.stopDriver()
	}
	if h.tools != nil {
		_ = h.tools.Close()
	}
	if h.driverDone != nil {
		<-h.driverDone
	}
	if h.jail != nil {
		_ = h.jail.Close()
	}
	if h.net != nil {
		_ = h.net.Close()
	}
	if h.agentSock != nil {
		_ = h.agentSock.Close()
	}
	if h.worktree != "" {
		_ = os.RemoveAll(h.worktree)
	}
}

type levelResult struct {
	level        int
	spawnP50     time.Duration // SetupRunNetwork + LaunchToolHost, per sandbox added this level
	spawnP95     time.Duration
	readyP50     time.Duration // spawn start → tool host connected and answering
	readyP95     time.Duration
	canaryMedian time.Duration // full network+jail+tool+teardown cycle while N are live

	jailMemSumMB   int     // Σ memory.current over live jails (mean over ticks)
	jailMemMeanMB  int     // mean memory.current per jail
	jailMemPeakMB  int     // largest single-run memory.current observed
	jailCoresMean  float64 // cycle-averaged CPU cores per jail
	jailCoresPeak  float64 // hottest per-step reading of any single jail
	jailCoresSum   float64
	hostCPUPct     float64 // whole-hold host CPU average
	hostCPUPeakPct float64 // hottest sample step

	memAvailMB int
	memDeltaMB int // pre-ramp MemAvailable minus now: true marginal cost
	swapUsedMB int
	load1      float64

	toolCalls int64
	toolErrs  int64
	failures  int
	aborted   string // non-empty = guardrail/spawn abort reason
}

// actualsLog accumulates each jail's teardown actuals — the same
// billing-grade cgroup read production stamps on claims.
var actualsLog = struct {
	mu       sync.Mutex
	peaksMB  []int
	cpuUsec  []int64
	recorded int
}{}

func recordActuals(_ context.Context, _, _ string, a sandbox.RunActuals) error {
	actualsLog.mu.Lock()
	defer actualsLog.mu.Unlock()
	actualsLog.recorded++
	if a.PeakMemMB != nil {
		actualsLog.peaksMB = append(actualsLog.peaksMB, *a.PeakMemMB)
	}
	if a.CPUUsec != nil {
		actualsLog.cpuUsec = append(actualsLog.cpuUsec, *a.CPUUsec)
	}
	return nil
}

func run(cfg benchConfig) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("must run as root (netns + iptables + chown to sandbox UID)")
	}
	for _, bin := range []string{"runsc", "ip", "iptables"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s not found on PATH", bin)
		}
	}
	if err := runmode.InitFromEnv(); err != nil {
		return err
	}
	if _, err := os.Stat(sandbox.TrustedToolHostBinaryPath()); err != nil {
		return fmt.Errorf("tool-host binary missing at %s — run inside the TF runtime image (scripts/sandbox-bench.sh): %w",
			sandbox.TrustedToolHostBinaryPath(), err)
	}
	if cfg.loadMax == 0 {
		cfg.loadMax = 3 * float64(numCPU())
	}

	// Root context: Ctrl-C / SIGTERM triggers the same orderly teardown
	// path as a completed ramp.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The launch is always brokered. Bring up our own broker the way the
	// production binary does on bare metal — capbroker.Start spawns this
	// binary's cap-broker subcommand when nothing is listening — and install
	// its client as the sandbox package's privileged ops.
	brokerProc, ops, err := capbroker.Start(ctx)
	if err != nil {
		return fmt.Errorf("start cap-broker: %w", err)
	}
	defer func() { _ = brokerProc.Close() }()
	sandbox.SetPrivilegedOps(ops)

	fmt.Printf("host: %d cpus, %d MB available | profile=%s levels=%v hold=%s tool-interval=%s\n",
		numCPU(), readMemAvailableMB(), cfg.profile, cfg.levels, cfg.hold, cfg.toolInterval)

	// Warm-up: one throwaway full cycle. On a cold rootfs cache this pays
	// the alpine download + apk toolchain install (minutes); afterwards the
	// rootfs step is one os.Stat. Keeps the cold cost out of every number.
	fmt.Println("warmup: bringing up one sandbox (cold rootfs cache can take ~10 min)...")
	warmCtx, warmCancel := context.WithTimeout(ctx, 20*time.Minute)
	warmStart := time.Now()
	if err := runOneShot(warmCtx, cfg, 9000); err != nil {
		warmCancel()
		return fmt.Errorf("warmup sandbox failed: %w", err)
	}
	warmCancel()
	fmt.Printf("warmup: ok in %s\n", time.Since(warmStart).Round(time.Millisecond))

	// Guardrail watcher: flips abortReason and cancels spawning when the
	// host crosses a limit. Sampled every 2s.
	guardCtx, guardCancel := context.WithCancel(ctx)
	defer guardCancel()
	var abortMu sync.Mutex
	abortReason := ""
	setAbort := func(reason string) {
		abortMu.Lock()
		if abortReason == "" {
			abortReason = reason
			fmt.Printf("!! guardrail: %s — aborting ramp, tearing down\n", reason)
		}
		abortMu.Unlock()
		guardCancel()
	}
	getAbort := func() string {
		abortMu.Lock()
		defer abortMu.Unlock()
		return abortReason
	}
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-guardCtx.Done():
				return
			case <-tick.C:
				if avail := readMemAvailableMB(); avail < cfg.memFloorMB {
					setAbort(fmt.Sprintf("MemAvailable %d MB < floor %d MB", avail, cfg.memFloorMB))
					return
				}
				if l1 := readLoad1(); l1 > cfg.loadMax {
					setAbort(fmt.Sprintf("load1 %.1f > max %.1f", l1, cfg.loadMax))
					return
				}
			}
		}
	}()

	if cfg.profile == "oomtest" {
		return runOOMTest(guardCtx, cfg, getAbort)
	}

	var live []*handle
	var results []levelResult
	nextID := 0
	canaryID := 9100
	baselineAvailMB := readMemAvailableMB()

	defer func() {
		if len(live) == 0 {
			return
		}
		fmt.Printf("teardown: closing %d sandboxes...\n", len(live))
		start := time.Now()
		teardownAll(live)
		fmt.Printf("teardown: done in %s\n", time.Since(start).Round(time.Millisecond))
	}()

	for _, level := range cfg.levels {
		if reason := getAbort(); reason != "" || ctx.Err() != nil {
			break
		}
		delta := level - len(live)
		fmt.Printf("level %d: spawning %d sandboxes (burst %d)...\n", level, delta, cfg.burst)

		added, spawnTimes, readyTimes, failures := spawnDelta(guardCtx, cfg, &nextID, delta)
		live = append(live, added...)

		res := levelResult{level: level, failures: failures}
		// More than a quarter of the delta failing to come up is itself the
		// soft-cap signal — record and stop rather than measuring a plateau
		// that doesn't exist.
		if failures > delta/4 {
			res.aborted = fmt.Sprintf("%d/%d spawns failed", failures, delta)
			results = append(results, res)
			break
		}
		if reason := getAbort(); reason != "" {
			res.aborted = reason
			results = append(results, res)
			break
		}

		res.spawnP50, res.spawnP95 = percentiles(spawnTimes)
		res.readyP50, res.readyP95 = percentiles(readyTimes)

		// The hold IS the sample: series-sample host CPU and every jail's
		// cgroup across the whole plateau, so bursty profiles report cycle
		// averages and peaks rather than whatever phase one window caught.
		ps := sampleSeries(guardCtx, live, cfg.hold, sampleStep)
		res.jailMemSumMB = ps.jailMemSumMB
		res.jailMemMeanMB = ps.jailMemMeanMB
		res.jailMemPeakMB = ps.jailMemPeakMB
		res.jailCoresMean = ps.jailCoresMean
		res.jailCoresPeak = ps.jailCoresPeak
		res.jailCoresSum = ps.jailCoresSum
		res.hostCPUPct = ps.hostCPUPct
		res.hostCPUPeakPct = ps.hostCPUPeakPct
		res.memAvailMB = readMemAvailableMB()
		res.memDeltaMB = baselineAvailMB - res.memAvailMB
		res.swapUsedMB = readSwapUsedMB()
		res.load1 = readLoad1()
		for _, h := range live {
			res.toolCalls += h.toolCalls.Load()
			res.toolErrs += h.toolErrs.Load()
		}

		// Canary: the marginal cost of one more full run while N are live.
		// Median of 3 network+jail+tool+teardown cycles. At the 256 hard cap
		// there is no 257th subnet, so no canary.
		var canary []time.Duration
		for i := 0; i < 3 && level < 256 && getAbort() == "" && guardCtx.Err() == nil; i++ {
			cStart := time.Now()
			cCtx, cCancel := context.WithTimeout(guardCtx, 2*time.Minute)
			err := runOneShot(cCtx, cfg, canaryID)
			canaryID++
			cCancel()
			if err != nil {
				fmt.Printf("level %d: canary %d failed: %v\n", level, i, err)
				continue
			}
			canary = append(canary, time.Since(cStart))
		}
		if len(canary) > 0 {
			sort.Slice(canary, func(i, j int) bool { return canary[i] < canary[j] })
			res.canaryMedian = canary[len(canary)/2]
		}
		if reason := getAbort(); reason != "" {
			res.aborted = reason
		}

		results = append(results, res)
		fmt.Printf("level %d: spawn p50=%s p95=%s ready p95=%s canary=%s | jail mem=%dMB (%dMB/run, peak %dMB) cores/run=%.2f (peak %.2f) Σ=%.1f | host cpu=%.0f%% (peak %.0f%%) delta=%dMB avail=%dMB load1=%.1f | calls=%d errs=%d fail=%d\n",
			level, res.spawnP50.Round(time.Millisecond), res.spawnP95.Round(time.Millisecond),
			res.readyP95.Round(time.Millisecond), res.canaryMedian.Round(time.Millisecond),
			res.jailMemSumMB, res.jailMemMeanMB, res.jailMemPeakMB,
			res.jailCoresMean, res.jailCoresPeak, res.jailCoresSum,
			res.hostCPUPct, res.hostCPUPeakPct, res.memDeltaMB, res.memAvailMB, res.load1,
			res.toolCalls, res.toolErrs, res.failures)

		if res.aborted != "" {
			break
		}
	}

	if err := writeCSV(cfg.csvPath, cfg.profile, results); err != nil {
		return fmt.Errorf("write csv: %w", err)
	}
	if err := writeCapacityJSON(cfg, results); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	printSummary(results, cfg)
	return nil
}

// oomResult is the blast-radius scenario's JSON: a pass/fail record, not a
// capacity curve.
type oomResult struct {
	GeneratedAt     string          `json:"generated_at"`
	Scenario        string          `json:"scenario"`
	Platform        string          `json:"platform"`
	Host            hostJSON        `json:"host"`
	VictimCeilingMB int             `json:"victim_ceiling_mb"`
	VictimAllocMB   int             `json:"victim_alloc_mb"`
	Victims         int             `json:"victims"`
	Contained       int             `json:"victims_contained"`
	VictimOutcomes  []oomVictimJSON `json:"victim_outcomes"`
	Neighbors       int             `json:"neighbors"`
	NeighborCalls   int64           `json:"neighbor_tool_calls"`
	NeighborErrors  int64           `json:"neighbor_tool_errors"`
	CanaryBeforeMs  int64           `json:"one_more_run_before_ms"`
	CanaryAfterMs   int64           `json:"one_more_run_after_ms"`
	Pass            bool            `json:"pass"`
}

// oomVictimJSON is one victim's fate. mode is "oom-killed" (the jail died
// under the kernel's killer), "alloc-failed" (gVisor surfaced the cgroup
// limit as ENOMEM inside the jail and the run survived its own failure), or
// "uncapped" (the allocation reached its target resident — the failure case).
type oomVictimJSON struct {
	Mode        string  `json:"mode"`
	PeakMB      int     `json:"peak_mb"`
	KillSeconds float64 `json:"kill_seconds,omitempty"`
}

// runOOMTest is the memory blast-radius scenario: victims are driven past
// their own cgroup ceiling while neighbors keep working. The contract under
// test is containment, not a specific death: the ceiling must hold (the
// victim's resident memory never exceeds it) and the failure must land
// inside the offending run — as a kernel OOM kill of that jail, or as
// gVisor surfacing ENOMEM to the allocating process — while no neighbor
// takes an error and spawning stays flat.
func runOOMTest(ctx context.Context, cfg benchConfig, getAbort func() string) error {
	allocMB := cfg.oomVictimLimitMB * 2
	fmt.Printf("oomtest: %d neighbors on the agent workload; %d victims with a %d MB ceiling allocating %d MB\n",
		cfg.oomNeighbors, cfg.oomVictims, cfg.oomVictimLimitMB, allocMB)

	var live []*handle
	defer func() { teardownAll(live) }()

	nextID := 0
	neighbors, _, _, failures := spawnDelta(ctx, cfg, &nextID, cfg.oomNeighbors)
	live = append(live, neighbors...)
	if failures > 0 {
		return fmt.Errorf("oomtest: %d/%d neighbor spawns failed — aborting before the scenario", failures, cfg.oomNeighbors)
	}

	canaryBefore, err := timeOneShot(ctx, cfg, 9300)
	if err != nil {
		return fmt.Errorf("oomtest: pre-kill canary failed: %w", err)
	}

	// Victims: no driver — each gets one oversized hold call, issued
	// concurrently, and dies mid-request when the kernel kills its jail.
	victimCfg := cfg
	victimCfg.memLimitMB = cfg.oomVictimLimitMB
	var victims []*handle
	for i := 0; i < cfg.oomVictims; i++ {
		v, _, _, err := startSandbox(ctx, victimCfg, 9400+i, false)
		if err != nil {
			return fmt.Errorf("oomtest: victim %d spawn failed: %w", i, err)
		}
		live = append(live, v)
		victims = append(victims, v)
	}

	killStart := time.Now()
	// Track each victim's cgroup high-water mark while it grows: it is the
	// arbiter between the possible outcomes. Peaking at the ceiling and
	// dying is the kill; peaking at the ceiling and answering politely is
	// in-jail allocation failure; sailing past it means no ceiling applied.
	victimPeak := make([]int, len(victims))
	sampleCtx, stopSampling := context.WithCancel(ctx)
	var sampleWG sync.WaitGroup
	sampleWG.Add(1)
	go func() {
		defer sampleWG.Done()
		for sampleCtx.Err() == nil {
			for i, v := range victims {
				if s := sandbox.SampleRunCgroup(v.containerID); s.MemCurrentMB != nil && *s.MemCurrentMB > victimPeak[i] {
					victimPeak[i] = *s.MemCurrentMB
				}
			}
			sleepCtx(sampleCtx, 300*time.Millisecond)
		}
	}()

	// The wire outcome per victim: "jail-died" (transport error — the kill
	// took the jail), "reached-target" (the grow printed its completion line
	// — the ceiling never stopped it), or "alloc-failed" (any answered call
	// that never reached target: gVisor surfaced the cgroup limit as ENOMEM
	// and the allocating process failed inside the jail).
	wireOutcome := make([]string, len(victims))
	var callWG sync.WaitGroup
	for i, v := range victims {
		callWG.Add(1)
		go func(i int, v *handle) {
			defer callWG.Done()
			out, err := v.tools.Call("bash", map[string]any{
				"command": fmt.Sprintf("node /work/tf-grow.mjs %d", allocMB),
				"timeout": 150,
			})
			tail := func(s string) string {
				if len(s) > 220 {
					return "…" + s[len(s)-220:]
				}
				return s
			}
			switch {
			case err != nil:
				wireOutcome[i] = "jail-died"
				fmt.Printf("  victim %d: transport died mid-call: %v\n", i, err)
			case out.Protocol != nil:
				wireOutcome[i] = "jail-died"
				fmt.Printf("  victim %d: protocol error: %s\n", i, out.Protocol.Error())
			case strings.Contains(out.Content, "target reached") || strings.Contains(out.ToolError, "target reached"):
				wireOutcome[i] = "reached-target"
				fmt.Printf("  victim %d: allocation REACHED TARGET — the ceiling did not stop it: %s\n", i, tail(out.Content))
			case out.ToolError != "":
				wireOutcome[i] = "alloc-failed"
				fmt.Printf("  victim %d: allocation failed inside the jail (tool error): %s\n", i, tail(out.ToolError))
			default:
				wireOutcome[i] = "alloc-failed"
				fmt.Printf("  victim %d: allocation failed inside the jail: %s\n", i, tail(out.Content))
			}
		}(i, v)
	}
	callWG.Wait()

	// Give a kernel kill a moment to register before reading each fate.
	sleepCtx(ctx, 3*time.Second)
	stopSampling()
	sampleWG.Wait()

	contained := 0
	outcomes := make([]oomVictimJSON, len(victims))
	for i, v := range victims {
		o := oomVictimJSON{PeakMB: victimPeak[i], Mode: wireOutcome[i]}
		if v.jail.OOMKilled() {
			o.Mode = "oom-killed"
			o.KillSeconds = time.Since(killStart).Seconds()
		}
		// Containment = the breach was actually observed (a zero peak means
		// the sampler never saw this victim — evidence of nothing), the
		// ceiling held within the slack, AND the failure landed inside this
		// run.
		if o.Mode != "reached-target" && o.PeakMB > 0 && o.PeakMB <= cfg.oomVictimLimitMB+oomPeakSlackMB {
			contained++
		}
		outcomes[i] = o
		fmt.Printf("  victim %d: %s · cgroup high-water %d MB (ceiling %d MB)\n", i, o.Mode, o.PeakMB, cfg.oomVictimLimitMB)
	}

	// Neighbors keep working through and past the kills before judgment.
	sleepCtx(ctx, 20*time.Second)
	var nCalls, nErrs int64
	for _, n := range neighbors {
		nCalls += n.toolCalls.Load()
		nErrs += n.toolErrs.Load()
	}

	canaryAfter, err := timeOneShot(ctx, cfg, 9301)
	if err != nil {
		return fmt.Errorf("oomtest: post-kill canary failed: %w", err)
	}

	pass := contained == len(victims) && nErrs == 0 &&
		canaryAfter < 3*canaryBefore && getAbort() == ""

	res := oomResult{
		GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
		Scenario:        "oom-blast-radius",
		Platform:        "gvisor-systrap",
		Host:            hostJSON{CPUs: numCPU(), MemTotalMB: readMemTotalMB()},
		VictimCeilingMB: cfg.oomVictimLimitMB,
		VictimAllocMB:   allocMB,
		Victims:         len(victims),
		Contained:       contained,
		VictimOutcomes:  outcomes,
		Neighbors:       len(neighbors),
		NeighborCalls:   nCalls,
		NeighborErrors:  nErrs,
		CanaryBeforeMs:  canaryBefore.Milliseconds(),
		CanaryAfterMs:   canaryAfter.Milliseconds(),
		Pass:            pass,
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfg.jsonPath, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write json: %w", err)
	}

	fmt.Printf("\n=== oom blast-radius ===\n")
	fmt.Printf("victims: %d/%d contained (ceiling %d MB, attempted %d MB)\n",
		contained, len(victims), cfg.oomVictimLimitMB, allocMB)
	fmt.Printf("neighbors: %d jails, %d tool calls, %d errors during and after the breaches\n",
		len(neighbors), nCalls, nErrs)
	fmt.Printf("one more run: %s before → %s after\n",
		canaryBefore.Round(time.Millisecond), canaryAfter.Round(time.Millisecond))
	if pass {
		fmt.Println("PASS: the ceiling holds and memory failure stays inside the offending run")
	} else {
		fmt.Println("FAIL: see the numbers above — the containment contract did not hold")
	}
	fmt.Printf("results: %s (no CSV for scenarios)\n", cfg.jsonPath)
	if !pass {
		return fmt.Errorf("oomtest failed")
	}
	return nil
}

// timeOneShot measures one full network+jail+tool+teardown cycle.
func timeOneShot(ctx context.Context, cfg benchConfig, id int) (time.Duration, error) {
	start := time.Now()
	cCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := runOneShot(cCtx, cfg, id); err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

// spawnDelta brings up `delta` new sandboxes with at most cfg.burst
// concurrent bring-ups. Returns the started handles plus spawn and ready
// timings.
func spawnDelta(ctx context.Context, cfg benchConfig, nextID *int, delta int) (added []*handle, spawnTimes, readyTimes []time.Duration, failures int) {
	sem := make(chan struct{}, cfg.burst)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < delta; i++ {
		id := *nextID
		*nextID = id + 1
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				mu.Lock()
				failures++
				mu.Unlock()
				return
			}
			h, spawnDur, readyDur, err := startSandbox(ctx, cfg, id, cfg.profile != "native-idle")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures++
				fmt.Printf("  spawn %d failed: %v\n", id, err)
				return
			}
			added = append(added, h)
			spawnTimes = append(spawnTimes, spawnDur)
			readyTimes = append(readyTimes, readyDur)
		}()
	}
	wg.Wait()
	return added, spawnTimes, readyTimes, failures
}

// startSandbox brings up one native jail exactly as the executor does:
// per-run network first, then the tool-host jail into it, then the accepted
// connection the workload drives. Ready means the resident host answered a
// real tool call, not merely that the jail process started.
func startSandbox(ctx context.Context, cfg benchConfig, id int, driven bool) (*handle, time.Duration, time.Duration, error) {
	conversationID := fmt.Sprintf("bench-%04d", id)
	h := &handle{id: id, conversationID: conversationID, claimID: "bench-claim-" + conversationID}
	fail := func(err error) (*handle, time.Duration, time.Duration, error) {
		h.teardown()
		return nil, 0, 0, err
	}

	worktree := sandbox.RunTreeRoot(conversationID)
	if err := seedWorktree(worktree, cfg.seedFiles); err != nil {
		return fail(fmt.Errorf("seed worktree: %w", err))
	}
	h.worktree = worktree

	start := time.Now()
	rn, err := sandbox.SetupRunNetwork(ctx, conversationID)
	if err != nil {
		return fail(fmt.Errorf("run network: %w", err))
	}
	h.net = rn

	// The broker validates the agenthost mount source against its own
	// per-run derivation and requires it to exist; in production the run's
	// credential sidecar binds it during bring-up. The bench has no sidecar
	// and its workload runs no exec verbs, so a bound-but-unserved socket at
	// the trusted path satisfies the launch without changing what the jail
	// sees on disk.
	agentMount := agenthost.SocketMountFor(conversationID)
	if err := os.MkdirAll(filepath.Dir(agentMount.Source), 0o700); err != nil {
		return fail(fmt.Errorf("create agenthost socket dir: %w", err))
	}
	_ = os.Remove(agentMount.Source)
	ln, err := net.Listen("unix", agentMount.Source)
	if err != nil {
		return fail(fmt.Errorf("bind agenthost socket: %w", err))
	}
	h.agentSock = ln

	jail, err := agentproc.LaunchToolHost(ctx, agentproc.ToolHostOptions{
		ConversationID:       conversationID,
		Worktree:             worktree,
		PrebuiltNetwork:      rn,
		AgentHostSocket:      agentMount,
		MemoryLimitMB:        cfg.memLimitMB,
		OrgID:                "bench",
		ClaimID:              h.claimID,
		RecordSandboxActuals: recordActuals,
	})
	if err != nil {
		return fail(fmt.Errorf("launch tool host: %w", err))
	}
	h.jail = jail
	spawnDur := time.Since(start)

	conn, err := jail.Accept(ctx, readyTimeout)
	if err != nil {
		return fail(fmt.Errorf("tool host never connected: %w", err))
	}
	h.tools = agentloop.NewToolHost(conn, toolCallTimeout)

	out, err := h.tools.Call("ls", map[string]any{"path": "/work"})
	if err != nil {
		return fail(fmt.Errorf("first tool call: %w", err))
	}
	if out.Protocol != nil {
		return fail(fmt.Errorf("first tool call: %s", out.Protocol.Error()))
	}
	readyDur := time.Since(start)

	// The registry entry is written before LaunchToolHost returns, so a miss
	// here is a wiring bug — and a jail without a container id would silently
	// vanish from cgroup sampling (skewed coefficients) and from the oomtest
	// peak tracking (a hollow containment pass). Fail loudly instead.
	h.containerID = containerIDFor(h.claimID)
	if h.containerID == "" {
		return fail(fmt.Errorf("no live-jail registry entry for claim %s", h.claimID))
	}

	if driven {
		dctx, cancel := context.WithCancel(context.Background())
		h.stopDriver = cancel
		h.driverDone = make(chan struct{})
		if cfg.profile == "native-heavy" {
			go h.driveHeavy(dctx, cfg)
		} else {
			go h.drive(dctx, cfg.toolInterval)
		}
	}
	return h, spawnDur, readyDur, nil
}

// runOneShot is the warmup/canary path: one full network+jail cycle, one
// real tool call, teardown — the whole lifecycle of "one more run" started
// while the plateau stays live.
func runOneShot(ctx context.Context, cfg benchConfig, id int) error {
	h, _, _, err := startSandbox(ctx, cfg, id, false)
	if err != nil {
		return err
	}
	defer h.teardown()
	out, err := h.tools.Call("bash", map[string]any{"command": "true", "timeout": 60})
	if err != nil {
		return err
	}
	if out.Protocol != nil {
		return fmt.Errorf("bash call: %s", out.Protocol.Error())
	}
	if out.ToolError != "" {
		return fmt.Errorf("bash call failed: %s", out.ToolError)
	}
	return nil
}

// containerIDFor resolves the runsc container id for a claim through the
// same registry the production resource sampler reads.
func containerIDFor(claimID string) string {
	for _, j := range agentproc.LiveJails() {
		if j.ClaimID == claimID {
			return j.ContainerID
		}
	}
	return ""
}

// call dispatches one workload tool call, counts it, and reports whether the
// driver can continue — false on a dead transport or a fatal protocol error.
func (h *handle) call(name string, args map[string]any) bool {
	out, err := h.tools.Call(name, args)
	h.toolCalls.Add(1)
	if err != nil {
		h.toolErrs.Add(1)
		return false
	}
	if out.Protocol != nil {
		h.toolErrs.Add(1)
		return !out.Protocol.Fatal()
	}
	if out.ToolError != "" {
		h.toolErrs.Add(1)
	}
	return true
}

// drive issues the agent-shaped workload until canceled: one tool call per
// interval, cycling bash / read / grep / write over the seeded tree. Bash
// dominates the mix deliberately — fork/exec is gVisor's most expensive
// pattern and the dominant in-jail cost of a real native run.
func (h *handle) drive(ctx context.Context, interval time.Duration) {
	defer close(h.driverDone)
	// A ticker, not per-iteration timers: the cadence is part of the CPU
	// coefficient, and sleeping AFTER each call would stretch the true
	// period to interval + call latency.
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		name, args := workloadCall(i)
		if !h.call(name, args) {
			return
		}
	}
}

// driveHeavy cycles the CI-shaped phases until canceled: a build burst, a
// memory hold, then idle sized so the active phases are cfg.duty of the
// cycle. Alignment is the load model: staggered offsets make burst overlap
// statistical like a real fleet; -sync-bursts phase-locks every jail to the
// same wall-clock grid for the worst case, re-aligning each cycle so tool
// latency jitter can't decohere the fleet mid-ramp.
func (h *handle) driveHeavy(ctx context.Context, cfg benchConfig) {
	defer close(h.driverDone)
	active := time.Duration(cfg.burstSec+cfg.holdSec) * time.Second
	cycle := time.Duration(float64(active) / cfg.duty)
	untilNextBoundary := func() time.Duration {
		return cycle - time.Duration(time.Now().UnixNano())%cycle
	}
	var wait time.Duration
	if cfg.syncBursts {
		wait = untilNextBoundary()
	} else {
		wait = time.Duration(int64(h.id)*7919%cycle.Milliseconds()) * time.Millisecond
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(wait):
	}
	for {
		if !h.call("bash", map[string]any{
			"command": fmt.Sprintf("node /work/tf-build.mjs %d 4", cfg.burstSec),
			"timeout": cfg.burstSec + 90,
		}) {
			return
		}
		if !h.call("bash", map[string]any{
			"command": fmt.Sprintf("node /work/tf-hold.mjs %d %d", cfg.holdMB, cfg.holdSec),
			"timeout": cfg.holdSec + 60,
		}) {
			return
		}
		idle := cycle - active
		if cfg.syncBursts {
			idle = untilNextBoundary()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(idle):
		}
	}
}

func workloadCall(i int) (string, map[string]any) {
	switch i % 6 {
	case 0:
		return "bash", map[string]any{"command": "ls -R /work | wc -l", "timeout": 60}
	case 1:
		return "read", map[string]any{"path": "/work/src/f0010.txt"}
	case 2:
		return "bash", map[string]any{"command": "cp /work/src/f0001.txt /work/scratch.txt && wc -l /work/scratch.txt", "timeout": 60}
	case 3:
		return "grep", map[string]any{"pattern": "needle", "path": "/work/src"}
	case 4:
		return "write", map[string]any{"path": "/work/notes.txt", "content": strings.Repeat("benchmark heartbeat line\n", 80)}
	default:
		return "bash", map[string]any{"command": "sh -c 'i=0; while [ $i -lt 3 ]; do date; i=$((i+1)); done' > /work/dates.txt && wc -l /work/dates.txt", "timeout": 60}
	}
}

// tfBuildMJS is the build-shaped burst the heavy profile runs inside the
// jail: real node executing real compute — JSON round trips, regex work,
// hashing (JIT + GC churn, the syscall class gVisor emulates dearest) — plus
// periodic child-process spawns (a build's worker storm). Offline by
// construction: bench jails have no sidecar and therefore no egress, so this
// stands in for npm/tsc work at the syscall level rather than literally
// installing anything.
const tfBuildMJS = `import { readFileSync, readdirSync, writeFileSync, mkdirSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
const sec = Number(process.argv[2] || 15);
const workers = Number(process.argv[3] || 4);
const deadline = Date.now() + sec * 1000;
const srcDir = '/work/src';
const files = readdirSync(srcDir).filter((f) => f.endsWith('.txt'));
const sources = files.map((f) => readFileSync(srcDir + '/' + f, 'utf8'));
mkdirSync('/work/dist', { recursive: true });
let n = 0;
let out = '';
while (Date.now() < deadline) {
  for (const s of sources) {
    const doc = { body: s, toks: s.split(/\s+/), n };
    const round = JSON.parse(JSON.stringify(doc));
    out = round.body.replace(/needle/g, 'thread') + round.toks.length;
    createHash('sha256').update(out).digest('hex');
  }
  n++;
  if (n % 3 === 0) {
    for (let w = 0; w < workers && Date.now() < deadline; w++) {
      execFileSync('/usr/bin/node', ['-e',
        'const{createHash}=require("node:crypto");let h="x";for(let i=0;i<20000;i++)h=createHash("sha256").update(h).digest("hex");']);
    }
  }
}
writeFileSync('/work/dist/build-out.txt', 'iterations=' + n + '\n');
console.log('build done iterations=' + n);
`

// tfHoldMJS is the test-shaped hold: pin a browser-sized resident buffer and
// tick moderate compute until the phase ends, the footprint of a headless
// test suite between build bursts.
const tfHoldMJS = `const mb = Number(process.argv[2] || 300);
const sec = Number(process.argv[3] || 15);
const buf = Buffer.allocUnsafe(mb * 1024 * 1024);
for (let i = 0; i < buf.length; i += 4096) buf[i] = 1;
const deadline = Date.now() + sec * 1000;
let x = 0;
while (Date.now() < deadline) {
  for (let i = 0; i < 5000000; i++) x = (x + i * i) % 9973;
  await new Promise((r) => setTimeout(r, 150));
}
console.log('hold done mb=' + mb + ' touched=' + buf.length + ' x=' + x);
`

// tfGrowMJS is the oomtest victim's allocation: grow resident memory in
// 32 MB touched chunks toward a target, the shape of a real process leaking
// or loading past its budget — a single giant allocation can fail politely
// inside the runtime, which is a different (gentler) outcome than the
// ceiling enforcement this scenario exists to demonstrate.
const tfGrowMJS = `const targetMB = Number(process.argv[2] || 1024);
const chunkMB = 32;
const chunks = [];
let grown = 0;
while (grown < targetMB) {
  const buf = Buffer.allocUnsafe(chunkMB * 1024 * 1024);
  for (let i = 0; i < buf.length; i += 4096) buf[i] = 1;
  chunks.push(buf);
  grown += chunkMB;
  console.log('grown mb=' + grown);
}
console.log('target reached, holding');
await new Promise((r) => setTimeout(r, 120000));
console.log('held to completion chunks=' + chunks.length);
`

// seedWorktree materializes the tree the workload reads: text files carrying
// a known token, sized so grep and ls do real work without dominating the
// memory picture, plus the heavy profile's burst scripts. The jail's launch
// chowns the tree to the sandbox identity.
func seedWorktree(root string, files int) error {
	if files < 16 {
		files = 16
	}
	_ = os.RemoveAll(root)
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		return err
	}
	var b strings.Builder
	for i := 0; i < 64; i++ {
		b.WriteString("lorem ipsum sandbox capacity line with a needle in it and padding padding padding\n")
	}
	content := []byte(b.String())
	for i := 1; i <= files; i++ {
		if err := os.WriteFile(filepath.Join(src, fmt.Sprintf("f%04d.txt", i)), content, 0o644); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(root, "tf-build.mjs"), []byte(tfBuildMJS), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "tf-grow.mjs"), []byte(tfGrowMJS), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "tf-hold.mjs"), []byte(tfHoldMJS), 0o644)
}

// plateauSample is one level's observation: host CPU and every live jail's
// cgroup memory/CPU, sampled as a series across the whole hold so bursty
// workloads report both the cycle average and the per-run peaks — a single
// end-of-hold window would catch each jail in a random phase.
type plateauSample struct {
	hostCPUPct     float64 // whole-hold average
	hostCPUPeakPct float64 // hottest sample step
	jailCoresSum   float64
	jailCoresMean  float64 // cycle-averaged, per run
	jailCoresPeak  float64 // hottest per-step reading of any single run
	jailMemSumMB   int     // mean over ticks of the fleet's summed memory.current
	jailMemMeanMB  int
	jailMemPeakMB  int // largest single-run memory.current observed
	sampledJails   int
}

// jailSeriesStat accumulates one jail's series across the hold.
type jailSeriesStat struct {
	firstCPU  int64
	lastCPU   int64
	haveCPU   bool
	peakCores float64
	peakMemMB int
}

func sampleSeries(ctx context.Context, live []*handle, total, step time.Duration) plateauSample {
	if total < step {
		total = step
	}
	stats := make(map[string]*jailSeriesStat, len(live))
	for _, h := range live {
		if h.containerID != "" {
			stats[h.containerID] = &jailSeriesStat{}
		}
	}

	firstBusy, firstTotal, okFirst := readCPUStat()
	prevBusy, prevTotal := firstBusy, firstTotal
	var out plateauSample
	var memSumAccum float64
	var memTicks int
	start := time.Now()

	for time.Since(start) < total && ctx.Err() == nil {
		sleepCtx(ctx, step)
		stepSec := step.Seconds()

		if busy, tot, ok := readCPUStat(); ok && tot > prevTotal {
			pct := 100 * float64(busy-prevBusy) / float64(tot-prevTotal)
			if pct > out.hostCPUPeakPct {
				out.hostCPUPeakPct = pct
			}
			prevBusy, prevTotal = busy, tot
		}

		var tickMemSum, tickMemN int
		for cid, st := range stats {
			s := sandbox.SampleRunCgroup(cid)
			if s.MemCurrentMB != nil {
				tickMemSum += *s.MemCurrentMB
				tickMemN++
				if *s.MemCurrentMB > st.peakMemMB {
					st.peakMemMB = *s.MemCurrentMB
				}
			}
			if s.CPUUsecCum != nil {
				if st.haveCPU && *s.CPUUsecCum >= st.lastCPU {
					if c := float64(*s.CPUUsecCum-st.lastCPU) / 1e6 / stepSec; c > st.peakCores {
						st.peakCores = c
					}
				}
				if !st.haveCPU {
					st.firstCPU = *s.CPUUsecCum
					st.haveCPU = true
				}
				st.lastCPU = *s.CPUUsecCum
			}
		}
		if tickMemN > 0 {
			memSumAccum += float64(tickMemSum)
			memTicks++
			if tickMemN > out.sampledJails {
				out.sampledJails = tickMemN
			}
		}
	}

	elapsed := time.Since(start).Seconds()
	if lastBusy, lastTotal, ok := readCPUStat(); okFirst && ok && lastTotal > firstTotal {
		out.hostCPUPct = 100 * float64(lastBusy-firstBusy) / float64(lastTotal-firstTotal)
	} else {
		out.hostCPUPct = -1
	}

	var coreRuns int
	for _, st := range stats {
		if st.haveCPU && st.lastCPU >= st.firstCPU && elapsed > 0 {
			out.jailCoresSum += float64(st.lastCPU-st.firstCPU) / 1e6 / elapsed
			coreRuns++
		}
		if st.peakCores > out.jailCoresPeak {
			out.jailCoresPeak = st.peakCores
		}
		if st.peakMemMB > out.jailMemPeakMB {
			out.jailMemPeakMB = st.peakMemMB
		}
	}
	if coreRuns > 0 {
		out.jailCoresMean = out.jailCoresSum / float64(coreRuns)
	}
	if memTicks > 0 {
		out.jailMemSumMB = int(memSumAccum / float64(memTicks))
	}
	if out.sampledJails > 0 {
		out.jailMemMeanMB = out.jailMemSumMB / out.sampledJails
	}
	return out
}

// teardownAll closes every sandbox 16 at a time — Close shells out to
// ip/iptables via the broker, so unbounded parallelism just thrashes the
// xtables lock.
func teardownAll(live []*handle) {
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for _, h := range live {
		wg.Add(1)
		go func(h *handle) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			h.teardown()
		}(h)
	}
	wg.Wait()
}

func percentiles(ds []time.Duration) (p50, p95 time.Duration) {
	if len(ds) == 0 {
		return 0, 0
	}
	sorted := append([]time.Duration(nil), ds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2], sorted[(len(sorted)*95)/100]
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func writeCSV(path, profile string, results []levelResult) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{
		"level", "profile", "spawn_p50_ms", "spawn_p95_ms", "ready_p50_ms", "ready_p95_ms",
		"canary_ms", "jail_mem_sum_mb", "jail_mem_mean_mb", "jail_mem_peak_mb",
		"jail_cores_per_run", "jail_cores_peak", "jail_cores_sum",
		"host_cpu_pct", "host_cpu_peak_pct", "mem_delta_mb", "mem_avail_mb", "swap_used_mb", "load1",
		"tool_calls", "tool_errors", "failures", "aborted",
	}); err != nil {
		return err
	}
	for _, r := range results {
		if err := w.Write([]string{
			fmt.Sprint(r.level), profile,
			fmt.Sprint(r.spawnP50.Milliseconds()), fmt.Sprint(r.spawnP95.Milliseconds()),
			fmt.Sprint(r.readyP50.Milliseconds()), fmt.Sprint(r.readyP95.Milliseconds()),
			fmt.Sprint(r.canaryMedian.Milliseconds()),
			fmt.Sprint(r.jailMemSumMB), fmt.Sprint(r.jailMemMeanMB), fmt.Sprint(r.jailMemPeakMB),
			fmt.Sprintf("%.3f", r.jailCoresMean), fmt.Sprintf("%.3f", r.jailCoresPeak),
			fmt.Sprintf("%.2f", r.jailCoresSum),
			fmt.Sprintf("%.1f", r.hostCPUPct), fmt.Sprintf("%.1f", r.hostCPUPeakPct),
			fmt.Sprint(r.memDeltaMB), fmt.Sprint(r.memAvailMB), fmt.Sprint(r.swapUsedMB),
			fmt.Sprintf("%.2f", r.load1),
			fmt.Sprint(r.toolCalls), fmt.Sprint(r.toolErrs),
			fmt.Sprint(r.failures), r.aborted,
		}); err != nil {
			return err
		}
	}
	return nil
}

// The capacity-model JSON: raw per-level data plus fitted per-run marginals,
// in the shape the public sizing page's slider consumes. Coefficients only —
// the min() capacity arithmetic lives with the consumer, and the formula
// string documents it.
type capacityJSON struct {
	GeneratedAt   string        `json:"generated_at"`
	Profile       string        `json:"profile"`
	ToolIntervalS float64       `json:"tool_interval_s,omitempty"`
	Workload      *workloadJSON `json:"workload,omitempty"`
	Platform      string        `json:"platform"`
	Host          hostJSON      `json:"host"`
	Levels        []levelJSON   `json:"levels"`
	Model         modelJSON     `json:"model"`
	Actuals       *actualsJSON  `json:"teardown_actuals,omitempty"`
}

// workloadJSON records the heavy profile's cycle parameters, so a
// coefficient set is never read without the load model that produced it.
type workloadJSON struct {
	Duty       float64 `json:"duty"`
	BurstSec   int     `json:"burst_sec"`
	HoldSec    int     `json:"hold_sec"`
	HoldMB     int     `json:"hold_mb"`
	SyncBursts bool    `json:"sync_bursts"`
}

type hostJSON struct {
	CPUs       int `json:"cpus"`
	MemTotalMB int `json:"mem_total_mb"`
}

type levelJSON struct {
	Level           int     `json:"level"`
	SpawnMsP50      int64   `json:"spawn_ms_p50"`
	SpawnMsP95      int64   `json:"spawn_ms_p95"`
	ReadyMsP50      int64   `json:"ready_ms_p50"`
	ReadyMsP95      int64   `json:"ready_ms_p95"`
	CanaryMs        int64   `json:"canary_ms"`
	JailMemSumMB    int     `json:"jail_mem_sum_mb"`
	JailMemMeanMB   int     `json:"jail_mem_mean_mb"`
	JailMemPeakMB   int     `json:"jail_mem_peak_mb"`
	JailCoresPerRun float64 `json:"jail_cores_per_run"`
	JailCoresPeak   float64 `json:"jail_cores_peak"`
	JailCoresSum    float64 `json:"jail_cores_sum"`
	HostCPUPct      float64 `json:"host_cpu_pct"`
	HostCPUPeakPct  float64 `json:"host_cpu_peak_pct"`
	HostMemDeltaMB  int     `json:"host_mem_delta_mb"`
	Load1           float64 `json:"load1"`
	ToolCalls       int64   `json:"tool_calls"`
	ToolErrors      int64   `json:"tool_errors"`
	Failures        int     `json:"failures"`
	Aborted         string  `json:"aborted,omitempty"`
}

type modelJSON struct {
	MemMBPerRun     float64 `json:"mem_mb_per_run"`
	JailMemMBPerRun float64 `json:"jail_mem_mb_per_run"`
	MemPeakMBPerRun int     `json:"mem_peak_mb_per_run,omitempty"`
	CoresPerRun     float64 `json:"cores_per_run"`
	CoresPeakPerRun float64 `json:"cores_peak_per_run,omitempty"`
	SpawnMsP50      int64   `json:"spawn_ms_p50"`
	OneMoreRunMs    int64   `json:"one_more_run_ms"`
	HardCap         int     `json:"hard_cap"`
	Formula         string  `json:"formula"`
}

type actualsJSON struct {
	Runs          int     `json:"runs"`
	MeanPeakMemMB float64 `json:"mean_peak_mem_mb"`
	TotalCPUSec   float64 `json:"total_cpu_sec"`
}

func writeCapacityJSON(cfg benchConfig, results []levelResult) error {
	var healthy []levelResult
	for _, r := range results {
		if r.aborted == "" {
			healthy = append(healthy, r)
		}
	}

	var model modelJSON
	model.HardCap = 256
	model.Formula = "concurrent_runs = min((host_mem_mb - platform_reserve_mb) / mem_mb_per_run, host_cores / cores_per_run, hard_cap)"
	if len(healthy) > 0 {
		top := healthy[len(healthy)-1]
		model.JailMemMBPerRun = float64(top.jailMemMeanMB)
		model.MemPeakMBPerRun = top.jailMemPeakMB
		model.CoresPerRun = top.jailCoresMean
		model.CoresPeakPerRun = top.jailCoresPeak
		model.OneMoreRunMs = top.canaryMedian.Milliseconds()
		var spawns []time.Duration
		var canaries []time.Duration
		for _, r := range healthy {
			spawns = append(spawns, r.spawnP50)
			if r.canaryMedian > 0 {
				canaries = append(canaries, r.canaryMedian)
			}
		}
		p50, _ := percentiles(spawns)
		model.SpawnMsP50 = p50.Milliseconds()
		if cm, _ := percentiles(canaries); cm > 0 {
			model.OneMoreRunMs = cm.Milliseconds()
		}
		model.MemMBPerRun = memSlopeMBPerRun(healthy)
	}

	out := capacityJSON{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Profile:     cfg.profile,
		Platform:    "gvisor-systrap",
		Host:        hostJSON{CPUs: numCPU(), MemTotalMB: readMemTotalMB()},
		Model:       model,
	}
	if cfg.profile == "native" {
		out.ToolIntervalS = cfg.toolInterval.Seconds()
	}
	if cfg.profile == "native-heavy" {
		out.Workload = &workloadJSON{
			Duty:       cfg.duty,
			BurstSec:   cfg.burstSec,
			HoldSec:    cfg.holdSec,
			HoldMB:     cfg.holdMB,
			SyncBursts: cfg.syncBursts,
		}
	}
	for _, r := range results {
		out.Levels = append(out.Levels, levelJSON{
			Level:           r.level,
			SpawnMsP50:      r.spawnP50.Milliseconds(),
			SpawnMsP95:      r.spawnP95.Milliseconds(),
			ReadyMsP50:      r.readyP50.Milliseconds(),
			ReadyMsP95:      r.readyP95.Milliseconds(),
			CanaryMs:        r.canaryMedian.Milliseconds(),
			JailMemSumMB:    r.jailMemSumMB,
			JailMemMeanMB:   r.jailMemMeanMB,
			JailMemPeakMB:   r.jailMemPeakMB,
			JailCoresPerRun: r.jailCoresMean,
			JailCoresPeak:   r.jailCoresPeak,
			JailCoresSum:    r.jailCoresSum,
			HostCPUPct:      r.hostCPUPct,
			HostCPUPeakPct:  r.hostCPUPeakPct,
			HostMemDeltaMB:  r.memDeltaMB,
			Load1:           r.load1,
			ToolCalls:       r.toolCalls,
			ToolErrors:      r.toolErrs,
			Failures:        r.failures,
			Aborted:         r.aborted,
		})
	}

	actualsLog.mu.Lock()
	if actualsLog.recorded > 0 {
		a := &actualsJSON{Runs: actualsLog.recorded}
		var peakSum int
		for _, p := range actualsLog.peaksMB {
			peakSum += p
		}
		if len(actualsLog.peaksMB) > 0 {
			a.MeanPeakMemMB = float64(peakSum) / float64(len(actualsLog.peaksMB))
		}
		var cpuSum int64
		for _, c := range actualsLog.cpuUsec {
			cpuSum += c
		}
		a.TotalCPUSec = float64(cpuSum) / 1e6
		out.Actuals = a
	}
	actualsLog.mu.Unlock()

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cfg.jsonPath, append(data, '\n'), 0o644)
}

// memSlopeMBPerRun fits host MemAvailable delta against level with simple
// least squares over the healthy levels — the intercept absorbs the fixed
// overhead standing the first plateau up, so the slope is the honest
// marginal cost per additional run.
func memSlopeMBPerRun(healthy []levelResult) float64 {
	if len(healthy) == 0 {
		return 0
	}
	if len(healthy) == 1 {
		if healthy[0].level > 0 {
			return float64(healthy[0].memDeltaMB) / float64(healthy[0].level)
		}
		return 0
	}
	var sx, sy, sxx, sxy float64
	n := float64(len(healthy))
	for _, r := range healthy {
		x, y := float64(r.level), float64(r.memDeltaMB)
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / den
}

// printSummary reports the measured soft cap — the last level that held its
// plateau without a guardrail abort and without the canary degrading past 2x
// the first plateau's baseline — plus the fitted capacity coefficients.
func printSummary(results []levelResult, cfg benchConfig) {
	if len(results) == 0 {
		fmt.Println("no levels completed")
		return
	}
	baseline := results[0].canaryMedian
	softCap := 0
	var reason string
	for _, r := range results {
		if r.aborted != "" {
			reason = fmt.Sprintf("level %d aborted: %s", r.level, r.aborted)
			break
		}
		if baseline > 0 && r.canaryMedian > 2*baseline {
			reason = fmt.Sprintf("canary at level %d (%s) exceeded 2x baseline (%s)",
				r.level, r.canaryMedian.Round(time.Millisecond), baseline.Round(time.Millisecond))
			break
		}
		softCap = r.level
	}
	fmt.Printf("\n=== summary (profile=%s) ===\n", cfg.profile)
	if reason != "" {
		fmt.Println(reason)
	}
	if softCap == results[len(results)-1].level && reason == "" {
		fmt.Printf("soft cap NOT reached: all %d levels healthy up to %d — raise -levels or lower -mem-floor-mb\n",
			len(results), softCap)
	} else {
		fmt.Printf("soft cap on this host: ~%d concurrent sandboxes\n", softCap)
	}
	var top *levelResult
	for i := range results {
		if results[i].aborted == "" {
			top = &results[i]
		}
	}
	if top != nil && top.jailCoresMean > 0 {
		fmt.Printf("steady state at level %d: %.2f cores/run, %d MB/run (jail cgroup) → CPU-implied capacity on this host: ~%d runs\n",
			top.level, top.jailCoresMean, top.jailMemMeanMB, int(float64(numCPU())/top.jailCoresMean))
		if top.jailCoresPeak > 0 || top.jailMemPeakMB > 0 {
			fmt.Printf("burst peaks at level %d: %.2f cores/run, %d MB/run — size headroom for these, not the averages\n",
				top.level, top.jailCoresPeak, top.jailMemPeakMB)
		}
	}
	fmt.Printf("results: %s + %s\n", cfg.csvPath, cfg.jsonPath)
}
