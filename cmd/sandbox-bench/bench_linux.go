//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// handle is one live benchmark sandbox: the launched run, the sandbox
// teardown state, and the cancel that SIGKILLs it.
type handle struct {
	id     int
	sb     *sandbox.Sandbox
	run    sandbox.LaunchedRun
	cancel context.CancelFunc

	waitOnce sync.Once
	waitErr  error
	// closeMock shuts down the per-run mock LLM endpoint (claude
	// profile only; nil otherwise).
	closeMock func()
}

// wait reaps the runsc process exactly once; safe to call from both
// the ready-watcher and teardown.
func (h *handle) wait() error {
	h.waitOnce.Do(func() { h.waitErr = h.run.Wait() })
	return h.waitErr
}

type levelResult struct {
	level        int
	spawnP50     time.Duration // sandbox.Wrap for sandboxes added this level
	spawnP95     time.Duration
	readyP50     time.Duration // Wrap start → workload printed READY
	readyP95     time.Duration
	canaryMedian time.Duration // full Wrap→echo→Close while N are live
	memAvailMB   int
	memDeltaMB   int // pre-ramp MemAvailable minus now: true marginal cost
	swapUsedMB   int
	load1        float64
	cpuPct       float64
	sandboxRSSMB int // summed RSS of every live sandbox process tree
	sandboxPSSMB int // summed PSS — shared pages divided; use for planning
	failures     int
	aborted      string // non-empty = guardrail/spawn abort reason
}

func run(cfg benchConfig) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("must run as root (netns + iptables + chown to sandbox UID)")
	}
	for _, bin := range []string{"runsc", "ip", "iptables", "chroot"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s not found on PATH", bin)
		}
	}
	if cfg.loadMax == 0 {
		cfg.loadMax = 3 * float64(numCPU())
	}
	if cfg.workRoot == "" {
		dir, err := os.MkdirTemp("", "sandbox-bench-")
		if err != nil {
			return err
		}
		cfg.workRoot = dir
		defer os.RemoveAll(dir)
	}

	fmt.Printf("host: %d cpus, %d MB available, profile=%s rss=%dMB duty=%d%% levels=%v hold=%s\n",
		numCPU(), readMemAvailableMB(), cfg.profile, cfg.rssMB, cfg.dutyPct, cfg.levels, cfg.hold)

	// claude: fail fast when the SDK's musl engine binary isn't where
	// the argv will point, rather than 90s-timeout per spawn.
	if cfg.profile == "claude" {
		bin := filepath.Join(cfg.sdkDir, claudeMuslRelPath())
		if _, err := os.Stat(bin); err != nil {
			return fmt.Errorf("claude profile: engine binary not found at %s (set -sdk-dir): %w", bin, err)
		}
		fmt.Printf("claude: engine %s, %d mock turns per run, JSC JIT %v\n", bin, cfg.turns, cfg.jscJIT)
		debugMock = cfg.debugStream
	}

	// pageshare: one read-only blob on the host, bind-mounted into
	// every sandbox. Real (non-sparse) bytes — hole reads would map
	// the shared zero page and fake a "shared" result.
	if cfg.profile == "pageshare" {
		if err := writeBlob(blobPath(cfg), cfg.blobMB); err != nil {
			return fmt.Errorf("write pageshare blob: %w", err)
		}
		fmt.Printf("pageshare: %d MB blob at %s — if MemAvailable delta grows ~%d MB per level step the pages are private per sandbox; if it stays ~flat they are shared\n",
			cfg.blobMB, blobPath(cfg), cfg.blobMB)
	}

	// Root context: Ctrl-C / SIGTERM triggers the same orderly
	// teardown path as a completed ramp.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Warm-up: one throwaway sandbox. On a cold rootfs cache this pays
	// the alpine download + apk toolchain install (minutes); afterwards
	// Wrap's rootfs step is one os.Stat. Doing it here keeps the cold
	// cost out of every measurement.
	fmt.Println("warmup: bringing up one sandbox (cold rootfs cache can take ~10 min)...")
	warmCtx, warmCancel := context.WithTimeout(ctx, 20*time.Minute)
	warmStart := time.Now()
	if err := runOneShot(warmCtx, cfg, "warmup", []string{"/bin/echo", "warmup-ok"}); err != nil {
		warmCancel()
		return fmt.Errorf("warmup sandbox failed: %w", err)
	}
	warmCancel()
	fmt.Printf("warmup: ok in %s\n", time.Since(warmStart).Round(time.Millisecond))

	// Guardrail watcher: flips abortReason and cancels spawning when
	// the host crosses a limit. Sampled every 2s.
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

	var live []*handle
	var results []levelResult
	nextID := 0
	baselineAvailMB := readMemAvailableMB()

	// Teardown everything on every exit path before reporting.
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
		// More than a quarter of the delta failing to come up is itself
		// the soft-cap signal — record and stop rather than measuring a
		// plateau that doesn't exist.
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

		// Hold the plateau, sampling host CPU over the tail so the
		// number reflects steady state, not spawn churn.
		sleepCtx(guardCtx, cfg.hold)
		res.cpuPct = sampleCPUPct(guardCtx, 3*time.Second)
		res.memAvailMB = readMemAvailableMB()
		res.memDeltaMB = baselineAvailMB - res.memAvailMB
		res.swapUsedMB = readSwapUsedMB()
		res.load1 = readLoad1()
		res.sandboxRSSMB = totalTreeRSSMB(live)
		res.sandboxPSSMB = totalTreePSSMB(live)

		// Canary: the marginal cost of starting one more run while N
		// are live. Median of 3 full Wrap→echo→Close cycles. At the
		// 256 hard cap there is no 257th subnet, so no canary.
		var canary []time.Duration
		for i := 0; i < 3 && level < 256 && getAbort() == "" && guardCtx.Err() == nil; i++ {
			cStart := time.Now()
			cCtx, cCancel := context.WithTimeout(guardCtx, 2*time.Minute)
			err := runOneShot(cCtx, cfg, fmt.Sprintf("canary-%d-%d", level, i), []string{"/bin/echo", "canary-ok"})
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
		fmt.Printf("level %d: spawn p50=%s p95=%s ready p95=%s canary=%s | pss=%dMB (rss=%dMB) delta=%dMB avail=%dMB swap=%dMB load1=%.1f cpu=%.0f%% fail=%d\n",
			level, res.spawnP50.Round(time.Millisecond), res.spawnP95.Round(time.Millisecond),
			res.readyP95.Round(time.Millisecond), res.canaryMedian.Round(time.Millisecond),
			res.sandboxPSSMB, res.sandboxRSSMB, res.memDeltaMB, res.memAvailMB,
			res.swapUsedMB, res.load1, res.cpuPct, res.failures)

		if res.aborted != "" {
			break
		}
	}

	if err := writeCSV(cfg.csvPath, cfg.profile, results); err != nil {
		return fmt.Errorf("write csv: %w", err)
	}
	printSummary(results, cfg)
	return nil
}

// spawnDelta brings up `delta` new sandboxes with at most cfg.burst
// concurrent bring-ups. Returns the successfully started handles plus
// spawn (Wrap) and ready (Wrap→READY) timings.
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
			h, spawnDur, readyDur, err := startSandbox(ctx, cfg, id)
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

// startSandbox Wraps + Starts one workload sandbox and waits for its
// ready signal: the READY line for synthetic profiles, the first
// `result` stream event for the claude profile — either way the
// workload is fully resident before the plateau counts it.
func startSandbox(ctx context.Context, cfg benchConfig, id int) (*handle, time.Duration, time.Duration, error) {
	worktree, sdkDir, err := makeRunDirs(cfg.workRoot, id)
	if err != nil {
		return nil, 0, 0, err
	}

	var extraMounts []sandbox.Mount
	if cfg.profile == "pageshare" {
		extraMounts = append(extraMounts, sandbox.Mount{
			Source:      blobPath(cfg),
			Destination: "/blob.bin",
			Options:     []string{"ro"},
		})
	}

	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/work"}
	var mock *mockLLM
	var configureProxies func(s *sandbox.Sandbox) ([]string, error)
	var closeMock func()
	readyTimeout := 90 * time.Second
	if cfg.profile == "claude" {
		// Real engine: the SDK dir mounts at /sdk exactly as in
		// production, and the mock endpoint binds on the run's gateway
		// IP inside ConfigureProxies — same place, same sequencing as
		// the production LLM proxy. The env mirrors TFAC-548's shape.
		sdkDir = cfg.sdkDir
		env = append(env,
			"TERM=xterm",
			"DISABLE_TELEMETRY=1",
			"DISABLE_ERROR_REPORTING=1",
			"DISABLE_AUTOUPDATER=1",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		)
		if !cfg.jscJIT {
			env = append(env, "BUN_JSC_useJIT=0")
		}
		mock = &mockLLM{turns: cfg.turns}
		configureProxies = func(s *sandbox.Sandbox) ([]string, error) {
			baseURL, closer, err := mock.start(s.HostIP)
			if err != nil {
				return nil, err
			}
			closeMock = closer
			return []string{
				"ANTHROPIC_BASE_URL=" + baseURL,
				"ANTHROPIC_API_KEY=sk-ant-mock-bench",
			}, nil
		}
		readyTimeout = 150 * time.Second
	}

	runCtx, cancel := context.WithCancel(ctx)
	start := time.Now()
	run, sb, err := sandbox.Wrap(runCtx, sandbox.Config{
		RunID:            fmt.Sprintf("bench-%04d", id),
		Worktree:         worktree,
		SDKDir:           sdkDir,
		Argv:             workloadArgv(cfg),
		Env:              env,
		ExtraMounts:      extraMounts,
		ConfigureProxies: configureProxies,
		MemoryLimitMB:    cfg.memLimitMB,
	})
	if err != nil {
		cancel()
		if closeMock != nil {
			closeMock()
		}
		return nil, 0, 0, err
	}
	spawnDur := time.Since(start)

	h := &handle{id: id, sb: sb, run: run, cancel: cancel, closeMock: closeMock}
	fail := func(err error) (*handle, time.Duration, time.Duration, error) {
		cancel()
		_ = h.wait()
		_ = run.Close()
		_ = sb.Close()
		if closeMock != nil {
			closeMock()
		}
		return nil, 0, 0, err
	}

	// LaunchedRun starts the run; its Stdin/Stdout are valid only after
	// Start, and its stderr is captured internally (read via run.Stderr()).
	if err := run.Start(); err != nil {
		cancel()
		_ = run.Close()
		_ = sb.Close()
		if closeMock != nil {
			closeMock()
		}
		return nil, 0, 0, err
	}
	stdout := run.Stdout()
	var stdin io.WriteCloser
	if cfg.profile == "claude" {
		stdin = run.Stdin()
	}

	if cfg.profile == "claude" {
		// One user message opens the conversation; stdin stays OPEN so
		// the engine idles resident after its result, holding the
		// plateau like a live delegated run between turns.
		_, err := io.WriteString(stdin,
			`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Run the benchmark task."}]}}`+"\n")
		if err != nil {
			return fail(fmt.Errorf("write initial message: %w", err))
		}
	}

	isReady := func(line string) bool { return line == "READY" }
	if cfg.profile == "claude" {
		isReady = func(line string) bool { return strings.Contains(line, `"type":"result"`) }
	}
	ready := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if cfg.debugStream {
				fmt.Printf("  [%d out] %.220s\n", id, line)
			}
			if isReady(line) {
				close(ready)
				break
			}
		}
		// Drain so the workload never blocks on a full pipe.
		for scanner.Scan() {
		}
	}()

	select {
	case <-ready:
		return h, spawnDur, time.Since(start), nil
	case <-time.After(readyTimeout):
		// Reap first so the exit reflects reality — an OOM-killed sandbox
		// otherwise reads as "still running".
		h.cancel()
		waitErr := "exited"
		if e := h.wait(); e != nil {
			waitErr = e.Error()
		}
		if h.run.OOMKilled() {
			waitErr += " (oom-killed)"
		}
		mockHits := int64(-1)
		if mock != nil {
			mockHits = mock.requests.Load()
		}
		return fail(fmt.Errorf("workload not ready after %s (proc: %s, mock hits: %d, stderr: %s)",
			readyTimeout, waitErr, mockHits, lastLines(h.run.Stderr(), 5)))
	case <-runCtx.Done():
		_ = h.wait()
		_ = run.Close()
		_ = sb.Close()
		if closeMock != nil {
			closeMock()
		}
		return nil, 0, 0, runCtx.Err()
	}
}

// runOneShot is the warmup/canary path: one sandbox running a short
// command through the full Wrap→exec→Close lifecycle.
func runOneShot(ctx context.Context, cfg benchConfig, name string, argv []string) error {
	worktree, sdkDir, err := makeRunDirs(cfg.workRoot, -1)
	if err != nil {
		return err
	}
	defer os.RemoveAll(worktree)
	defer os.RemoveAll(sdkDir)

	run, sb, err := sandbox.Wrap(ctx, sandbox.Config{
		RunID:    name,
		Worktree: worktree,
		SDKDir:   sdkDir,
		Argv:     argv,
		Env:      []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/work"},
	})
	if err != nil {
		return err
	}
	defer func() { _ = sb.Close() }()
	defer func() { _ = run.Close() }()

	// Drive the run to completion: drain stdout (so the workload never
	// blocks on a full pipe), then Wait, mirroring the old cmd.CombinedOutput.
	if err := run.Start(); err != nil {
		return err
	}
	out, _ := io.ReadAll(run.Stdout())
	if err := run.Wait(); err != nil {
		combined := strings.TrimSpace(string(out) + run.Stderr())
		return fmt.Errorf("%w (output: %s)", err, combined)
	}
	return nil
}

// makeRunDirs creates the per-run worktree (chowned so the sandbox
// UID can write it) and an empty SDK stub dir — the bench workloads
// don't load the SDK, but Wrap requires the mount source to exist.
func makeRunDirs(workRoot string, id int) (worktree, sdkDir string, err error) {
	pattern := "run-"
	if id >= 0 {
		pattern = fmt.Sprintf("run-%04d-", id)
	}
	worktree, err = os.MkdirTemp(workRoot, pattern)
	if err != nil {
		return "", "", err
	}
	if err := os.Chown(worktree, sandbox.WorktreeUID, sandbox.WorktreeGID); err != nil {
		return "", "", fmt.Errorf("chown worktree: %w", err)
	}
	sdkDir = filepath.Join(worktree + "-sdk")
	if err := os.Mkdir(sdkDir, 0o755); err != nil {
		return "", "", err
	}
	return worktree, sdkDir, nil
}

// workloadArgv builds the in-sandbox command for the chosen profile.
// Paths are absolute into the alpine rootfs (runsc does no PATH
// resolution). The agent profile mirrors what a real delegated run
// costs on the host: a few hundred MB resident, a few percent of one
// core, and a trickle of writes into /work.
func workloadArgv(cfg benchConfig) []string {
	switch cfg.profile {
	case "idle":
		return []string{"/bin/sh", "-c", "echo READY; exec sleep 86400"}
	case "cpu":
		return []string{"/usr/bin/python3", "-c",
			"print('READY', flush=True)\nwhile True: pass"}
	case "claude":
		// The real engine, exactly as production runs it minus the node
		// supervisor: stream-json stdio against the mock endpoint. The
		// model name is decorative — the mock answers whatever is asked.
		return []string{
			"/sdk/" + claudeMuslRelPath(), "-p",
			"--input-format", "stream-json",
			"--output-format", "stream-json",
			"--verbose",
			"--model", "haiku",
			"--permission-mode", "bypassPermissions",
		}
	case "pageshare":
		// mmap the shared RO blob and touch every page — the same
		// access pattern as executing a large mapped binary.
		return []string{"/usr/bin/python3", "-c", `
import mmap, os, time
f = os.open('/blob.bin', os.O_RDONLY)
m = mmap.mmap(f, 0, prot=mmap.PROT_READ)
s = 0
for i in range(0, len(m), 4096):
    s += m[i]
print('READY', flush=True)
while True:
    time.sleep(60)
`}
	default: // agent
		script := fmt.Sprintf(`
import time
buf = bytearray(%d * 1024 * 1024)
for i in range(0, len(buf), 4096):
    buf[i] = 1
print('READY', flush=True)
duty = %d / 100.0
n = 0
while True:
    t = time.monotonic()
    while time.monotonic() - t < duty:
        n += 1
    with open('/work/beat.txt', 'w') as f:
        f.write(str(n) * 512)
    time.sleep(1.0 - duty)
`, cfg.rssMB, cfg.dutyPct)
		return []string{"/usr/bin/python3", "-c", script}
	}
}

func blobPath(cfg benchConfig) string {
	return filepath.Join(cfg.workRoot, "blob.bin")
}

// claudeMuslRelPath is the engine binary's path relative to the SDK
// install root. The sandbox rootfs is alpine, so it's always the musl
// build; only the arch varies.
func claudeMuslRelPath() string {
	arch := "x64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}
	return filepath.Join("node_modules", "@anthropic-ai",
		"claude-agent-sdk-linux-"+arch+"-musl", "claude")
}

// writeBlob writes sizeMB of real nonzero bytes, world-readable so
// the sandbox UID can map it through the RO bind mount.
func writeBlob(path string, sizeMB int) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = byte(i%251 + 1)
	}
	for i := 0; i < sizeMB; i++ {
		if _, err := f.Write(chunk); err != nil {
			return err
		}
	}
	return f.Sync()
}

// teardownAll cancels every runsc (SIGKILL), reaps it, then Closes the
// sandbox (iptables/netns/bundle teardown), 16 at a time — Close shells
// out to ip/iptables, so unbounded parallelism just thrashes the
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
			h.cancel()
			_ = h.wait()
			_ = h.run.Close()
			if err := h.sb.Close(); err != nil {
				fmt.Printf("  close %d: %v\n", h.id, err)
			}
			if h.closeMock != nil {
				h.closeMock()
			}
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

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
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
		"canary_ms", "sandbox_pss_mb", "sandbox_rss_mb", "mem_delta_mb", "mem_avail_mb",
		"swap_used_mb", "load1", "cpu_pct", "failures", "aborted",
	}); err != nil {
		return err
	}
	for _, r := range results {
		if err := w.Write([]string{
			fmt.Sprint(r.level), profile,
			fmt.Sprint(r.spawnP50.Milliseconds()), fmt.Sprint(r.spawnP95.Milliseconds()),
			fmt.Sprint(r.readyP50.Milliseconds()), fmt.Sprint(r.readyP95.Milliseconds()),
			fmt.Sprint(r.canaryMedian.Milliseconds()), fmt.Sprint(r.sandboxPSSMB),
			fmt.Sprint(r.sandboxRSSMB), fmt.Sprint(r.memDeltaMB),
			fmt.Sprint(r.memAvailMB), fmt.Sprint(r.swapUsedMB),
			fmt.Sprintf("%.2f", r.load1), fmt.Sprintf("%.1f", r.cpuPct),
			fmt.Sprint(r.failures), r.aborted,
		}); err != nil {
			return err
		}
	}
	return nil
}

// printSummary reports the measured soft cap: the last level that held
// its plateau without a guardrail abort and without the canary
// degrading past 2x the first plateau's baseline.
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
	fmt.Printf("results: %s\n", cfg.csvPath)
}
