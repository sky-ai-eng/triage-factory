package placement

import (
	"fmt"
	"math"
	"testing"
)

// TestRank_Deterministic: the same (key, members) always yields the same
// order, regardless of input order — the seed-free property every pod relies
// on to agree without coordination.
func TestRank_Deterministic(t *testing.T) {
	members := []Member{
		{InstanceID: "exec-a", Weight: 4},
		{InstanceID: "exec-b", Weight: 4},
		{InstanceID: "exec-c", Weight: 4},
	}
	got1 := ids(Rank("org1\x1frepo\x1facme/web", members))
	// Shuffle the input; the output order must not depend on it.
	shuffled := []Member{members[2], members[0], members[1]}
	got2 := ids(Rank("org1\x1frepo\x1facme/web", shuffled))
	if fmt.Sprint(got1) != fmt.Sprint(got2) {
		t.Fatalf("Rank not order-independent: %v vs %v", got1, got2)
	}
}

// TestRank_DropsZeroWeight: a member with no capacity is never a candidate.
func TestRank_DropsZeroWeight(t *testing.T) {
	members := []Member{
		{InstanceID: "exec-a", Weight: 4},
		{InstanceID: "exec-zero", Weight: 0},
		{InstanceID: "exec-neg", Weight: -3},
	}
	got := ids(Rank("k", members))
	if len(got) != 1 || got[0] != "exec-a" {
		t.Fatalf("expected only exec-a ranked, got %v", got)
	}
}

// TestRank_MembershipReshuffleIsLocal is the core rendezvous property and a
// direct acceptance item ("membership change reshuffles only affected keys").
// Removing one member may only move keys that member OWNED; every key owned
// by a surviving member must keep the same owner.
func TestRank_MembershipReshuffleIsLocal(t *testing.T) {
	full := []Member{
		{InstanceID: "exec-a", Weight: 4},
		{InstanceID: "exec-b", Weight: 4},
		{InstanceID: "exec-c", Weight: 4},
		{InstanceID: "exec-d", Weight: 4},
	}
	removed := "exec-c"
	reduced := make([]Member, 0, len(full))
	for _, m := range full {
		if m.InstanceID != removed {
			reduced = append(reduced, m)
		}
	}

	const N = 4000
	movedOwnedByOther := 0
	ownerChangedForRemoved := 0
	for i := 0; i < N; i++ {
		key := fmt.Sprintf("org\x1frepo\x1fk-%d", i)
		before := Rank(key, full)
		after := Rank(key, reduced)
		ownerBefore := before[0].InstanceID
		ownerAfter := after[0].InstanceID
		if ownerBefore == ownerAfter {
			continue
		}
		// Owner changed: the only legitimate cause is that the removed member
		// was the previous owner.
		if ownerBefore == removed {
			ownerChangedForRemoved++
			continue
		}
		movedOwnedByOther++
	}
	if movedOwnedByOther != 0 {
		t.Fatalf("rendezvous violated: %d keys owned by a surviving member changed owner when %s left", movedOwnedByOther, removed)
	}
	if ownerChangedForRemoved == 0 {
		t.Fatalf("sanity: expected some keys previously owned by %s to move; got 0 (hash may be degenerate)", removed)
	}
	t.Logf("%d/%d keys previously owned by %s reshuffled; 0 surviving-owner keys disturbed", ownerChangedForRemoved, N, removed)
}

// TestRank_WeightProportional: over many keys, a member weighted 4x wins
// roughly 4x as many keys as a 1x member. Loose bounds — this checks the
// weighting isn't inverted or ignored, not a precise distribution.
func TestRank_WeightProportional(t *testing.T) {
	members := []Member{
		{InstanceID: "big", Weight: 8},
		{InstanceID: "small", Weight: 2},
	}
	const N = 20000
	bigWins := 0
	for i := 0; i < N; i++ {
		key := fmt.Sprintf("o\x1frepo\x1fk-%d", i)
		if Rank(key, members)[0].InstanceID == "big" {
			bigWins++
		}
	}
	frac := float64(bigWins) / float64(N)
	// Expected 8/10 = 0.8; allow a wide tolerance for hash noise at this N.
	if frac < 0.72 || frac > 0.88 {
		t.Fatalf("weight-4x member won %.3f of keys, want ~0.80", frac)
	}
}

// TestRank_TieBreakStable: two members with identical weight still get a
// total, id-ordered tiebreak so the order is never nondeterministic.
func TestRank_TieBreakStable(t *testing.T) {
	// Force a score tie by using identical scores is impractical; instead
	// assert the order is total (no equal-score pair left unordered) by
	// checking scores are strictly non-increasing and any equal pair is
	// id-ordered.
	members := []Member{
		{InstanceID: "z", Weight: 5},
		{InstanceID: "a", Weight: 5},
		{InstanceID: "m", Weight: 5},
	}
	r := Rank("k\x1fk\x1fk", members)
	for i := 1; i < len(r); i++ {
		if r[i-1].Score < r[i].Score {
			t.Fatalf("not sorted descending at %d: %v", i, r)
		}
		if r[i-1].Score == r[i].Score && r[i-1].InstanceID > r[i].InstanceID {
			t.Fatalf("equal-score pair not id-ordered: %v", r)
		}
	}
}

// TestWeightedScore_Finite guards the (0,1] mapping: no member ever produces
// a NaN/Inf score that would corrupt the sort.
func TestWeightedScore_Finite(t *testing.T) {
	for i := 0; i < 1000; i++ {
		s := weightedScore("key", fmt.Sprintf("exec-%d", i), 1+i%16)
		if math.IsNaN(s) || math.IsInf(s, 0) || s <= 0 {
			t.Fatalf("degenerate score for exec-%d: %v", i, s)
		}
	}
}

func ids(rs []Ranked) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.InstanceID
	}
	return out
}
