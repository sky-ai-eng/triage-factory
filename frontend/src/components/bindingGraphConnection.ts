import type { Connection } from '@xyflow/react'
import type { EventHandlerCreate } from '../lib/eventHandlers'

// Connection planning for BindingGraph. Every connect gesture is resolved
// against blueprint membership (decision 5 — always render/route from
// blueprint_id, never infer boxes from the live edge graph) into one concrete
// plan, or a refusal with a user-facing reason. This is the single place the
// linear-chain invariant (decision 7) lives: a blueprint's entry prompt takes
// any number of events, and every other prompt has one input, its upstream
// step. It lives outside BindingGraph.tsx so it can be tested without breaking
// React Fast Refresh, which wants a component module to export only components.

export type ConnPlan =
  | { ok: false; reason: string }
  // event → a blueprint's entry prompt: create an event handler.
  | { ok: true; kind: 'trigger'; eventType: string; blueprintId: string }
  // tail of one chain → entry of a chain no handler references: atomic merge.
  | { ok: true; kind: 'merge'; host: string; source: string }

export interface Membership {
  // step-0 prompt id per blueprint (the event-edge target / chain entry).
  firstPrompt: Record<string, string>
  // ordered step prompt ids per blueprint.
  steps: Record<string, string[]>
  // prompt id → its owning blueprint (1:1 — prompts are copy-only).
  promptToBlueprint: Record<string, string>
  // blueprint id → the event types of the handlers that reference it.
  triggerEvents: Record<string, string[]>
}

export const EVENT_ALREADY_RUNS_BLUEPRINT = 'This event already runs this blueprint'

// A merge, and a sequence reconnect, which merges its target in, absorbs only
// a blueprint no event handler references.
export const DETACH_BEFORE_MERGE =
  'Detach the downstream blueprint’s event handlers before merging it in'

// eventRunsBlueprint reports whether a handler on eventType already references
// blueprintId. The API takes a second one; the canvas refuses it, because it
// draws one edge per event and blueprint.
export function eventRunsBlueprint(
  triggerEvents: Membership['triggerEvents'],
  eventType: string,
  blueprintId: string,
): boolean {
  return triggerEvents[blueprintId]?.includes(eventType) ?? false
}

export function planConnection(c: Connection, m: Membership): ConnPlan {
  const source = c.source ?? ''
  const target = c.target ?? ''
  if (!target.startsWith('p:')) {
    return { ok: false, reason: 'A connection has to end on a prompt' }
  }
  const targetPid = target.slice(2)
  const targetBp = m.promptToBlueprint[targetPid]
  if (!targetBp) {
    return { ok: false, reason: 'That prompt is not part of a blueprint' }
  }
  // Only a chain's entry (step 0) takes a new input: a mid-chain prompt
  // already has its upstream step edge.
  if (m.firstPrompt[targetBp] !== targetPid) {
    return {
      ok: false,
      reason: 'That prompt already has an input — a step takes a single incoming edge',
    }
  }

  if (source.startsWith('et:')) {
    const eventType = source.slice(3)
    if (eventRunsBlueprint(m.triggerEvents, eventType, targetBp)) {
      return { ok: false, reason: EVENT_ALREADY_RUNS_BLUEPRINT }
    }
    return { ok: true, kind: 'trigger', eventType, blueprintId: targetBp }
  }

  if (source.startsWith('p:')) {
    const srcPid = source.slice(2)
    const srcBp = m.promptToBlueprint[srcPid]
    if (!srcBp) {
      return { ok: false, reason: 'That prompt is not part of a blueprint' }
    }
    if (srcBp === targetBp) {
      return { ok: false, reason: 'A blueprint can’t wire into itself' }
    }
    // A step has a single output: only a chain's tail (last step) has a free
    // outgoing slot to extend onward.
    const srcSteps = m.steps[srcBp] ?? []
    const srcIsTail = srcSteps.length > 0 && srcSteps[srcSteps.length - 1] === srcPid
    if (!srcIsTail) {
      return {
        ok: false,
        reason: 'Only a blueprint’s last step can extend onward — a step has a single output',
      }
    }
    // Merge absorbs the trigger-less downstream chain onto this tail.
    if ((m.triggerEvents[targetBp]?.length ?? 0) > 0) {
      return { ok: false, reason: DETACH_BEFORE_MERGE }
    }
    return { ok: true, kind: 'merge', host: srcBp, source: targetBp }
  }

  return { ok: false, reason: 'Unsupported connection' }
}

// handlerForConnection is the handler an event → entry connection creates: a
// new one, in off, on the event and the blueprint the edge joins. Drawing the
// edge attaches the blueprint; turning the handler on is a choice of its own,
// and the connection never reuses another handler of the event.
export function handlerForConnection(
  plan: { eventType: string; blueprintId: string },
  teamId: string,
): EventHandlerCreate {
  return {
    team_id: teamId,
    event_type: plan.eventType,
    blueprint_id: plan.blueprintId,
    mode: 'off',
  }
}
