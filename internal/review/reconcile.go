// Package review holds the shared forward-mapping logic for staged review
// comments (TFAC-497/499/500). A staged comment is anchored to a commit on a
// new-side line; when the reviewed PR advances, every consumer needs the same
// answer per comment — does its code still live where the anchor says, and if so
// where now? Three callers ask it:
//
//   - the agent-facing finalize gate (cmd/exec/agenthost), which reconciles every
//     comment to the PR head and HARD-FAILS if any is outdated, before arming the
//     review for approval;
//   - the human-facing freshness read (internal/server reviewArtifactDetails),
//     which classifies each comment current/moved/outdated against the live
//     head for a badge, degrading to "unknown" on any error;
//   - the human-facing Refresh action (internal/server), which re-pins survivors
//     to the live head and DROPS the outdated ones.
//
// They differ only in policy (hard-fail vs. degrade vs. drop); the transform —
// forward-map an anchor, remap a survivor, classify an anchor as outdated — is
// identical, so it lives here once instead of diverging across packages.
package review

import (
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// Freshness verdicts on the wire (TFAC-500) — the human-facing vocabulary the
// review overlay renders as a per-comment badge.
const (
	// FreshnessCurrent: anchored code unchanged at the same line (LineMapUnchanged).
	FreshnessCurrent = "current"
	// FreshnessMoved: anchored code survived but relocated — line shifted and/or
	// file renamed (LineMapMoved). The new position is on the Outcome's comment.
	FreshnessMoved = "moved"
	// FreshnessOutdated: anchored code changed or was deleted (LineMapOutdated).
	FreshnessOutdated = "outdated"
	// FreshnessUnknown: freshness couldn't be computed — no line anchor, no anchor
	// commit, or the anchor→head map couldn't be loaded.
	FreshnessUnknown = "unknown"
)

// Freshness maps a forward-map status to its wire verdict. An unrecognized status
// (the zero LineMapStatus) reads as "unknown".
func Freshness(status ghclient.LineMapStatus) string {
	switch status {
	case ghclient.LineMapUnchanged:
		return FreshnessCurrent
	case ghclient.LineMapMoved:
		return FreshnessMoved
	case ghclient.LineMapOutdated:
		return FreshnessOutdated
	default:
		return FreshnessUnknown
	}
}

// Outcome is the per-comment result of reconciling one staged comment forward to a
// target head. Exactly one of the descriptive states holds:
//
//   - Unpositioned: the comment had no line anchor (nil Line) — nothing to map.
//   - MapErr != nil: the comment is positioned but its anchor→head line-map
//     couldn't be loaded (a transient compare failure, or no anchor commit at
//     all). Comment is returned unchanged.
//   - otherwise classified: Status is set, and Comment is the SURVIVOR remapped to
//     the target head (new line/start/path, CommitSHA re-anchored) when
//     Survived is true, or the original comment unchanged when Status is
//     LineMapOutdated.
//
// Comment always carries a usable value (remapped or original) so a caller that
// keeps everything can just read Outcome.Comment regardless of state.
type Outcome struct {
	Comment      domain.ReviewArtifactComment
	Status       ghclient.LineMapStatus
	Survived     bool
	Unpositioned bool
	MapErr       error
}

// Outdated reports whether this comment's anchored code changed or was deleted at
// the target head (a classified, non-surviving outcome).
func (o Outcome) Outdated() bool { return o.Status == ghclient.LineMapOutdated }

// Classified reports whether a freshness verdict could be computed (the comment
// was positioned and its map loaded). When false, Status is meaningless and the
// caller should treat the comment as "unknown".
func (o Outcome) Classified() bool { return !o.Unpositioned && o.MapErr == nil }

// LineMapFor resolves the forward line-map for one anchor commit → the target
// head. ReconcileToHead calls it at most once per distinct anchor (it caches the
// result, error included), so implementations need not memoize. A non-nil error
// marks the comment(s) on that anchor as unresolved (MapErr) rather than aborting
// the whole reconcile — the caller decides whether that's fatal.
type LineMapFor func(anchor string) (ghclient.LineMap, error)

// ReconcileToHead forward-maps every comment to head and returns one Outcome per
// comment, in input order. fallbackAnchor is used as the anchor commit for a
// comment whose CommitSHA is empty (rows staged before per-comment anchoring).
//
// Policy lives in the caller, not here: ReconcileToHead never fails and never
// drops — it classifies. A finalize gate treats any MapErr/Outdated as fatal; a
// Refresh keeps survivors and drops Outdated; a freshness read maps each Outcome
// to a badge. The per-anchor line-map is fetched once and cached, so N comments
// sharing the finalize head cost one resolver call.
func ReconcileToHead(comments []domain.ReviewArtifactComment, head, fallbackAnchor string, lineMapFor LineMapFor) []Outcome {
	type cached struct {
		lm  ghclient.LineMap
		err error
	}
	cache := map[string]cached{}
	resolve := func(anchor string) (ghclient.LineMap, error) {
		if c, ok := cache[anchor]; ok {
			return c.lm, c.err
		}
		lm, err := lineMapFor(anchor)
		cache[anchor] = cached{lm: lm, err: err}
		return lm, err
	}

	out := make([]Outcome, 0, len(comments))
	for _, c := range comments {
		out = append(out, reconcileOne(c, head, fallbackAnchor, resolve))
	}
	return out
}

func reconcileOne(c domain.ReviewArtifactComment, head, fallbackAnchor string, resolve LineMapFor) Outcome {
	if c.Line == nil {
		// No anchor to forward-map. Returned unchanged; the approve path already
		// rejects an unpositioned comment, so we don't invent a verdict for it.
		return Outcome{Comment: c, Unpositioned: true}
	}
	anchor := c.CommitSHA
	if anchor == "" {
		anchor = fallbackAnchor
	}
	if anchor == "" {
		// Positioned but no anchor commit at all (legacy row, no fallback): we can't
		// prove anything, so report it unresolved rather than guess.
		return Outcome{Comment: c, MapErr: errNoAnchor}
	}
	if anchor == head {
		// Already at the target head — coherent by definition, no map needed. Make
		// the anchor explicit so a downstream submit pins one commit_id.
		c.CommitSHA = head
		return Outcome{Comment: c, Status: ghclient.LineMapUnchanged, Survived: true}
	}

	lm, err := resolve(anchor)
	if err != nil {
		return Outcome{Comment: c, MapErr: err}
	}
	res := lm.MapComment(c.Path, *c.Line, c.StartLine)
	next, survived := Apply(c, res, head)
	return Outcome{Comment: next, Status: res.Status, Survived: survived}
}

// Apply remaps a surviving comment to its new-side position and re-anchors it to
// head, returning the updated comment and survived=true. An outdated result
// returns the comment unchanged and survived=false. The input is taken by value
// and Line/StartLine are reassigned to fresh pointers, so the caller's comment is
// never mutated in place.
func Apply(c domain.ReviewArtifactComment, res ghclient.LineMapResult, head string) (domain.ReviewArtifactComment, bool) {
	switch res.Status {
	case ghclient.LineMapUnchanged, ghclient.LineMapMoved:
		newLine := res.Line
		c.Line = &newLine
		if c.StartLine != nil {
			newStart := res.StartLine
			c.StartLine = &newStart
		}
		if res.Path != "" {
			c.Path = res.Path
		}
		c.CommitSHA = head
		return c, true
	default: // ghclient.LineMapOutdated
		return c, false
	}
}

// errNoAnchor marks a positioned comment with no resolvable anchor commit. It's a
// sentinel for the MapErr field, surfaced as "unknown" by freshness consumers.
var errNoAnchor = anchorError("staged comment has no anchor commit to forward-map from")

type anchorError string

func (e anchorError) Error() string { return string(e) }
