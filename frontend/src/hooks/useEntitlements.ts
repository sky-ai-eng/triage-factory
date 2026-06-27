import { useSyncExternalStore } from 'react'

// Shared store for GET /api/entitlements — the EE features available to the
// viewer right now. Mirrors the module-store + useSyncExternalStore shape (and
// the org-switch invalidation) of useTeams: one module-level cache means every
// EE surface across the app shares one round-trip, and the fetch fires lazily
// on the first subscriber.
//
// The licensing POLICY lives entirely server-side (see entitlements.Available),
// so this hook is a deliberately thin mirror of whatever the probe returns:
//   - Local mode is fully source-available and free → the probe reports every
//     feature, so EE surfaces render and buying EE in local changes nothing.
//   - Multi mode reports the licensed subset for the viewer's active org — a
//     deployment-wide license today (self-host EE), per-org Stripe state later
//     (the SaaS tenant model).
//
// Because the multi-mode answer is scoped to the ACTIVE ORG, switching orgs
// must drop this cache (org A may have governance, org B may not) — exactly
// like useTeams. OrgPicker calls invalidateEntitlements() on switch. Today a
// deployment license answers the same for every org so the refetch is a no-op
// in effect; the seam is in place for when per-org entitlements land.

type State = {
  features: Set<string>
  loaded: boolean
}

let state: State = { features: new Set(), loaded: false }
const listeners = new Set<() => void>()
let inFlight: Promise<void> | null = null
// Bumped whenever the cache is replaced wholesale (org-switch invalidate). A
// fetch captures the generation at dispatch and discards its result if a newer
// invalidation has since bumped it — so a slow request for the previous org
// can't repopulate the cache after the active org changed (which would surface
// the wrong org's feature set). Same guard as useTeams.
let generation = 0

function setState(next: State) {
  state = next
  for (const l of listeners) l()
}

function load(): Promise<void> {
  if (inFlight) return inFlight
  const gen = generation
  inFlight = fetch('/api/entitlements')
    .then(async (r) => {
      if (!r.ok) throw new Error(`entitlements probe failed (${r.status})`)
      const data = (await r.json()) as { features?: string[] }
      if (gen !== generation) return // superseded — a newer invalidation won
      setState({ features: new Set(data.features ?? []), loaded: true })
    })
    .catch(() => {
      if (gen !== generation) return
      // Fail closed: keep the feature set empty but flip `loaded` true, so EE
      // surfaces resolve (hidden) rather than flickering or hanging unresolved.
      // The probe is a core route that always answers in a healthy deployment;
      // a transient failure must not strand the gate.
      setState({ features: new Set(), loaded: true })
    })
    .finally(() => {
      // Only clear if it's still ours — an invalidate may have nulled it and a
      // newer load() installed its own promise.
      if (gen === generation) inFlight = null
    })
  return inFlight
}

function subscribe(cb: () => void): () => void {
  listeners.add(cb)
  // Lazy first load: the first subscriber kicks the single fetch.
  if (!state.loaded && !inFlight) void load()
  return () => {
    listeners.delete(cb)
  }
}

function getSnapshot(): State {
  return state
}

/** Drop the cached entitlements and reload for any mounted subscriber. Call on
 *  active-org switch — entitlements are scoped to the active org in multi mode,
 *  so the previous org's set must not linger. OrgPicker does this alongside
 *  invalidateTeams(). Bumps the generation first so a still-pending prior-org
 *  request can't repopulate the cache after the reset. */
export function invalidateEntitlements(): void {
  generation++
  inFlight = null
  state = { features: new Set(), loaded: false }
  // Reload for any mounted subscriber — load() re-notifies via setState. With
  // no subscribers the reset above already flips loaded=false, so the next
  // subscribe() kicks a fresh load.
  if (listeners.size > 0) void load()
}

export interface Entitlements {
  /** Whether `feature` is available to the viewer. False for every feature
   *  until the probe resolves; in multi mode, false for features the active
   *  org isn't licensed for. Always true in local mode (free / fully featured). */
  has: (feature: string) => boolean
  /** True once GET /api/entitlements has resolved (success or fail-closed).
   *  Gate EE surfaces on `loaded && has('x')` so they don't flash before the
   *  probe answers. */
  loaded: boolean
}

// useEntitlements exposes the available-feature check to EE frontend surfaces.
// The returned `has` reads the shared cache, so N consumers across the app
// trigger exactly one /api/entitlements fetch (until an org switch invalidates).
export function useEntitlements(): Entitlements {
  const snap = useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
  return {
    has: (feature: string) => snap.features.has(feature),
    loaded: snap.loaded,
  }
}

// resetEntitlementsForTest clears the module-level cache so each test starts
// from the pre-fetch state. Test-only — production invalidates per active-org
// switch via invalidateEntitlements, never wholesale.
export function resetEntitlementsForTest(): void {
  generation++
  state = { features: new Set(), loaded: false }
  inFlight = null
  listeners.clear()
}
