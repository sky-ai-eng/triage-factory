import { useEffect, useState } from 'react'
import { apiJSON } from '../lib/apiClient'
import { useApiOrgId } from './useApiOrgId'

/** One offered model, as GET /api/orgs/{org}/models returns it. The read
 *  carries more (prices, context window, availability); this is the slice the
 *  pickers need — the key is what gets stored, the display name is what a
 *  person reads, and `enabled` is the org's own say over what may be picked.
 *
 *  `provider` is OPTIONAL, and its absence is data rather than a gap: the read
 *  serves whichever execution vocabulary the deployment speaks, and a model id
 *  names an access path only in one of them. Where the harness resolves the
 *  path from the credential instead, no provider is published — and a client
 *  must not invent one, group by one, or ask the user to pick one. The same
 *  convention governs the fields this slice does not take (prices, context
 *  window, availability): present means TF can assert it, absent means nothing
 *  is claimed. */
export interface ModelCatalogEntry {
  key: string
  display_name: string
  provider?: string
  enabled: boolean
}

// One org-scoped read shared by every picker on the page: the team default, the
// prompt drawer's per-step override, the setup step. A module-level cache per
// org is what keeps four mounted pickers from being four round-trips.
const cache = new Map<string, ModelCatalogEntry[]>()
const inFlight = new Map<string, Promise<void>>()
const listeners = new Set<() => void>()

// Display names are a fact about the build, not about the org — the same key
// renders the same name for everyone, and only `enabled` varies per tenant. So
// they accumulate here across orgs, which is what lets a collapsed summary
// name a stored model from outside a component that could hold the hook.
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

export interface UseModelCatalog {
  /** The models this org may pick from, in display order. Empty until the read
   *  resolves. */
  models: ModelCatalogEntry[]
  /** True once the read has resolved. */
  loaded: boolean
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

  const models = orgId ? cache.get(orgId) : undefined
  return { models: models?.filter((m) => m.enabled) ?? [], loaded: models !== undefined }
}

/** resetModelCatalogForTest clears the module-level cache so each test starts
 *  from the pre-fetch state. Test-only. */
export function resetModelCatalogForTest(): void {
  cache.clear()
  inFlight.clear()
  displayNames.clear()
  listeners.clear()
}
