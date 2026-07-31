// Shared core for delegated-run tool-permission prompts (the `canUseTool`
// round-trip). When a run hits an off-allowlist tool the backend broadcasts a
// `permission_request` WS event and parks the agent's turn until the user
// answers via POST .../permissions/{toolCallID} or a server-side timeout denies
// it. Both the run-detail page (one run) and the board (many runs at once)
// surface that prompt, so the queue/TTL/resolve logic lives here once —
// `usePermissionQueues` builds on it; `useRunDetail` consumes that hook filtered
// to its single run. The UI lives in components/permissions/PermissionPrompt.

import { readError } from './api'

// PendingPermission is one in-flight tool-approval prompt surfaced by a run
// (the `permission_request` WS payload). tool_call_id is the tool_use id of the
// call being gated — the same id the transcript carries — so parallel tool
// calls in one assistant message each get their own independently answerable
// prompt. timeout_ms is the prompt's server-side deadline (relative), used to
// derive the client dismiss TTL; older payloads may lack it.
//
// title / display_name / description are the prompt copy the SDK already
// rendered, absent when it rendered none. Only title is displayed: description
// is a bridge subtitle that can restate the agent's own words for the call, and
// what the user approves must be the real input (see PermissionPrompt).
export interface PendingPermission {
  tool_call_id: string
  tool_name: string
  input: Record<string, unknown>
  timeout_ms?: number
  title?: string
  display_name?: string
  description?: string
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
