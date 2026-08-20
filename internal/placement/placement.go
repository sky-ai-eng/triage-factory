// Package placement is the horizontal-scaling affinity layer (spec §6): a
// capacity-weighted rendezvous hash that maps a per-org cache key —
// (org, repo) for delegation, (org, project) for curator — onto the live
// executor fleet, so a repeatedly-hit repo tends to execute on the same
// executor and keeps its bare/worktree cache warm (the per-(org, repo) cache
// economics preserved at N>1).
//
// The governing doctrine is *the queue is the truth; placement is a
// preference*: nothing here carries correctness. A self-contained multi-mode
// conversation rehydrates on any executor, so a stale, wrong, or absent
// placement answer only costs a cold clone, never a broken conversation. That
// is why the map is a pure function of live membership rather than a stored
// routing table — any pod computes it identically, a join/leave reshuffles
// only the affected keys, and there is no mutable state to drift or
// rebalance.
//
// This file is the pure kernel: a deterministic, seed-free weighted
// rendezvous ranking with no I/O. resolver.go layers live-membership +
// override reads on top; the two-tier claim (internal/db) and the enqueue
// stamp (internal/delegate) consume its output.
package placement

import (
	"math"
	"sort"
)

// Member is one candidate for a rendezvous key: an executor-capable
// registry instance and its capacity weight (instances.max_runs). Only
// members with Weight > 0 can own a key — a zero/negative weight is "no
// slots to be an owner" and is dropped by Rank.
type Member struct {
	InstanceID string
	Weight     int
}

// Ranked is one member's place in the computed candidate order for a key:
// its weighted-rendezvous Score and the Weight that produced it. The
// explainer (GET /api/fleet/placement) surfaces this list verbatim so the
// operator sees exactly why an executor won or lost a key.
type Ranked struct {
	InstanceID string
	Weight     int
	Score      float64
}

// Rank orders members by descending weighted-rendezvous score for key. It is
// the whole placement map: deterministic and seed-free, so every pod derives
// the identical order from the same (key, members), and adding or removing a
// member reshuffles only the keys that member would win — the rendezvous
// (highest-random-weight) property.
//
// Weighting follows the standard weighted-rendezvous construction: each
// member hashes (key, id) to a uniform u in (0,1] and scores
// -weight / ln(u), so a member's probability of winning a key is proportional
// to its weight. Members with Weight <= 0 are dropped (no capacity to own a
// key). Ties — equal scores, or the impossible-in-practice identical hash —
// break by instance id ascending, making the order total and stable. Input
// slice is never mutated.
func Rank(key string, members []Member) []Ranked {
	ranked := make([]Ranked, 0, len(members))
	for _, m := range members {
		if m.Weight <= 0 {
			continue
		}
		ranked = append(ranked, Ranked{
			InstanceID: m.InstanceID,
			Weight:     m.Weight,
			Score:      weightedScore(key, m.InstanceID, m.Weight),
		})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		return ranked[i].InstanceID < ranked[j].InstanceID
	})
	return ranked
}

// weightedScore is the per-(key, member) weighted-rendezvous score. u is the
// member's hash mapped strictly into (0,1); -ln(u) is a positive
// Exp(1)-distributed value, and dividing weight by it yields the max-is-winner
// statistic whose argmax is chosen with probability proportional to weight.
func weightedScore(key, instanceID string, weight int) float64 {
	return -float64(weight) / math.Log(hashToUnit(hash64(key, instanceID)))
}

// hashToUnit maps a 64-bit hash strictly into the open interval (0,1). It
// takes the top 53 bits (float64's exact-integer range) and divides by 2^53,
// so the result lands in [0,1) with no rounding to exactly 1 — the naive
// (h+1)/2^64 rounds UP to u==1 for the largest hashes (float64(MaxUint64)
// already IS 2^64, and the +1s vanish), which would make ln(u)==0 and the
// weightedScore ±Inf and corrupt the ordering. The v==0 corner (top 53 bits
// all zero) is nudged to the smallest step so u is never 0 either; ln stays
// finite and negative across the whole domain.
func hashToUnit(h uint64) float64 {
	u := float64(h>>11) / (1 << 53)
	if u == 0 {
		return 1.0 / (1 << 53)
	}
	return u
}

// hash64 is a deterministic, seed-free 64-bit hash of key ⊕ instanceID —
// deliberately NOT hash/maphash (per-process random seed) so every pod in
// the fleet computes byte-identical rendezvous scores. FNV-1a over the two
// fields (separated by a 0x1f unit byte so "ab"+"c" and "a"+"bc" never
// collide) followed by a splitmix64 finalizer, which gives FNV the avalanche
// its raw output lacks — adjacent instance ids must not produce correlated
// scores or the weighting skews.
func hash64(key, instanceID string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	writeFNV := func(s string) {
		for i := 0; i < len(s); i++ {
			h ^= uint64(s[i])
			h *= prime64
		}
	}
	writeFNV(key)
	h ^= 0x1f
	h *= prime64
	writeFNV(instanceID)
	return splitmix64(h)
}

// splitmix64 is the standard SplittableRandom finalizer — a strong 64-bit
// avalanche mix used here purely as a hash finalizer, not a PRNG.
func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}
