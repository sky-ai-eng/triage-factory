package placement

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

type fakeInstances struct{ rows []domain.Instance }

func (f fakeInstances) List(context.Context) ([]domain.Instance, error) { return f.rows, nil }

type fakeOverrides struct {
	m map[string]*domain.PlacementOverride
}

func (f fakeOverrides) Get(_ context.Context, org, kind, val string) (*domain.PlacementOverride, error) {
	return f.m[org+"|"+kind+"|"+val], nil
}

func execRow(id string, maxRuns int, hbAgo time.Duration, now time.Time) domain.Instance {
	m := maxRuns
	return domain.Instance{
		ID:              id,
		Role:            domain.InstanceRoleExecutor,
		MaxRuns:         &m,
		LastHeartbeatAt: now.Add(-hbAgo),
	}
}

func TestResolve_DisabledIsNoOp(t *testing.T) {
	r := NewResolver(fakeInstances{}, fakeOverrides{}, Config{Enabled: false})
	plan, err := r.Resolve(context.Background(), "org1", domain.PlacementKindRepo, "acme/web")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Enabled || len(plan.PreferredSet) != 0 {
		t.Fatalf("disabled resolver must yield no preferred set: %+v", plan)
	}
	if plan.PreferredForRun("run-1") != "" {
		t.Fatalf("disabled plan must stamp nothing")
	}
}

func TestResolve_SingleOwner(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := NewResolver(fakeInstances{rows: []domain.Instance{
		execRow("exec-a", 4, time.Second, now),
		execRow("exec-b", 4, time.Second, now),
		execRow("exec-c", 4, time.Second, now),
	}}, fakeOverrides{}, Config{Enabled: true, Liveness: DefaultLiveness})
	r.SetNow(func() time.Time { return now })

	plan, err := r.Resolve(context.Background(), "org1", domain.PlacementKindRepo, "acme/web")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.PreferredSet) != 1 {
		t.Fatalf("default is a single owner, got %v", plan.PreferredSet)
	}
	// The single preferred must be the top rendezvous candidate.
	if plan.Candidates[0].InstanceID != plan.PreferredSet[0] || !plan.Candidates[0].Preferred {
		t.Fatalf("top candidate should be the preferred owner: %+v", plan.Candidates)
	}
	// Every run of the key stamps the same single owner.
	if plan.PreferredForRun("run-1") != plan.PreferredSet[0] || plan.PreferredForRun("run-2") != plan.PreferredSet[0] {
		t.Fatalf("single-owner plan must stamp the owner for every run")
	}
}

func TestResolve_ExcludesDeadDrainingControl(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	dead := execRow("exec-dead", 4, time.Hour, now)
	draining := execRow("exec-drain", 4, time.Second, now)
	draining.Draining = true
	control := domain.Instance{ID: "ctl", Role: domain.InstanceRoleControl, LastHeartbeatAt: now}
	nocap := domain.Instance{ID: "exec-nocap", Role: domain.InstanceRoleExecutor, LastHeartbeatAt: now} // MaxRuns nil
	live := execRow("exec-live", 4, time.Second, now)

	r := NewResolver(fakeInstances{rows: []domain.Instance{dead, draining, control, nocap, live}},
		fakeOverrides{}, Config{Enabled: true, Liveness: DefaultLiveness})
	r.SetNow(func() time.Time { return now })

	plan, err := r.Resolve(context.Background(), "o", domain.PlacementKindRepo, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.PreferredSet) != 1 || plan.PreferredSet[0] != "exec-live" {
		t.Fatalf("only the live executor is eligible; got preferred %v", plan.PreferredSet)
	}
	// The excluded rows are still surfaced with a reason.
	reasons := map[string]string{}
	eligible := 0
	for _, c := range plan.Candidates {
		if c.Eligible {
			eligible++
		} else {
			reasons[c.InstanceID] = c.Reason
		}
	}
	if eligible != 1 {
		t.Fatalf("exactly one eligible candidate, got %d", eligible)
	}
	for id, want := range map[string]string{
		"exec-dead": "dead", "exec-drain": "draining", "ctl": "not-an-executor", "exec-nocap": "no-capacity",
	} {
		if reasons[id] != want {
			t.Fatalf("candidate %s reason = %q, want %q", id, reasons[id], want)
		}
	}
}

func TestResolve_PinWinsOverHash(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rows := []domain.Instance{
		execRow("exec-a", 4, time.Second, now),
		execRow("exec-b", 4, time.Second, now),
	}
	ov := &domain.PlacementOverride{OrgID: "o", KeyKind: domain.PlacementKindRepo, KeyValue: "a/b", PinnedInstanceID: "exec-b"}
	r := NewResolver(fakeInstances{rows: rows}, fakeOverrides{m: map[string]*domain.PlacementOverride{
		"o|" + domain.PlacementKindRepo + "|a/b": ov,
	}}, Config{Enabled: true, Liveness: DefaultLiveness})
	r.SetNow(func() time.Time { return now })

	plan, err := r.Resolve(context.Background(), "o", domain.PlacementKindRepo, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.PreferredSet) != 1 || plan.PreferredSet[0] != "exec-b" {
		t.Fatalf("pin must force exec-b, got %v", plan.PreferredSet)
	}
	if plan.PreferredForRun("run-1") != "exec-b" {
		t.Fatalf("pinned plan stamps the pin for every run")
	}
}

func TestResolve_PinToOfflineInstanceStillPreferred(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rows := []domain.Instance{execRow("exec-a", 4, time.Second, now)}
	ov := &domain.PlacementOverride{OrgID: "o", KeyKind: domain.PlacementKindRepo, KeyValue: "a/b", PinnedInstanceID: "exec-ghost"}
	r := NewResolver(fakeInstances{rows: rows}, fakeOverrides{m: map[string]*domain.PlacementOverride{
		"o|" + domain.PlacementKindRepo + "|a/b": ov,
	}}, Config{Enabled: true, Liveness: DefaultLiveness})
	r.SetNow(func() time.Time { return now })

	plan, _ := r.Resolve(context.Background(), "o", domain.PlacementKindRepo, "a/b")
	if len(plan.PreferredSet) != 1 || plan.PreferredSet[0] != "exec-ghost" {
		t.Fatalf("a pin to an offline instance is still the preferred target (claim aging handles it): %v", plan.PreferredSet)
	}
	if !containsCandidate(plan.Candidates, "exec-ghost") {
		t.Fatalf("pinned-but-offline instance must be surfaced in candidates for the explainer")
	}
}

func TestResolve_ReplicasSpreadAcrossTopK(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rows := []domain.Instance{
		execRow("exec-a", 4, time.Second, now),
		execRow("exec-b", 4, time.Second, now),
		execRow("exec-c", 4, time.Second, now),
		execRow("exec-d", 4, time.Second, now),
	}
	ov := &domain.PlacementOverride{OrgID: "o", KeyKind: domain.PlacementKindRepo, KeyValue: "mono/repo", Replicas: 3}
	r := NewResolver(fakeInstances{rows: rows}, fakeOverrides{m: map[string]*domain.PlacementOverride{
		"o|" + domain.PlacementKindRepo + "|mono/repo": ov,
	}}, Config{Enabled: true, Liveness: DefaultLiveness})
	r.SetNow(func() time.Time { return now })

	plan, _ := r.Resolve(context.Background(), "o", domain.PlacementKindRepo, "mono/repo")
	if len(plan.PreferredSet) != 3 {
		t.Fatalf("replicas=3 → top-3 preferred, got %v", plan.PreferredSet)
	}
	// Runs spread across all 3 preferred replicas (deterministically).
	seen := map[string]bool{}
	for i := 0; i < 300; i++ {
		id := plan.PreferredForRun(conversationID(i))
		if !inSet(plan.PreferredSet, id) {
			t.Fatalf("stamp %q not in preferred set %v", id, plan.PreferredSet)
		}
		seen[id] = true
	}
	if len(seen) != 3 {
		t.Fatalf("hot-key runs should spread across all 3 replicas, hit only %v", seen)
	}
}

func inSet(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func conversationID(i int) string { return "run-" + string(rune('a'+i%26)) + "-" + itoa(i) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
