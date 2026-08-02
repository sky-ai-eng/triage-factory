import type { Conversation, Artifact } from '../types'

// Derived-approval helpers. A run never parks for
// approval; the "needs approval" state is a *view* over the
// run's unresolved-artifact set, projected onto the run as has_unresolved_artifacts
// + pending_artifact_ids + the per-kind counts. These helpers keep the
// count-aware labels and the resolve-all copy in one place so the dock, the board
// card, and the confirmation modal stay in lockstep.

export type ApprovalKind = 'review' | 'pr'

export interface ApprovalCounts {
  pr: number
  review: number
  total: number
}

// approvalCounts reads the run projection's per-kind unresolved counts. Absent
// fields (the server's transient-failure guard, or a run with no artifacts) read
// as 0 — a card only surfaces approval affordances when has_unresolved_artifacts
// is true, where the counts are guaranteed present.
export function approvalCounts(run: Conversation): ApprovalCounts {
  const pr = run.unresolved_pr_count ?? 0
  const review = run.unresolved_review_count ?? 0
  return { pr, review, total: pr + review }
}

// hasUnresolvedArtifacts is the single predicate every approval surface keys
// off — approval is never a run status. The flag is three-valued: the
// server emits an explicit true/false only when the answer is definitive (the
// artifact set was read), and OMITS it (undefined) under its transient-failure
// guard. So honor the authoritative boolean directly when present — a definitive
// `false` means "none", full stop — and fall back to the id set / counts only
// when it's undefined (older projections / the transient window).
export function hasUnresolvedArtifacts(run: Conversation | null | undefined): boolean {
  if (!run) return false
  if (run.has_unresolved_artifacts === false) return false
  if (run.has_unresolved_artifacts === true) return true
  // undefined → re-derive from whatever the projection did carry.
  if ((run.pending_artifact_ids?.length ?? 0) > 0) return true
  const { total } = approvalCounts(run)
  return total > 0
}

// approvalAction is the trailing verb on the "your move" affordance — count-aware:
// a single PR keeps "Open PR", a single review "Review", and any plural / mixed
// set collapses to "Review N items" (the full mixed-kind dock is TFAC-385). Used
// by the run-station dock and the board card so both read identically.
export function approvalAction({ pr, review, total }: ApprovalCounts): string {
  if (total === 0) return 'Review'
  if (pr > 0 && review > 0) return `Review ${total} items`
  if (pr > 0) return pr === 1 ? 'Open PR' : `Open ${pr} PRs`
  return review === 1 ? 'Review' : `Review ${review} items`
}

// approvalKicker is the toned uppercase lead-in above the action ("PR ready to
// open" / "2 reviews ready" / "3 items ready").
export function approvalKicker({ pr, review, total }: ApprovalCounts): string {
  if (total === 0) return 'Awaiting approval'
  if (pr > 0 && review > 0) return `${total} items ready`
  if (pr > 0) return pr === 1 ? 'PR ready to open' : `${pr} PRs ready to open`
  return review === 1 ? 'Review ready' : `${review} reviews ready`
}

// kindForArtifact maps an artifact to the overlay discriminator the per-item
// editors key off (PendingPROverlay / ReviewOverlay). Null for kinds with no
// approval overlay (branch / issue / comment) — those never appear in the
// unresolved set, but the guard keeps callers total.
export function kindForArtifact(a: Artifact): ApprovalKind | null {
  if (a.kind === 'pull_request') return 'pr'
  if (a.kind === 'review') return 'review'
  return null
}

// unresolvedArtifacts intersects a run's full artifact list with its
// pending_artifact_ids (the authoritative unresolved set — the ready-review
// predicate lives server-side, so artifact.state alone can't reconstruct it),
// returning the artifact rows in the backend's order (all draft PRs first, then
// ready reviews). Ids without a matching row (a fetch race) are skipped.
export function unresolvedArtifacts(artifacts: Artifact[], pendingIds: string[]): Artifact[] {
  if (pendingIds.length === 0) return []
  const byId = new Map(artifacts.map((a) => [a.id, a]))
  const out: Artifact[] = []
  for (const id of pendingIds) {
    const a = byId.get(id)
    if (a) out.push(a)
  }
  return out
}

// resolveAllSummary is the resolve-all confirmation copy (drag-to-Done /
// Return-to-queue): "Close 2 draft PRs and discard 1 pending review? Pushed
// branches are kept." A clause is dropped when its count is 0; the live note is
// appended only when the run is still executing (the teardown cancels it).
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
