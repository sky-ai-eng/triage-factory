import { useCallback, useEffect, useState } from 'react'
import { apiJSON } from '../lib/apiClient'
import { setWsSourcesInvalidator } from './useWebSocket'
import { useApiOrgId } from './useApiOrgId'

/** The closed vocabulary GET /api/orgs/{org}/sources answers with. Each value
 *  names a different thing the reader can do about it: `unconfigured` is
 *  theirs to fix, `disabled` is an org admin's to undo, `unlicensed` is a
 *  purchasing decision, `wip` is nobody's. */
export type EventSourceState = 'available' | 'unconfigured' | 'disabled' | 'unlicensed' | 'wip'

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
// Bumped PER ORG whenever that org's cached answer is dropped. A read captures
// its org's generation at dispatch and discards its answer if an invalidation
// has since bumped it, so a request already in flight when a credential
// changed cannot repopulate the cache with the answer from before the change.
//
// Per org rather than one global counter because invalidation is per org: a
// bump is "this org's answer is stale", and spending it globally would discard
// a perfectly good in-flight read for an org nothing happened to.
const generations = new Map<string, number>()

function generationOf(orgId: string): number {
  return generations.get(orgId) ?? 0
}

function notify() {
  for (const l of listeners) l()
}

function load(orgId: string): Promise<void> {
  const pending = inFlight.get(orgId)
  if (pending) return pending
  const gen = generationOf(orgId)
  const read: Promise<void> = apiJSON<{ sources?: EventSourceAvailability[] }>(
    `/api/orgs/${encodeURIComponent(orgId)}/sources`,
  )
    .then((data) => {
      if (gen !== generationOf(orgId)) return // superseded — a newer invalidation won
      cache.set(orgId, data.sources ?? [])
      notify()
    })
    .catch(() => {
      // Leave the org UNRESOLVED rather than caching an empty vocabulary. An
      // empty list would read as "no source is available" and go on to mark
      // every handler inert and empty the event picker — a far worse answer
      // than making no claim, which is what an unresolved read does below.
    })
  const tracked: Promise<void> = read.finally(() => {
    // Retire only OUR OWN entry. An invalidation can start a newer read for
    // this org while this one is still open, and a blind delete here would
    // retire that one — leaving it running but untracked, so the next caller
    // starts a third duplicate read of the same org.
    if (inFlight.get(orgId) === tracked) inFlight.delete(orgId)
  })
  inFlight.set(orgId, tracked)
  return tracked
}

/** Drop the cached availability and refetch for any mounted subscriber. Call
 *  after a change that can move a source — binding or unbinding a credential,
 *  connecting a workspace, an org admin pausing or resuming one. Pass an orgId
 *  to invalidate just that org; omit it to invalidate every org this page has
 *  read.
 *
 *  The `sources_updated` websocket ping lands here too: the server sends it
 *  payload-free and org-scoped, and the refetch through the REST read is what
 *  carries the scoping. */
export function invalidateEventSources(orgId?: string): void {
  const targets = orgId ? [orgId] : [...cache.keys(), ...inFlight.keys()]
  for (const id of new Set(targets)) {
    // Bump before dropping: a read still in flight for this org has captured
    // the old generation and must not write its pre-change answer into the
    // cache we are about to empty.
    generations.set(id, generationOf(id) + 1)
    inFlight.delete(id)
    cache.delete(id)
  }
  // Mounted consumers refetch from their own subscription (below) rather than
  // this function knowing which orgs are on screen.
  notify()
}

// Answer the server's `sources_updated` ping by dropping every cached org.
// Registered at module load, alongside the cache it clears: a page that never
// read availability never loaded this module, and has nothing stale to drop.
// The ping carries no payload — the REST refetch is what carries the scoping.
setWsSourcesInvalidator(() => invalidateEventSources())

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
  generations.clear()
  listeners.clear()
}
