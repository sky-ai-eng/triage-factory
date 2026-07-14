// Package placement is the horizontal-scaling affinity layer (TFAC-587,
// spec §6): a capacity-weighted rendezvous hash that maps a per-org cache
// key — (org, repo) for delegation, (org, project) for curator — onto the
// live executor fleet, so a repeatedly-hit repo tends to execute on the same
// executor and keeps its bare/worktree cache warm (TFAC-60 economics
// preserved at N>1).
//
// The governing doctrine is *the queue is the truth; placement is a
// preference*: nothing here carries correctness. A self-contained multi-mode
// run rehydrates on any executor, so a stale, wrong, or absent placement
// answer only costs a cold clone, never a broken run. That is why the map is
// a pure function of live membership rather than a stored routing table —
// any pod computes it identically, a join/leave reshuffles only the affected
// keys, and there is no mutable state to drift or rebalance.
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
// member's hash mapped into (0,1]; -ln(u) is a positive Exp(1)-distributed
// value, and dividing weight by it yields the max-is-winner statistic whose
// argmax is chosen with probability proportional to weight.
func weightedScore(key, instanceID string, weight int) float64 {
	h := hash64(key, instanceID)
	// Map to (0,1]: (h+1)/2^64 lands in (0,1], never 0, so ln is finite and
	// the division never blows up. 2^64 as a float is exact.
	u := (float64(h) + 1.0) / (float64(math.MaxUint64) + 1.0)
	return -float64(weight) / math.Log(u)
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
