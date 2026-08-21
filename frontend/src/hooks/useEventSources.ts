import { useCallback, useEffect, useState } from 'react'
import { apiJSON } from '../lib/apiClient'
import { useApiOrgId } from './useApiOrgId'

/** The closed vocabulary GET /api/orgs/{org}/sources answers with. Each value
 *  names a different thing the reader can do about it: `unconfigured` is
 *  theirs to fix, `unlicensed` is an org admin's, `wip` is nobody's. */
export type EventSourceState = 'available' | 'unconfigured' | 'unlicensed' | 'wip'

export interface EventSourceAvailability {
  kind: string
  state: EventSourceState
}

// The read is one org-scoped singleton, and several surfaces need it at once
// (the rule editor's picker, the binding canvas palette, every handler list).
// A module-level cache per org is what keeps that from being N round-trips and
// N independent mount flickers — the same reason the backend answers all the
// sources in one read instead of making the client compose three probes.
const cache = new Map<string, EventSourceAvailability[]>()
const inFlight = new Map<string, Promise<void>>()
const listeners = new Set<() => void>()
// Bumped whenever the cache is dropped. A read captures the generation at
// dispatch and discards its answer if an invalidation has since bumped it, so
// a request that was already in flight when a credential changed cannot
// repopulate the cache with the answer from before the change.
let generation = 0

function notify() {
  for (const l of listeners) l()
}

function load(orgId: string): Promise<void> {
  const pending = inFlight.get(orgId)
  if (pending) return pending
  const gen = generation
  const p = apiJSON<{ sources?: EventSourceAvailability[] }>(
    `/api/orgs/${encodeURIComponent(orgId)}/sources`,
  )
    .then((data) => {
      if (gen !== generation) return // superseded — a newer invalidation won
      cache.set(orgId, data.sources ?? [])
      notify()
    })
    .catch(() => {
      // Leave the org UNRESOLVED rather than caching an empty vocabulary. An
      // empty list would read as "no source is available" and go on to mark
      // every handler inert and empty the event picker — a far worse answer
      // than making no claim, which is what an unresolved read does below.
    })
    .finally(() => {
      inFlight.delete(orgId)
    })
  inFlight.set(orgId, p)
  return p
}

/** Drop the cached availability and refetch for any mounted subscriber. Call
 *  after a change that can move a source — binding or unbinding a credential,
 *  connecting a workspace. */
export function invalidateEventSources(orgId?: string): void {
  generation++
  inFlight.clear()
  if (orgId) cache.delete(orgId)
  else cache.clear()
  // Mounted consumers refetch from their own subscription (below) rather than
  // this function knowing which orgs are on screen.
  notify()
}

export interface UseEventSources {
  /** Every source this deployment can report on for the org, in wire order.
   *  Empty until the read resolves. Render what is here rather than a fixed
   *  list: local mode omits the sources that cannot exist there. */
  sources: EventSourceAvailability[]
  /** True once the read has resolved. */
  loaded: boolean
  /** The kind's state, or undefined when the deployment does not carry it. */
  stateOf: (kind: string) => EventSourceState | undefined
  /** Whether events of this source can reach the org — i.e. whether a handler
   *  bound to them could ever fire. An unresolved read and a kind outside the
   *  vocabulary both report TRUE: this gate hides and disables things, and
   *  doing that on an answer we do not have is worse than doing it late.
   *  Mirrors Availability.CanProduce server-side. */
  canProduce: (kind: string) => boolean
}

/**
 * useEventSources reads which event sources can reach the active org.
 *
 * The one call answers the whole vocabulary, which is the point: composing it
 * from the org settings read plus the entitlements probe plus the workspace
 * list would be three async probes wide, and a surface that renders on the
 * first to land flashes through states that were never true.
 *
 * TODO(TFAC-880): the team page's GitHub / Jira / Slack source cards still
 * hardcode state="configured" and its band rows are interactive for every
 * source. They are wired to this hook — and rendered inert for a non-available
 * one — once the design-language branch that owns pages/team/TeamSettings.tsx
 * lands; that file does not exist on this branch to edit.
 */
export function useEventSources(): UseEventSources {
  const orgId = useApiOrgId()
  const [, forceRender] = useState(0)

  useEffect(() => {
    const cb = () => {
      // An invalidation drops the cache and notifies; a mounted consumer
      // refills it. That keeps the refetch with whoever is actually rendering.
      if (orgId && !cache.has(orgId) && !inFlight.has(orgId)) void load(orgId)
      forceRender((n) => n + 1)
    }
    listeners.add(cb)
    if (orgId && !cache.has(orgId)) void load(orgId)
    return () => {
      listeners.delete(cb)
    }
  }, [orgId])

  const sources = orgId ? cache.get(orgId) : undefined
  const loaded = sources !== undefined

  const stateOf = useCallback(
    (kind: string) => sources?.find((s) => s.kind === kind)?.state,
    [sources],
  )
  const canProduce = useCallback(
    (kind: string) => {
      const state = sources?.find((s) => s.kind === kind)?.state
      return state === undefined || state === 'available'
    },
    [sources],
  )
  return { sources: sources ?? [], loaded, stateOf, canProduce }
}

/** resetEventSourcesForTest clears the module-level cache so each test starts
 *  from the pre-fetch state. Test-only. */
export function resetEventSourcesForTest(): void {
  cache.clear()
  inFlight.clear()
  listeners.clear()
}
