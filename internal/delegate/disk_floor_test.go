package delegate

import (
	"context"
	"os"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/host"
)

// TestParseDispatchDiskFloorMB pins the knob's two defaults and its refusals.
// Local defaults to off for the reason local eviction does: the disk is the
// user's own, and a laptop that stops taking runs at 10 GiB free would read
// as broken.
func TestParseDispatchDiskFloorMB(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		local   bool
		want    int
		wantErr bool
	}{
		{"multi empty uses default", "", false, DefaultDispatchDiskFloorMultiMB, false},
		{"local empty is off", "", true, 0, false},
		{"whitespace uses default", " ", false, DefaultDispatchDiskFloorMultiMB, false},
		{"zero disables", "0", false, 0, false},
		{"local opts in", "2048", true, 2048, false},
		{"trims whitespace", " 4096 ", false, 4096, false},
		{"negative is invalid", "-1", false, DefaultDispatchDiskFloorMultiMB, true},
		{"garbage is invalid in local", "lots", true, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDispatchDiskFloorMB(tt.raw, tt.local)
			if got != tt.want {
				t.Errorf("ParseDispatchDiskFloorMB(%q, local=%v) = %d, want %d", tt.raw, tt.local, got, tt.want)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseDispatchDiskFloorMB(%q, local=%v) err = %v, wantErr %v", tt.raw, tt.local, err, tt.wantErr)
			}
		})
	}
}

// TestDispatchDiskGated pins the guardrail's decision table, the same one the
// memory floor keeps: disabled and unreadable fail open, below gates, at the
// floor does not.
func TestDispatchDiskGated(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")

	s.diskAvailMB = func() int { return 1 }
	if s.dispatchDiskGated() {
		t.Error("gated with no floor configured")
	}

	s.SetDispatchDiskFloor(10240)
	s.diskAvailMB = func() int { return host.Unknown }
	if s.dispatchDiskGated() {
		t.Error("gated on an Unknown probe; guardrail must fail open")
	}

	s.diskAvailMB = func() int { return 512 }
	if !s.dispatchDiskGated() {
		t.Error("not gated with free disk below floor")
	}
	if !s.dispatchDiskGated() {
		t.Error("gate did not hold on re-check")
	}

	s.diskAvailMB = func() int { return 100000 }
	if s.dispatchDiskGated() {
		t.Error("still gated after free disk recovered above floor")
	}

	s.diskAvailMB = func() int { return 10240 }
	if s.dispatchDiskGated() {
		t.Error("gated at exactly the floor; boundary is strict-below")
	}
}

// TestHeartbeatOnce_DiskFloorReportsGated: a pod below its disk floor admits
// no new dispatch, so placement must read it as gated and send queued work
// elsewhere rather than hold it for a pod that will not claim it.
func TestHeartbeatOnce_DiskFloorReportsGated(t *testing.T) {
	database := newDelegateTestDB(t)
	stores := testSpawnerStores(database)
	s := NewSpawner(database, stores, nil, nil, "")
	s.dispatcherRunning.Store(true)
	s.SetDispatchDiskFloor(10240)
	s.mu.Lock()
	s.diskAvailMB = func() int { return 512 }
	s.mu.Unlock()

	ctx := context.Background()
	const id = "hb-disk-gate"
	epoch, err := stores.Instances.Register(ctx, id, domain.InstanceRoleExecutor, "test-version", "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	s.SetExecutorID(id, epoch)

	s.heartbeatOnce(ctx)
	got, err := stores.Instances.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DispatchGated == nil || !*got.DispatchGated {
		t.Errorf("DispatchGated = %v, want true while free disk is below the floor", got.DispatchGated)
	}
}

// TestEvictWorkspacesForSpace_ReclaimsInsideTheIdleWindowBelowTheFloor: the
// tree TestEvictIdleWorkspaces_RefusesWithinTheIdleWindow keeps is reclaimed
// once free disk is below the floor, and kept while it is above.
func TestEvictWorkspacesForSpace_ReclaimsInsideTheIdleWindowBelowTheFloor(t *testing.T) {
	isolateRunNamespace(t)
	s, database, conversationID, _ := setupAdvanceFixture(t, "evict-pressure")
	wireBlobStore(t, s)
	_, wtPath := seedEvictableTree(t, s, database, conversationID)
	s.SetDispatchDiskFloor(10240)

	free := 100000
	s.mu.Lock()
	s.diskAvailMB = func() int { return free }
	s.mu.Unlock()
	s.EvictWorkspacesForSpace(context.Background())
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("tree %s evicted with free disk above the floor: %v", wtPath, err)
	}

	free = 512
	s.EvictWorkspacesForSpace(context.Background())
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatalf("tree %s survived a pass below the disk floor (stat err %v)", wtPath, err)
	}
}

// TestEvictWorkspacesForSpace_KeepsTheOnlyCopy: pressure moves the idle window
// and nothing else. A tree whose snapshot is still being written is the only
// copy of the agent's work, and a full disk is no licence to delete it.
func TestEvictWorkspacesForSpace_KeepsTheOnlyCopy(t *testing.T) {
	isolateRunNamespace(t)
	s, database, conversationID, _ := setupAdvanceFixture(t, "evict-pressure-pending")
	wireBlobStore(t, s)
	wsKey := taskIDForConversation(t, database, conversationID)
	wtPath := makeRunTree(t, wsKey)
	putTestSnapshot(t, s, wsKey)
	seedSnapshotState(t, s, wsKey, evictWriterClaim, domain.WorkspaceSnapshotPending)
	parkAged(t, database, conversationID, wtPath, "-2 hours")
	s.SetDispatchDiskFloor(10240)
	s.mu.Lock()
	s.diskAvailMB = func() int { return 512 }
	s.mu.Unlock()

	s.EvictWorkspacesForSpace(context.Background())

	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("tree %s with a pending snapshot evicted under disk pressure: %v", wtPath, err)
	}
}
