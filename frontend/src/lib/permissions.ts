// Shared core for delegated-run tool-permission prompts (the `canUseTool`
// round-trip). When a run hits an off-allowlist tool the backend records the
// prompt, broadcasts a `permission_request` WS event carrying only its
// tool_call_id, and parks the agent's turn until the user answers via
// POST .../permissions/{toolCallID} or a server-side timeout denies it.
//
// The frame is a hint, never the carrier: every surface reads the prompt itself
// from GET /api/agent/conversations/{id}/permissions. That costs one round-trip
// before a prompt renders — nothing, for a rare human-paced event — and buys
// the three cases a fire-once frame could never serve: a refresh, a second tab,
// and a board loaded cold while an agent was already parked.
//
// Both the run-detail page (one run) and the board (many runs at once) surface
// prompts, so the queue/TTL/fetch/resolve logic lives here once —
// `usePermissionQueues` builds on it; `useRunDetail` consumes that hook filtered
// to its single run. The UI lives in components/permissions/PermissionPrompt.

import { readError } from './api'

// PendingPermission is one in-flight tool-approval prompt, as the pending-set
// endpoint returns it. tool_call_id is the tool_use id of the call being gated
// — the same id the transcript carries — so parallel tool calls in one
// assistant message each get their own independently answerable prompt.
//
// timeout_ms is the deadline REMAINING (relative), not the window the prompt
// was granted, so a prompt reconstructed after a refresh gets the time it
// actually has left. 0 / absent means the server stored no expiry and the
// client falls back to its own default.
//
// title is the prompt copy the SDK already rendered, absent when it rendered
// none — what the user approves is the real input, never a restatement of it
// (see PermissionPrompt).
export interface PendingPermission {
  tool_call_id: string
  tool_name: string
  input: Record<string, unknown>
  timeout_ms?: number
  title?: string
  requested_at?: string
}

// PermissionDecisionInput is the user's answer to a prompt — the body the
// resolve endpoint accepts (message is an optional deny reason / note).
export interface PermissionDecisionInput {
  behavior: 'allow' | 'deny'
  message?: string
}

// Fallback client-side TTL for a payload that carries no timeout_ms,
// mirroring the backend's default permTimeout() (= 5m/2). The backend denies
// an unanswered prompt at its bound but emits no "expired" event, so without
// a timer a timed-out prompt would linger in the dock forever.
export const PERMISSION_TTL_FALLBACK_MS = 150_000

// How long the prompt outlives the server deadline before the client drops
// it. Deliberately AFTER the deadline, not racing it: the broker guarantees a
// late answer 404s (never a dropped 200), and the resolver drops the prompt
// on 404 — so a user clicking into the grace window gets a clean dismiss
// instead of a prompt that vanished from under the cursor at t-0.
export const PERMISSION_TTL_GRACE_MS = 5_000

// ttlForPrompt derives the client dismiss TTL for one prompt: its server-side
// deadline (when present) plus the grace, else the fallback plus the grace.
export function ttlForPrompt(prompt: PendingPermission): number {
  const base =
    typeof prompt.timeout_ms === 'number' && prompt.timeout_ms > 0
      ? prompt.timeout_ms
      : PERMISSION_TTL_FALLBACK_MS
  return base + PERMISSION_TTL_GRACE_MS
}

// fetchPendingPermissions reads a run's currently-open prompts. Pending is
// derived server-side against the run's active claim, so a prompt left behind
// by a process that no longer exists is already absent here — the client never
// has to reason about whether a parked agent is still parked.
//
// null means "couldn't ask" (non-2xx, network failure, or a body that isn't the
// expected array) and is deliberately NOT the same value as []. An empty array
// is the server stating this run has nothing outstanding, which the caller acts
// on by clearing the queue; treating a failed read the same way would blank a
// live prompt off every surface on one transient 500 and leave it invisible
// until some later frame happened to arrive — reintroducing exactly the
// reachability hole this endpoint exists to close.
export async function fetchPendingPermissions(runID: string): Promise<PendingPermission[] | null> {
  try {
    const res = await fetch(`/api/agent/conversations/${runID}/permissions`)
    if (!res.ok) return null
    const data = await res.json()
    if (!Array.isArray(data)) return null
    // Normalize at the boundary rather than trusting the cast: `input` is what
    // the user is actually approving, and PermissionPrompt indexes it directly,
    // so a payload that somehow omits it would throw during render instead of
    // degrading. An empty object renders as a prompt with no summary — visibly
    // thin, but answerable and not a crash.
    return (data as PendingPermission[]).map((p) => ({ ...p, input: p.input ?? {} }))
  } catch {
    return null
  }
}

// PermissionResolveResult is the discriminated outcome of a resolve POST.
// 'resolved' (200) and 'gone' (404 — already answered / timed out / never
// existed) are both definitive: the caller drops the prompt from its queue. An
// 'error' (5xx / network) is transient: the prompt stays up so the user can
// retry, and `message` is ready for a toast.
export type PermissionResolveResult =
  | { kind: 'resolved' }
  | { kind: 'gone' }
  | { kind: 'error'; message: string }

// resolvePermission answers a pending prompt by POSTing the decision to the
// run-scoped endpoint. A 200 (resolved) or 404 (already answered / timed out)
// is definitive; anything else is a transient error the caller surfaces.
export async function resolvePermission(
  runID: string,
  toolCallID: string,
  decision: PermissionDecisionInput,
): Promise<PermissionResolveResult> {
  try {
    const res = await fetch(`/api/agent/conversations/${runID}/permissions/${toolCallID}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(decision),
    })
    if (res.ok) return { kind: 'resolved' }
    if (res.status === 404) return { kind: 'gone' }
    return { kind: 'error', message: await readError(res, 'Failed to answer permission request') }
  } catch (err) {
    return {
      kind: 'error',
      message: `Failed to answer permission request: ${(err as Error).message}`,
    }
  }
}
