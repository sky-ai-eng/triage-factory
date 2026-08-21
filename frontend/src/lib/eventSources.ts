import type { EventSourceState } from '../hooks/useEventSources'

/** sourceKindOf returns the source an event type belongs to — the segment
 *  before the first ':' ("jira" for "jira:issue:assigned"). Mirrors
 *  eventsource.KindOf server-side. */
export function sourceKindOf(eventType: string): string {
  const i = eventType.indexOf(':')
  return i < 0 ? eventType : eventType.slice(0, i)
}

// Display names for the source kinds the availability read can answer with.
// A kind with no entry renders as its own key rather than being dropped: a
// source this build has never heard of is still one the server told us about,
// and a raw lowercase name is a better answer than a missing group.
const labels: Record<string, string> = {
  github: 'GitHub',
  jira: 'Jira',
  slack: 'Slack',
  linear: 'Linear',
  schedule: 'Schedule',
}

export function sourceLabel(kind: string): string {
  return labels[kind] ?? kind
}

/**
 * sourceUnavailableReason renders why events of this source cannot reach the
 * org, or null when they can. An undefined state is an unresolved read or a
 * source outside this deployment's vocabulary — neither is a reason, so both
 * answer null rather than a guess.
 *
 * The three sentences differ because the remedies do: connecting an
 * integration is the reader's to do (or to ask an org admin for), a licence is
 * a purchasing decision, and an unbuilt source is nobody's to fix.
 */
export function sourceUnavailableReason(
  kind: string,
  state: EventSourceState | undefined,
): string | null {
  switch (state) {
    case 'unconfigured':
      return `${sourceLabel(kind)} is not connected for this workspace.`
    case 'unlicensed':
      return `${sourceLabel(kind)} is not enabled for this workspace.`
    case 'wip':
      return `${sourceLabel(kind)} events aren’t supported yet.`
    default:
      return null
  }
}

/**
 * sourceCannotFireNote is the sentence to put on a handler the server has
 * already told us cannot fire (`source_available: false`). It always returns
 * prose: the row's own flag is the authority on WHETHER, and the availability
 * read is only consulted for WHY — so a row that renders before that read
 * resolves still says something rather than going quietly inert.
 */
export function sourceCannotFireNote(kind: string, state: EventSourceState | undefined): string {
  return sourceUnavailableReason(kind, state) ?? `${sourceLabel(kind)} can’t reach this workspace.`
}
