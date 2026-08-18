import type { Conversation } from '../types'

// Derived-approval helpers. A conversation never parks for
// approval; the "needs approval" state is a *view* over the
// conversation's unresolved-artifact set, projected onto the conversation as has_unresolved_artifacts
// + pending_artifact_ids + the per-kind counts. These helpers keep the
// count-aware labels and the resolve-all copy in one place so the dock, the board
// card, and the confirmation modal stay in lockstep.

export interface ApprovalCounts {
  pr: number
  review: number
  total: number
}

// artifactSetKey derives a cheap change-key for a conversation's artifact set from the
// projections that ride the conversation row (websocket-fresh), so an always-mounted
// ArtifactList can refetch when the set changes shape — a live conversation minting a
// new PR, a resolve landing from another surface or tab — without polling.
export function artifactSetKey(conversation: Conversation): string {
  return `${conversation.artifact_count ?? 0}:${(conversation.pending_artifact_ids ?? []).join(',')}`
}

// approvalCounts reads the conversation projection's per-kind unresolved counts. Absent
// fields (the server's transient-failure guard, or a conversation with no artifacts) read
// as 0 — a card only surfaces approval affordances when has_unresolved_artifacts
// is true, where the counts are guaranteed present.
export function approvalCounts(conversation: Conversation): ApprovalCounts {
  const pr = conversation.unresolved_pr_count ?? 0
  const review = conversation.unresolved_review_count ?? 0
  return { pr, review, total: pr + review }
}

// hasUnresolvedArtifacts is the single predicate every approval surface keys
// off — approval is never a conversation status. The flag is three-valued: the
// server emits an explicit true/false only when the answer is definitive (the
// artifact set was read), and OMITS it (undefined) under its transient-failure
// guard. So honor the authoritative boolean directly when present — a definitive
// `false` means "none", full stop — and fall back to the id set / counts only
// when it's undefined (older projections / the transient window).
export function hasUnresolvedArtifacts(conversation: Conversation | null | undefined): boolean {
  if (!conversation) return false
  if (conversation.has_unresolved_artifacts === false) return false
  if (conversation.has_unresolved_artifacts === true) return true
  // undefined → re-derive from whatever the projection did carry.
  if ((conversation.pending_artifact_ids?.length ?? 0) > 0) return true
  const { total } = approvalCounts(conversation)
  return total > 0
}

// approvalAction is the trailing verb on the "your move" affordance — count-aware:
// a single item opens its editor directly ("Open PR" / "Review"), and any
// plural / mixed set opens the conversation's artifact list to choose from ("Review N
// items"). Used by the RunStation dock and the board card so both read
// identically.
export function approvalAction({ pr, review, total }: ApprovalCounts): string {
  if (total === 0) return 'Review'
  if (pr > 0 && review > 0) return `Review ${total} items`
  if (pr > 0) return pr === 1 ? 'Open PR' : `Open ${pr} PRs`
  return review === 1 ? 'Review' : `Review ${review} items`
}

// approvalKicker is the toned uppercase lead-in above the action ("PR ready to
// open" / "2 reviews ready"). A mixed set keeps the per-kind breakdown
// ("2 PRs · 1 review ready") rather than collapsing to a bare item count —
// what's waiting is the information the row exists to carry.
export function approvalKicker({ pr, review, total }: ApprovalCounts): string {
  if (total === 0) return 'Awaiting approval'
  if (pr > 0 && review > 0)
    return `${pr} PR${pr === 1 ? '' : 's'} · ${review} review${review === 1 ? '' : 's'} ready`
  if (pr > 0) return pr === 1 ? 'PR ready to open' : `${pr} PRs ready to open`
  return review === 1 ? 'Review ready' : `${review} reviews ready`
}

// resolveAllSummary is the resolve-all confirmation copy (drag-to-Done /
// Return-to-queue): "Close 2 draft PRs and discard 1 pending review? Pushed
// branches are kept." A clause is dropped when its count is 0; the live note is
// appended only when the conversation is still executing (the teardown cancels it).
export function resolveAllSummary(pr: number, review: number, isLive: boolean): string {
  const parts: string[] = []
  if (pr > 0) parts.push(`close ${pr} draft PR${pr === 1 ? '' : 's'}`)
  if (review > 0) parts.push(`discard ${review} pending review${review === 1 ? '' : 's'}`)
  const body =
    parts.length > 0
      ? parts.join(' and ').replace(/^./, (c) => c.toUpperCase())
      : 'Resolve all open artifacts'
  const live = isLive ? ' The in-progress run will be cancelled.' : ''
  return `${body}? Pushed branches are kept.${live}`
}
