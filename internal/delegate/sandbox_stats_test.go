package delegate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// fakeSandboxStatStore records what the sampler wrote so a test can assert on
// the batch rather than on a database.
type fakeSandboxStatStore struct {
	batches   [][]domain.SandboxStat
	reapCalls []time.Time
	recordErr error
	reapErr   error
}

func (f *fakeSandboxStatStore) RecordBatch(_ context.Context, stats []domain.SandboxStat) error {
	f.batches = append(f.batches, stats)
	return f.recordErr
}

func (f *fakeSandboxStatStore) ListForClaim(context.Context, string) ([]domain.SandboxStat, error) {
	return nil, nil
}

func (f *fakeSandboxStatStore) Reap(_ context.Context, cutoff time.Time) (int64, error) {
	f.reapCalls = append(f.reapCalls, cutoff)
	return 3, f.reapErr
}

var _ db.SandboxStatStore = (*fakeSandboxStatStore)(nil)

// stubJails installs a fake live-jail registry and cgroup reader for the
// duration of a test, returning a pointer to the read counter so the "idle
// costs nothing" property is observable rather than assumed.
func stubJails(t *testing.T, jails []agentproc.LiveJail, read func(containerID string) sandbox.RunSample) *int {
	t.Helper()
	origList, origSample := liveJailsFn, sampleJailFn
	t.Cleanup(func() { liveJailsFn, sampleJailFn = origList, origSample })

	reads := 0
	liveJailsFn = func() []agentproc.LiveJail { return jails }
	sampleJailFn = func(containerID string) sandbox.RunSample {
		reads++
		return read(containerID)
	}
	return &reads
}

func iptr(v int) *int       { return &v }
func i64ptr(v int64) *int64 { return &v }

// TestSampleSandboxStatsOnce_BatchesEveryLiveJail is the happy path: one row
// per live jail, keyed to its claim, all sharing one timestamp so a
// multi-sandbox read lines up without bucketing — and all in a single insert.
func TestSampleSandboxStatsOnce_BatchesEveryLiveJail(t *testing.T) {
	store := &fakeSandboxStatStore{}
	s := &Spawner{sandboxStats: store}
	samples := map[string]sandbox.RunSample{
		"tf-run-a-1": {MemCurrentMB: iptr(310), CPUUsecCum: i64ptr(1_200_000)},
		"tf-run-b-2": {MemCurrentMB: iptr(188), CPUUsecCum: i64ptr(90_000)},
	}
	reads := stubJails(t, []agentproc.LiveJail{
		{ClaimID: "claim-a", ContainerID: "tf-run-a-1"},
		{ClaimID: "claim-b", ContainerID: "tf-run-b-2"},
	}, func(id string) sandbox.RunSample { return samples[id] })

	s.sampleSandboxStatsOnce(context.Background())

	if *reads != 2 {
		t.Errorf("cgroup reads = %d, want 2 (one per live jail)", *reads)
	}
	if len(store.batches) != 1 {
		t.Fatalf("writes = %d, want exactly 1 batched insert", len(store.batches))
	}
	batch := store.batches[0]
	if len(batch) != 2 {
		t.Fatalf("batch = %+v, want 2 rows", batch)
	}
	if batch[0].ClaimID != "claim-a" || batch[1].ClaimID != "claim-b" {
		t.Errorf("rows not attributed to their claims: %+v", batch)
	}
	if batch[0].MemCurrentMB == nil || *batch[0].MemCurrentMB != 310 ||
		batch[0].CPUUsecCum == nil || *batch[0].CPUUsecCum != 1_200_000 {
		t.Errorf("claim-a metrics lost: %+v", batch[0])
	}
	if !batch[0].At.Equal(batch[1].At) {
		t.Errorf("rows in one tick must share a timestamp: %v vs %v", batch[0].At, batch[1].At)
	}
	if batch[0].At.IsZero() {
		t.Error("sample timestamp is zero")
	}
}

// TestSampleSandboxStatsOnce_IdleCostsNothing pins the acceptance property:
// with no live jails there is no cgroup read and no row — an idle executor and
// every control pod pay nothing for this rider on their sampler tick.
func TestSampleSandboxStatsOnce_IdleCostsNothing(t *testing.T) {
	store := &fakeSandboxStatStore{}
	s := &Spawner{sandboxStats: store}
	reads := stubJails(t, nil, func(string) sandbox.RunSample {
		t.Error("sampled a cgroup with no live jails")
		return sandbox.RunSample{}
	})

	s.sampleSandboxStatsOnce(context.Background())

	if *reads != 0 {
		t.Errorf("cgroup reads = %d with no live jails, want 0", *reads)
	}
	if len(store.batches) != 0 {
		t.Errorf("writes = %d with no live jails, want 0 (not even an empty insert)", len(store.batches))
	}
}

// TestSampleSandboxStatsOnce_GrouplessJailProducesNoRow covers a registered
// jail with no cgroup to read, at either end of a run's life: one registered
// but not yet started (registration precedes the exec that creates the group)
// and one torn down between the registry snapshot and the read. Both contribute
// no row and no error while their live sibling records normally — a zeroed row
// would report a run that has not started, or has already finished, as an idle
// live one.
func TestSampleSandboxStatsOnce_GrouplessJailProducesNoRow(t *testing.T) {
	store := &fakeSandboxStatStore{}
	s := &Spawner{sandboxStats: store}
	stubJails(t, []agentproc.LiveJail{
		{ClaimID: "claim-starting", ContainerID: "tf-run-starting-1"},
		{ClaimID: "claim-gone", ContainerID: "tf-run-gone-2"},
		{ClaimID: "claim-live", ContainerID: "tf-run-live-3"},
	}, func(id string) sandbox.RunSample {
		switch id {
		case "tf-run-starting-1": // cgroup not created yet (pre-Start)
			return sandbox.RunSample{}
		case "tf-run-gone-2": // cgroup removed mid-tick
			return sandbox.RunSample{}
		}
		return sandbox.RunSample{MemCurrentMB: iptr(256), CPUUsecCum: i64ptr(4242)}
	})

	s.sampleSandboxStatsOnce(context.Background())

	if len(store.batches) != 1 {
		t.Fatalf("writes = %d, want 1", len(store.batches))
	}
	batch := store.batches[0]
	if len(batch) != 1 || batch[0].ClaimID != "claim-live" {
		t.Fatalf("batch = %+v, want only the live jail's row", batch)
	}
}

// TestSampleSandboxStatsOnce_EveryJailGoneWritesNothing is the same race with
// nothing left to record: the tick must not issue an insert at all.
func TestSampleSandboxStatsOnce_EveryJailGoneWritesNothing(t *testing.T) {
	store := &fakeSandboxStatStore{}
	s := &Spawner{sandboxStats: store}
	stubJails(t, []agentproc.LiveJail{{ClaimID: "claim-gone", ContainerID: "tf-run-gone-1"}},
		func(string) sandbox.RunSample { return sandbox.RunSample{} })

	s.sampleSandboxStatsOnce(context.Background())

	if len(store.batches) != 0 {
		t.Errorf("writes = %d when every jail vanished, want 0", len(store.batches))
	}
}

// TestSampleSandboxStatsOnce_PartialObservationStillRecords covers the
// instance-stats family's posture applied here: a sample that read one metric
// and not the other is still worth a row, with the unread metric left NULL
// rather than zeroed.
func TestSampleSandboxStatsOnce_PartialObservationStillRecords(t *testing.T) {
	store := &fakeSandboxStatStore{}
	s := &Spawner{sandboxStats: store}
	stubJails(t, []agentproc.LiveJail{{ClaimID: "claim-a", ContainerID: "tf-run-a-1"}},
		func(string) sandbox.RunSample { return sandbox.RunSample{CPUUsecCum: i64ptr(77)} })

	s.sampleSandboxStatsOnce(context.Background())

	if len(store.batches) != 1 || len(store.batches[0]) != 1 {
		t.Fatalf("batches = %+v, want one row", store.batches)
	}
	row := store.batches[0][0]
	if row.MemCurrentMB != nil {
		t.Errorf("unread memory must stay NULL, got %d", *row.MemCurrentMB)
	}
	if row.CPUUsecCum == nil || *row.CPUUsecCum != 77 {
		t.Errorf("read cpu lost: %+v", row)
	}
}

// TestSampleSandboxStatsOnce_NoStoreIsSilent covers a partially-wired Spawner:
// no store means no reads either, so the tick costs nothing rather than
// sampling cgroups it has nowhere to put.
func TestSampleSandboxStatsOnce_NoStoreIsSilent(t *testing.T) {
	s := &Spawner{}
	reads := stubJails(t, []agentproc.LiveJail{{ClaimID: "claim-a", ContainerID: "tf-run-a-1"}},
		func(string) sandbox.RunSample { return sandbox.RunSample{MemCurrentMB: iptr(1)} })

	s.sampleSandboxStatsOnce(context.Background())

	if *reads != 0 {
		t.Errorf("cgroup reads = %d with no store wired, want 0", *reads)
	}
}

// TestSampleSandboxStatsOnce_WriteFailureIsSwallowed pins the independence the
// shared tick requires: the series' write failing must not surface, so the
// instance sample that follows it on the same tick is unaffected.
func TestSampleSandboxStatsOnce_WriteFailureIsSwallowed(t *testing.T) {
	store := &fakeSandboxStatStore{recordErr: errors.New("db down")}
	s := &Spawner{sandboxStats: store}
	stubJails(t, []agentproc.LiveJail{{ClaimID: "claim-a", ContainerID: "tf-run-a-1"}},
		func(string) sandbox.RunSample { return sandbox.RunSample{MemCurrentMB: iptr(64)} })

	s.sampleSandboxStatsOnce(context.Background()) // must not panic or block

	if len(store.batches) != 1 {
		t.Fatalf("the write should still have been attempted, batches = %+v", store.batches)
	}
}

// fakeInstanceStatStore is the tick's other sink — present only so the shared
// sampler loop will start, and so a test can see that both writes happened on
// one tick.
type fakeInstanceStatStore struct {
	mu      sync.Mutex
	records []domain.InstanceStat
}

func (f *fakeInstanceStatStore) Record(_ context.Context, stat domain.InstanceStat) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, stat)
	return nil
}

func (f *fakeInstanceStatStore) ListSince(context.Context, time.Time) ([]domain.InstanceStat, error) {
	return nil, nil
}

func (f *fakeInstanceStatStore) Reap(context.Context, time.Time) (int64, error) { return 0, nil }

func (f *fakeInstanceStatStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.records)
}

var _ db.InstanceStatStore = (*fakeInstanceStatStore)(nil)

// TestRunInstanceStatSampler_TickAlsoSamplesSandboxes proves the series is
// actually wired into the existing loop rather than needing one of its own: one
// tick of RunInstanceStatSampler produces both the pod's instance row and a row
// per live jail. Nothing here starts a goroutine of its own beyond the sampler.
func TestRunInstanceStatSampler_TickAlsoSamplesSandboxes(t *testing.T) {
	instances := &fakeInstanceStatStore{}
	sandboxes := &safeSandboxStatStore{}
	s := &Spawner{instanceStats: instances, sandboxStats: sandboxes, executorID: "exec-1"}
	stubJails(t, []agentproc.LiveJail{{ClaimID: "claim-a", ContainerID: "tf-run-a-1"}},
		func(string) sandbox.RunSample {
			return sandbox.RunSample{MemCurrentMB: iptr(220), CPUUsecCum: i64ptr(31_337)}
		})

	// The instance sample blocks on its own 1s CPU window, so one tick is the
	// budget here; the interval only has to be short enough to fire promptly.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.RunInstanceStatSampler(ctx, 10*time.Millisecond, time.Hour)
	}()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if instances.count() > 0 && sandboxes.batchCount() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if instances.count() == 0 {
		t.Error("no instance_stats row from a tick")
	}
	batches := sandboxes.batchSnapshot()
	if len(batches) == 0 {
		t.Fatal("no sandbox series row from a tick — the extension is not on the loop")
	}
	row := batches[0][0]
	if row.ClaimID != "claim-a" || row.CPUUsecCum == nil || *row.CPUUsecCum != 31_337 {
		t.Errorf("sampled row = %+v, want claim-a with the stubbed metrics", row)
	}
}

// safeSandboxStatStore is fakeSandboxStatStore with a mutex, for the one test
// that reads batches while the sampler goroutine writes them.
type safeSandboxStatStore struct {
	mu      sync.Mutex
	batches [][]domain.SandboxStat
}

func (f *safeSandboxStatStore) RecordBatch(_ context.Context, stats []domain.SandboxStat) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, stats)
	return nil
}

func (f *safeSandboxStatStore) ListForClaim(context.Context, string) ([]domain.SandboxStat, error) {
	return nil, nil
}

func (f *safeSandboxStatStore) Reap(context.Context, time.Time) (int64, error) { return 0, nil }

func (f *safeSandboxStatStore) batchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.batches)
}

func (f *safeSandboxStatStore) batchSnapshot() [][]domain.SandboxStat {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]domain.SandboxStat(nil), f.batches...)
}

var _ db.SandboxStatStore = (*safeSandboxStatStore)(nil)

// TestReapSandboxStats pins that the series is trimmed against the cutoff it is
// handed — the same one the instance samples are reaped with, so the family
// keeps one retention policy — and that a failure is swallowed.
func TestReapSandboxStats(t *testing.T) {
	cutoff := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	store := &fakeSandboxStatStore{}
	s := &Spawner{sandboxStats: store}
	s.reapSandboxStats(context.Background(), cutoff)
	if len(store.reapCalls) != 1 || !store.reapCalls[0].Equal(cutoff) {
		t.Fatalf("reap calls = %v, want one at %v", store.reapCalls, cutoff)
	}

	failing := &fakeSandboxStatStore{reapErr: errors.New("db down")}
	(&Spawner{sandboxStats: failing}).reapSandboxStats(context.Background(), cutoff)
	if len(failing.reapCalls) != 1 {
		t.Fatalf("a failing reap should still have been attempted, got %v", failing.reapCalls)
	}

	(&Spawner{}).reapSandboxStats(context.Background(), cutoff) // no store: silent no-op
}
