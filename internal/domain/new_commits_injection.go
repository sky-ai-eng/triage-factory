package domain

import "fmt"

// PRNewCommitsInjection is the agent-facing copy for the new-commits freshness injection
// (TFAC-501): a PR a conversation is reviewing advanced under it while the agent was
// in-progress or parked. The injection names the PR and the old→new head so the agent
// knows exactly what moved, and tells it to re-pull and reconcile its pending
// review comments against the new diff before finalizing — the agent-facing half
// of the freshness loop TFAC-494 closes (the other half being the finalize gate).
//
// Bare (no <system-note> wrapper): the live path wraps one injection, the staged path
// bundles and wraps the block (StagedInjectionBlock). SHAs are shortened for
// readability — the agent works against the branch, not these literals, and the
// finalize-review reconciliation pins the real commit.
func PRNewCommitsInjection(repo string, prNumber int, oldHeadSHA, newHeadSHA string) string {
	return fmt.Sprintf(
		"PR #%d (%s) advanced from %s to %s while you were reviewing it. "+
			"Re-pull the branch and reconcile your pending review comments against the new diff before finalizing.",
		prNumber, repo, ShortSHA(oldHeadSHA), ShortSHA(newHeadSHA),
	)
}

// ShortSHA truncates a git SHA to a human-readable 12-char prefix, leaving
// shorter or empty values untouched. 12 hex chars is the conventional
// unambiguous abbreviation for a single repo; the injection is informational, so a
// prefix is clearer than a full 40-char hash repeated twice in one sentence.
func ShortSHA(sha string) string {
	const n = 12
	if len(sha) <= n {
		return sha
	}
	return sha[:n]
}
