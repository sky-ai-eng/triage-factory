package placement

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// InstanceLister is the registry read the resolver needs — the full
// instances list, which it filters to live executor-capable candidates
// itself. Satisfied by db.InstanceStore. Kept as a one-method interface so
// this package doesn't import internal/db (which imports domain, which would
// otherwise cycle back).
type InstanceLister interface {
	List(ctx context.Context) ([]domain.Instance, error)
}

// OverrideReader is the placement_overrides read the resolver needs.
// Satisfied by db.PlacementOverrideStore. Returns (nil, nil) for a key with
// no override — the common case.
type OverrideReader interface {
	Get(ctx context.Context, orgID, keyKind, keyValue string) (*domain.PlacementOverride, error)
}

// Config tunes the resolver + the two-tier claim it feeds. The zero value
// (Enabled=false) is the whole layer switched off: Resolve returns an empty
// (no-preferred) plan and callers stamp nothing, so the claim sees only NULL
// preferreds and reverts to global-oldest — the pre-placement dispatcher,
// byte-identical. This is the "drop the entire placement layer leaves all
// tests green" contract (spec §6 acceptance).
type Config struct {
	Enabled bool

	// Liveness is the heartbeat-staleness window for candidacy: an instance
	// whose last heartbeat is older than this is dead and cannot own a key.
	// Also the window the claim uses to decide a stamped preferred is no
	// longer a live claimant (dead → immediate spillover). Advisory here
	// (Go-clock, tolerant of skew); the claim re-checks it authoritatively
	// in DB time.
	Liveness time.Duration

	// Aging is the tier-2 spillover delay: how long a queued conversation waits
	// for its preferred owner before any executor may claim it. Not used by the
	// resolver's ranking — carried so the explainer can report the exact
	// number the claim runs with.
	Aging time.Duration
}

// Resolver computes a placement Plan for a key from live registry membership
// and any override. Pure-function core (Rank) plus two reads; holds no
// per-key state, so it self-heals on every membership change with nothing to
// invalidate. Safe for concurrent use if its dependencies are (the stores
// are).
type Resolver struct {
	instances InstanceLister
	overrides OverrideReader
	cfg       Config

	// now is injectable so tests can pin liveness without sleeping; production
	// leaves it nil and gets time.Now.
	now func() time.Time
}

// NewResolver builds a Resolver. A nil instances or overrides reader is
// tolerated (Resolve treats them as "no members" / "no override") so partial
// wiring degrades to no-affinity rather than panicking.
func NewResolver(instances InstanceLister, overrides OverrideReader, cfg Config) *Resolver {
	return &Resolver{instances: instances, overrides: overrides, cfg: cfg}
}

// SetNow overrides the clock (tests only).
func (r *Resolver) SetNow(now func() time.Time) { r.now = now }

func (r *Resolver) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// Enabled reports whether the placement layer is on.
func (r *Resolver) Enabled() bool { return r.cfg != Config{} && r.cfg.Enabled }

// Candidate is one instance's placement standing for a key: its rendezvous
// rank plus the liveness/eligibility facts that placed (or excluded) it.
// Carries everything the explainer prints and everything PreferredForConversation
// needs, so a Plan is self-contained.
type Candidate struct {
	InstanceID string
	Weight     int
	Score      float64

	// BootEpoch is the instance's registration epoch, carried through from
	// the registry row so a consumer that persists a placement decision can
	// record which boot it chose without a second registry read. 0 for a
	// pin to an instance that isn't a live registry row.
	BootEpoch int64

	// Eligible is true when this instance was a ranking candidate (live,
	// executor-capable, not draining, weight > 0). An ineligible instance is
	// still listed (with why) so the explainer can show a dead/draining
	// member instead of silently omitting it — but it never wins a key.
	Eligible bool

	// Preferred is true when this instance is in the key's preferred set:
	// the pin, or the top-Replicas (default top-1) of the eligible order.
	Preferred bool

	// Reason is a short human tag for the explainer: "" when plainly ranked,
	// otherwise why it was excluded or specially placed ("dead", "draining",
	// "not-an-executor", "no-capacity", "pinned"). Note "gated" is deliberately
	// NOT here: a momentarily dispatch-gated executor stays an eligible
	// rendezvous candidate (gating is transient and handled at claim time), so
	// the resolver never tags one.
	Reason string
}

// Plan is the resolved placement for one key: the full candidate order
// (eligible and not) and the preferred set that the enqueue stamp draws
// from. Empty PreferredSet means no live owner — the conversation is stamped
// NULL and claimable by anyone immediately.
type Plan struct {
	Enabled    bool
	OrgID      string
	KeyKind    string
	KeyValue   string
	Candidates []Candidate // full order, eligible first (rendezvous), then excluded
	// PreferredSet is the ordered preferred instance ids (pin, or top-K).
	// PreferredForConversation draws the single stamp from it.
	PreferredSet []string
	// Override is the row in effect, or nil. Surfaced by the explainer.
	Override *domain.PlacementOverride
	Aging    time.Duration
	Liveness time.Duration
}

// PreferredForConversation picks the single preferred_executor_id to stamp on a
// conversation from the plan's preferred set. With one owner (the default and
// every pin) it is that owner; with a hot-key replica count it spreads
// conversations across the top-K deterministically by conversation id, so all K
// caches stay warm and no single replica head-of-line-blocks the key. Empty
// when there is no live owner.
func (p Plan) PreferredForConversation(conversationID string) string {
	switch len(p.PreferredSet) {
	case 0:
		return ""
	case 1:
		return p.PreferredSet[0]
	default:
		idx := hash64(conversationID, "") % uint64(len(p.PreferredSet))
		return p.PreferredSet[idx]
	}
}

// Resolve computes the Plan for (orgID, keyKind, keyValue), used at enqueue
// to stamp a conversation. A disabled resolver short-circuits to a plan with no
// preferred set (callers stamp nothing). Any store error is returned; the
// caller decides whether to proceed with no affinity (enqueue does — a failed
// placement read must never block minting work).
func (r *Resolver) Resolve(ctx context.Context, orgID, keyKind, keyValue string) (Plan, error) {
	if !r.Enabled() {
		return Plan{OrgID: orgID, KeyKind: keyKind, KeyValue: keyValue, Aging: r.cfg.Aging, Liveness: r.cfg.Liveness}, nil
	}
	return r.computePlan(ctx, orgID, keyKind, keyValue)
}

// Explain computes the Plan the same way Resolve does but ALWAYS evaluates the
// full candidate order, even when placement is disabled — so the operator
// explainer (GET /api/fleet/placement) can preview exactly who would own a key
// before the layer is switched on. The plan's Enabled field still reports the
// live config, so the caller can tell an advisory-off view from a live one.
func (r *Resolver) Explain(ctx context.Context, orgID, keyKind, keyValue string) (Plan, error) {
	return r.computePlan(ctx, orgID, keyKind, keyValue)
}

func (r *Resolver) computePlan(ctx context.Context, orgID, keyKind, keyValue string) (Plan, error) {
	plan := Plan{
		Enabled:  r.Enabled(),
		OrgID:    orgID,
		KeyKind:  keyKind,
		KeyValue: keyValue,
		Aging:    r.cfg.Aging,
		Liveness: r.cfg.Liveness,
	}

	var rows []domain.Instance
	if r.instances != nil {
		var err error
		rows, err = r.instances.List(ctx)
		if err != nil {
			return plan, err
		}
	}

	var override *domain.PlacementOverride
	if r.overrides != nil {
		ov, err := r.overrides.Get(ctx, orgID, keyKind, keyValue)
		if err != nil {
			return plan, err
		}
		override = ov
	}
	plan.Override = override

	// Split the registry into rankable members and pre-tagged exclusions.
	cutoff := r.clock().Add(-r.livenessOrDefault())
	members := make([]Member, 0, len(rows))
	excluded := make([]Candidate, 0)
	bootByID := make(map[string]int64, len(rows))
	for _, in := range rows {
		bootByID[in.ID] = in.BootEpoch
		if reason, ok := ineligibleReason(in, cutoff); ok {
			excluded = append(excluded, Candidate{
				InstanceID: in.ID,
				Weight:     maxRunsOrZero(in),
				BootEpoch:  in.BootEpoch,
				Reason:     reason,
			})
			continue
		}
		members = append(members, Member{InstanceID: in.ID, Weight: maxRunsOrZero(in)})
	}

	ranked := Rank(keyFor(orgID, keyKind, keyValue), members)
	plan.PreferredSet = preferredSet(ranked, override)

	preferred := make(map[string]bool, len(plan.PreferredSet))
	for _, id := range plan.PreferredSet {
		preferred[id] = true
	}
	for _, rk := range ranked {
		c := Candidate{
			InstanceID: rk.InstanceID,
			Weight:     rk.Weight,
			Score:      rk.Score,
			BootEpoch:  bootByID[rk.InstanceID],
			Eligible:   true,
			Preferred:  preferred[rk.InstanceID],
		}
		if c.Preferred && override != nil && override.PinnedInstanceID == rk.InstanceID {
			c.Reason = "pinned"
		}
		plan.Candidates = append(plan.Candidates, c)
	}
	// A pin to an instance that isn't a live ranking candidate still counts as
	// preferred (human intent overrides liveness) — surface it so the
	// explainer's preferred set matches the claim's tier-1 target.
	if override != nil && override.PinnedInstanceID != "" && !containsCandidate(plan.Candidates, override.PinnedInstanceID) {
		plan.Candidates = append(plan.Candidates, Candidate{
			InstanceID: override.PinnedInstanceID,
			Preferred:  true,
			Reason:     "pinned",
		})
	}
	plan.Candidates = append(plan.Candidates, excluded...)
	return plan, nil
}

// preferredSet derives the ordered preferred ids from the ranked order and
// any override. A pin wins outright (single-element set, live or not). A
// replica count takes the top-K of the eligible order. Otherwise the single
// rendezvous winner. Empty when nothing is eligible and there is no pin.
func preferredSet(ranked []Ranked, override *domain.PlacementOverride) []string {
	if override != nil && override.PinnedInstanceID != "" {
		return []string{override.PinnedInstanceID}
	}
	if len(ranked) == 0 {
		return nil
	}
	k := 1
	if override != nil && override.Replicas > 1 {
		k = override.Replicas
	}
	if k > len(ranked) {
		k = len(ranked)
	}
	ids := make([]string, 0, k)
	for i := 0; i < k; i++ {
		ids = append(ids, ranked[i].InstanceID)
	}
	return ids
}

// ineligibleReason reports whether an instance can NOT be a rendezvous owner
// and why. Excluded: non-executor roles (placement only runs in multi mode,
// where every dispatcher is an executor — a control pod runs none, and the
// all role is local-only and never registers into a multi fleet), a dead
// heartbeat, an operator drain, or no advertised capacity. A momentarily
// memory-gated executor is NOT excluded here — gating is transient and
// dropping it would thrash every key it owns on each gate/ungate; the claim
// handles a gated preferred via its own spillover.
func ineligibleReason(in domain.Instance, liveCutoff time.Time) (string, bool) {
	if in.Role != domain.InstanceRoleExecutor {
		return "not-an-executor", true
	}
	if in.LastHeartbeatAt.Before(liveCutoff) {
		return "dead", true
	}
	if in.Draining {
		return "draining", true
	}
	if maxRunsOrZero(in) <= 0 {
		return "no-capacity", true
	}
	return "", false
}

func maxRunsOrZero(in domain.Instance) int {
	if in.MaxRuns == nil {
		return 0
	}
	return *in.MaxRuns
}

func (r *Resolver) livenessOrDefault() time.Duration {
	if r.cfg.Liveness > 0 {
		return r.cfg.Liveness
	}
	return DefaultLiveness
}

func containsCandidate(cs []Candidate, id string) bool {
	for _, c := range cs {
		if c.InstanceID == id {
			return true
		}
	}
	return false
}

// keyFor builds the rendezvous key string. orgID is folded in so two orgs
// that share a repo name never rendezvous onto the same owner, and the kind
// separates the key's namespace from any future one. The 0x1f unit
// separator keeps the three fields unambiguous.
func keyFor(orgID, keyKind, keyValue string) string {
	return orgID + "\x1f" + keyKind + "\x1f" + keyValue
}
