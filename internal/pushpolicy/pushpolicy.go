// Package pushpolicy owns one definition of "which refs may a delegated agent
// not push, and when may the team's policy lift that" — shared by the two
// places the rule is enforced.
//
//   - Multi mode: the per-run git proxy's ref gate (internal/delegate builds
//     the gitproxy.Decision this package's protected set shapes).
//   - Local mode: the TF-controlled pre-push hook (internal/githooks, via
//     `triagefactory hook check-push`).
//
// The rule lives here rather than in either enforcement point because two
// copies of it is how they drift.
//
// This is a SAFETY GUARD against a mistaken agent, not a security control
// against a hostile one. Local mode is not a security boundary and cannot be
// made one: the agent runs as the operator with unrestricted bash, so the
// pre-push hook is one `git push --no-verify` (or one rewritten remote) away
// from bypass, and nothing in this package defends against an agent that tries.
// What it does defend against is the failure mode that actually happens — an
// agent that never considered the rule and ran the naive command.
//
// Error posture is the caller's to choose, so every function returns its
// errors rather than picking a default. The multi-mode gate fails CLOSED (an
// unresolvable protected set denies the push — being unable to tell whether
// the live branch IS the base branch is exactly when a push must not go
// through); the local-mode hook fails OPEN (a guard that blocks pushes on a
// dead database turns a local convenience into a broken tool).
package pushpolicy

import (
	"context"
	"fmt"
	"sort"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ProtectedBranches returns the branch names that are candidates for
// protection on a repo, before the team's policy is applied. The universal
// default-branch names (main, master) are ALWAYS in the set, so a repo that
// has no profile yet — or whose profile omits the default — still can't be
// pushed to on them; the repo's recorded default branch and the user-configured
// base branch are added on top when the profile is readable.
//
// A profile lookup error is returned (not swallowed): resolving the set
// wrongly is worse than not resolving it, and the caller decides what an
// unresolvable set means. A nil profile is NOT an error — a repo that is
// configured but unprofiled, and a name no registry row answers to, both still
// get the universal set. This is why the lookup is ref-keyed: both enforcement
// points learn the repository from something outside the database (a push's
// git remote, the proxy's request path), so "no row for that name" is a state
// they must resolve to protection rather than to a failure. Turning it into an
// error would swing the answer to whichever way the caller fails — denying
// every push in multi, allowing a push to main in local.
func ProtectedBranches(ctx context.Context, stores db.Stores, orgID string, ref domain.RepoRef) (map[string]bool, error) {
	// Universal protected names, refused regardless of profile state.
	out := map[string]bool{"main": true, "master": true}
	if stores.Repos == nil {
		return nil, fmt.Errorf("resolve protected branches for %s: no repo store", ref.Slug())
	}
	profile, err := stores.Repos.GetByRefSystem(ctx, orgID, ref)
	if err != nil {
		return nil, fmt.Errorf("resolve protected branches for %s: %w", ref.Slug(), err)
	}
	if profile != nil {
		if profile.DefaultBranch != "" {
			out[profile.DefaultBranch] = true
		}
		if profile.BaseBranch != "" {
			out[profile.BaseBranch] = true
		}
	}
	return out, nil
}

// Allows reports whether the team's base-branch push policy permits a run to
// push a protected ref. eventTriggered is the run's dispatch shape
// (agenthost.ConversationInfo.IsEventTriggered): "manual_only" turns on exactly the
// runs a human kicked off, because an event-triggered run's task text comes
// from PR bodies, issue comments and labels — externally authored input that
// must never be able to talk its way past the team's gate.
//
// An unreadable or unrecognized stored policy resolves to the strictest
// answer (false, protection stays on): a degraded read may narrow permission,
// never widen it.
func Allows(ctx context.Context, stores db.Stores, teamID string, eventTriggered bool) (bool, error) {
	if stores.Teams == nil {
		return false, fmt.Errorf("resolve base-branch push policy: no team store")
	}
	if teamID == "" {
		return false, fmt.Errorf("resolve base-branch push policy: no team id")
	}
	settings, err := stores.Teams.GetSettingsSystem(ctx, teamID)
	if err != nil {
		return false, fmt.Errorf("resolve base-branch push policy for team %s: %w", teamID, err)
	}
	switch settings.BaseBranchPushPolicy {
	case domain.BaseBranchPushAlways:
		return true, nil
	case domain.BaseBranchPushManualOnly:
		return !eventTriggered, nil
	default:
		// "never", plus any value a future/rolled-back writer left behind.
		return false, nil
	}
}

// ProtectedFor is the composition both enforcement points call: the repo's
// protected branch names, or an EMPTY set when the team's policy permits this
// run to push them. An empty (non-nil) map means "nothing is protected here" —
// the caller can range over it uniformly either way.
func ProtectedFor(ctx context.Context, stores db.Stores, orgID, teamID string, ref domain.RepoRef, eventTriggered bool) (map[string]bool, error) {
	protected, err := ProtectedBranches(ctx, stores, orgID, ref)
	if err != nil {
		return nil, err
	}
	allowed, err := Allows(ctx, stores, teamID, eventTriggered)
	if err != nil {
		return nil, err
	}
	if allowed {
		return map[string]bool{}, nil
	}
	return protected, nil
}

// Refs renders a protected branch set as full refs (refs/heads/<name>), sorted
// so a denial message and an audit row read the same way twice. This is the
// form the git-proxy decision carries across the relay to the receive-pack
// gate, which compares against the refs a push names.
func Refs(protected map[string]bool) []string {
	if len(protected) == 0 {
		return nil
	}
	out := make([]string, 0, len(protected))
	for b := range protected {
		out = append(out, "refs/heads/"+b)
	}
	sort.Strings(out)
	return out
}
