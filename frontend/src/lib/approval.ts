import type { Conversation } from '../types'

// Derived-approval helpers. A conversation never parks for
// approval; the "needs approval" state is a *view* over the
// conversation's unresolved-deliverable set, projected onto the conversation as has_unresolved_deliverables
// + pending_deliverable_ids + the per-kind counts. These helpers keep the
// count-aware labels and the resolve-all copy in one place so every surface
// that raises them reads the same.

export interface ApprovalCounts {
  pr: number
  review: number
  total: number
}

// artifactSetKey derives a cheap change-key for a conversation's artifacts and
// deliverables from the projections that ride the conversation row
// (websocket-fresh), so a mounted list can refetch when either set changes —
// a live conversation acting on an object, new or already listed, or being
// refused, a resolve landing from another surface or tab — without polling.
export function artifactSetKey(conversation: Conversation): string {
  return `${conversation.artifact_count ?? 0}:${conversation.action_count ?? 0}:${(conversation.pending_deliverable_ids ?? []).join(',')}`
}

// approvalCounts reads the conversation projection's per-kind unresolved counts. Absent
// fields (the server's transient-failure guard, or a conversation with no deliverables) read
// as 0 — a card only surfaces approval affordances when has_unresolved_deliverables
// is true, where the counts are guaranteed present.
export function approvalCounts(conversation: Conversation): ApprovalCounts {
  const pr = conversation.unresolved_pr_count ?? 0
  const review = conversation.unresolved_review_count ?? 0
  return { pr, review, total: pr + review }
}

// hasUnresolvedDeliverables is the single predicate every approval surface keys
// off — approval is never a conversation status. The flag is three-valued: the
// server emits an explicit true/false only when the answer is definitive (the
// deliverable set was read), and OMITS it (undefined) under its transient-failure
// guard. So honor the authoritative boolean directly when present — a definitive
// `false` means "none", full stop — and fall back to the id set / counts only
// when it's undefined (older projections / the transient window).
export function hasUnresolvedDeliverables(conversation: Conversation | null | undefined): boolean {
  if (!conversation) return false
  if (conversation.has_unresolved_deliverables === false) return false
  if (conversation.has_unresolved_deliverables === true) return true
  // undefined → re-derive from whatever the projection did carry.
  if ((conversation.pending_deliverable_ids?.length ?? 0) > 0) return true
  const { total } = approvalCounts(conversation)
  return total > 0
}

// approvalAction is the trailing verb on the "your move" affordance — count-aware:
// a single item opens its editor directly ("Open PR" / "Review"), and any
// plural / mixed set opens the conversation's deliverable list to choose from ("Review N
// items"). Used by the RunStation dock and the board card so both read
// identically.
export function approvalAction({ pr, review, total }: ApprovalCounts): string {
  if (total === 0) return 'Review'
  if (pr > 0 && review > 0) return `Review ${total} items`
  if (pr > 0) return pr === 1 ? 'Open PR' : `Open ${pr} PRs`
  return review === 1 ? 'Review' : `Review ${review} items`
}

// resolveAllSummary is the resolve-all confirmation copy the task-end gesture
// raises: "Close 2 draft PRs and discard 1 pending review? Pushed
// branches are kept." A clause is dropped when its count is 0; the live note is
// appended only when the conversation is still executing (the teardown cancels it).
export function resolveAllSummary(pr: number, review: number, isLive: boolean): string {
  const parts: string[] = []
  if (pr > 0) parts.push(`close ${pr} draft PR${pr === 1 ? '' : 's'}`)
  if (review > 0) parts.push(`discard ${review} pending review${review === 1 ? '' : 's'}`)
  const body =
    parts.length > 0
      ? parts.join(' and ').replace(/^./, (c) => c.toUpperCase())
      : 'Resolve all open deliverables'
  const live = isLive ? ' The in-progress run will be cancelled.' : ''
  return `${body}? Pushed branches are kept.${live}`
}
