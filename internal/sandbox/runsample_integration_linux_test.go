//go:build linux && integration

// Live proof that a running jail can be observed from the unprivileged side.
// The readers are unit-tested against fixture files (runsample_linux_test.go);
// what only a real jail can show is that the container id Wrap published
// actually names a readable cgroup, that consecutive reads of a live sandbox
// move the way a resource series has to (CPU never backwards, memory inside
// the run's own ceiling), and that the read goes quiet — not zero — the moment
// the group is torn down.
//
// Nothing here holds a capability or talks to the cap-broker: cgroup v2 stat
// files are world-readable, and the broker's monopoly is on cgroup lifecycle
// and mutation, never observation.

package sandbox

import (
	"context"
	"testing"
	"time"
)

// sampleLimitMB is the ceiling the observed run gets — the same reasoning as
// actualsLimitMB: low enough that "memory inside the ceiling" is a real bound,
// high enough that the payload exits rather than OOMs.
const sampleLimitMB = 512

// TestIntegration_SampleRunCgroup_SeriesFromLiveJail samples a live sandbox
// repeatedly while a CPU-burning payload runs, and pins the two properties a
// consumer differences into a rate: cumulative CPU is monotonically
// non-decreasing, and it actually advances across the window.
func TestIntegration_SampleRunCgroup_SeriesFromLiveJail(t *testing.T) {
	requireRunsc(t)

	cfg := minimalConfig(t)
	cfg.MemoryLimitMB = sampleLimitMB
	// A payload that keeps working long enough to be sampled several times and
	// then exits on its own if the test dies before killing it.
	cfg.Argv = []string{"/bin/sh", "-c", "i=0; while [ $i -lt 30 ]; do dd if=/dev/zero of=/dev/null bs=1M count=64 2>/dev/null; i=$((i+1)); done"}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer sb.Close()
	defer run.Close()

	if sb.ContainerID == "" {
		t.Fatal("Wrap published no container id; the sampler has nothing to observe")
	}
	if err := run.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	const samples = 4
	series := make([]RunSample, 0, samples)
	for i := 0; i < samples; i++ {
		time.Sleep(300 * time.Millisecond)
		s := SampleRunCgroup(sb.ContainerID)
		if !s.Observed() {
			t.Fatalf("sample %d observed nothing from a live jail's cgroup (%s)", i, sb.ContainerID)
		}
		series = append(series, s)
	}

	for i, s := range series {
		if s.CPUUsecCum == nil {
			t.Fatalf("sample %d has no cpu figure; cpu.stat is present on every cgroup v2 group", i)
		}
		if s.MemCurrentMB == nil {
			t.Fatalf("sample %d has no memory figure; memory.current is present on every cgroup v2 group", i)
		}
		if *s.MemCurrentMB > sampleLimitMB {
			t.Errorf("sample %d memory = %d MB exceeds the run's own %d MB ceiling — memory.max enforces it, so a correctly-scoped read cannot see this",
				i, *s.MemCurrentMB, sampleLimitMB)
		}
		if i == 0 {
			continue
		}
		if *s.CPUUsecCum < *series[i-1].CPUUsecCum {
			t.Errorf("cumulative cpu went backwards between samples %d and %d: %d → %d",
				i-1, i, *series[i-1].CPUUsecCum, *s.CPUUsecCum)
		}
	}
	// The series is only useful if it moves: a flat counter across a second of
	// dd would mean the read is scoped to the wrong group.
	if *series[len(series)-1].CPUUsecCum <= *series[0].CPUUsecCum {
		t.Errorf("cumulative cpu did not advance across the window (%d → %d) for a jail doing real work",
			*series[0].CPUUsecCum, *series[len(series)-1].CPUUsecCum)
	}
}

// TestIntegration_SampleRunCgroup_TornDownJailIsUnobserved is the sampler's
// routine race, live: once the group is removed the read must report nothing at
// all, so the sampler writes no row. A zeroed sample here would record a
// finished run as an idle one.
func TestIntegration_SampleRunCgroup_TornDownJailIsUnobserved(t *testing.T) {
	requireRunsc(t)

	cfg := minimalConfig(t)
	cfg.MemoryLimitMB = sampleLimitMB
	cfg.Argv = []string{"/bin/sh", "-c", "while true; do :; done"}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer sb.Close()

	if err := run.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if !SampleRunCgroup(sb.ContainerID).Observed() {
		t.Fatalf("live jail %s was not observable before teardown", sb.ContainerID)
	}

	// Close kills the runtime and removes the cgroup — the same teardown the
	// sampler races on every tick.
	if err := run.Close(); err != nil {
		t.Fatalf("Close (kill): %v", err)
	}
	_ = run.Wait()

	if s := SampleRunCgroup(sb.ContainerID); s.Observed() {
		t.Errorf("torn-down jail sampled as %+v, want nothing observed", s)
	}
}
