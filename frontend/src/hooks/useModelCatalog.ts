import { useEffect, useState } from 'react'
import { apiJSON } from '../lib/apiClient'
import type { ModelCatalogEntry } from '../lib/models'
import { useApiOrgId } from './useApiOrgId'

// One org-scoped read shared by every picker on the page: the team default, the
// prompt drawer's per-step override, the setup step, the org availability list.
// A module-level cache per org is what keeps five mounted pickers from being
// five round-trips.
//
// The whole row is cached, not a slice of it: availability, prices and the
// caching fact are what the picker renders beside a name, and a second narrower
// read would be a second answer about the same model.
const cache = new Map<string, ModelCatalogEntry[]>()
const inFlight = new Map<string, Promise<void>>()
const listeners = new Set<() => void>()

// Display names are a fact about the build, not about the org — the same key
// renders the same name for everyone, and only `enabled` and the availability
// triple vary per tenant. So they accumulate here across orgs, which is what
// lets a collapsed summary name a stored model from outside a component that
// could hold the hook.
const displayNames = new Map<string, string>()

function notify() {
  for (const l of listeners) l()
}

function load(orgId: string): Promise<void> {
  const pending = inFlight.get(orgId)
  if (pending) return pending
  const read: Promise<void> = apiJSON<{ items?: ModelCatalogEntry[] }>(
    `/api/orgs/${encodeURIComponent(orgId)}/models`,
  )
    .then((data) => {
      const items = data.items ?? []
      cache.set(orgId, items)
      for (const m of items) displayNames.set(m.key, m.display_name)
      notify()
    })
    .catch(() => {
      // Leave the org unresolved rather than caching an empty catalog. An empty
      // list reads as "this deployment offers no models", which would empty
      // every picker and make a save look impossible; an unresolved read makes
      // no claim and the picker falls back to naming the stored key.
    })
  const tracked: Promise<void> = read.finally(() => {
    // Retire only our own entry, so a newer read for the same org started
    // meanwhile keeps its tracking.
    if (inFlight.get(orgId) === tracked) inFlight.delete(orgId)
  })
  inFlight.set(orgId, tracked)
  return tracked
}

/** refreshModelCatalog re-reads one org's catalog and republishes it to every
 *  mounted picker. Called after a test concludes: the badge a person is looking
 *  at is a stored verdict, and the write that changed it happened on the server,
 *  so nothing local can update it correctly. */
export function refreshModelCatalog(orgId: string): Promise<void> {
  // Drop the in-flight entry first so this genuinely re-reads rather than
  // joining a read that was issued before the test wrote its row.
  inFlight.delete(orgId)
  return load(orgId)
}

/** modelDisplayName renders a stored model key the way a picker would, falling
 *  back to the key itself for anything this build has not named — a model the
 *  catalog dropped, or a read that has not landed yet. Deliberately callable
 *  outside a component: the wizard's collapsed summaries are plain functions of
 *  the wizard state and have nowhere to hold a hook.
 *
 *  A BLANK KEY IS NOT A MODEL — it is the absence of one — and it comes back
 *  blank rather than as some word this module picked. What "unset" reads as
 *  differs per surface (a prompt inherits the team default, a team setting is
 *  simply not set, a wizard step has not been chosen yet), so each caller says
 *  it; none may pass a possibly-empty key straight into rendered text. */
export function modelDisplayName(key: string): string {
  return displayNames.get(key) ?? key
}

/** modelCatalogEntry returns one org's cached row for a model key, or undefined
 *  when the read has not landed or the build no longer offers it.
 *
 *  Callable outside a component, like modelDisplayName and for the same reason:
 *  a wizard step's persist is a plain async function with nowhere to hold a
 *  hook, and it is one of the places that has to know whether saving this model
 *  needs a verdict first.
 *
 *  Undefined is "no claim", never "no gate": a caller that cannot resolve a row
 *  proceeds, because the save it is gating is refused server-side anyway if the
 *  model is one this org cannot run. */
export function modelCatalogEntry(
  orgId: string | null,
  key: string,
): ModelCatalogEntry | undefined {
  return modelCatalogRows(orgId).find((m) => m.key === key)
}

/** modelCatalogRows is one org's whole cached catalog, or empty before the read
 *  lands. The non-component reader of the same cache the hook serves — what the
 *  post-connect sweep counts its candidates from, having just refreshed it. */
export function modelCatalogRows(orgId: string | null): ModelCatalogEntry[] {
  if (!orgId) return []
  return cache.get(orgId) ?? []
}

export interface UseModelCatalog {
  /** The models this org may pick from, in display order: the org's enable-set,
   *  which is what a picker offers. Empty until the read resolves. */
  models: ModelCatalogEntry[]
  /** Every model this deployment offers, enable-set or not. What the org's
   *  availability list shows: availability is org truth about what the
   *  credentials can do, and a model an admin has not enabled is still a model
   *  they may want to test before enabling it. */
  all: ModelCatalogEntry[]
  /** True once the read has resolved. */
  loaded: boolean
  /** Re-read this org's catalog — after a test, or after a credential bind. */
  refresh: () => Promise<void>
}

/**
 * useModelCatalog reads the models the active org can select.
 *
 * Every model picker in the product draws from this one read rather than a
 * hardcoded list: the catalog is the build's own vocabulary, the org's
 * enable-set narrows it, and a picker offering anything else would be offering
 * a value the save would reject.
 */
export function useModelCatalog(): UseModelCatalog {
  const orgId = useApiOrgId()
  const [, forceRender] = useState(0)

  useEffect(() => {
    const cb = () => forceRender((n) => n + 1)
    listeners.add(cb)
    if (orgId && !cache.has(orgId)) void load(orgId)
    return () => {
      listeners.delete(cb)
    }
  }, [orgId])

  const all = orgId ? cache.get(orgId) : undefined
  return {
    models: all?.filter((m) => m.enabled) ?? [],
    all: all ?? [],
    loaded: all !== undefined,
    refresh: () => (orgId ? refreshModelCatalog(orgId) : Promise.resolve()),
  }
}

/** resetModelCatalogForTest clears the module-level cache so each test starts
 *  from the pre-fetch state. Test-only. */
export function resetModelCatalogForTest(): void {
  cache.clear()
  inFlight.clear()
  displayNames.clear()
  listeners.clear()
}
