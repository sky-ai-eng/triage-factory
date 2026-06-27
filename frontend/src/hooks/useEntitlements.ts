import { useSyncExternalStore } from 'react'

// Shared store for GET /api/entitlements — the deployment's licensed Enterprise
// feature set. Mirrors the module-store + useSyncExternalStore shape of
// useTeams: a single module-level cache means every EE surface across the app
// shares one round-trip, and the fetch fires lazily on the first subscriber.
//
// Unlike useTeams this never needs org-switch invalidation: entitlements are
// process-global and deployment-level (one TF_LICENSE verified at boot in
// ee.Install), so the answer is constant for the life of the page. That makes
// the store deliberately simpler — no generation counter, no invalidate.
//
// This is the reusable seam every EE frontend feature consumes: render nothing
// until `loaded && has('<feature>')`. Works in both modes — a local /
// unlicensed build returns features: [], so has(x) is false for all.

type State = {
  features: Set<string>
  loaded: boolean
}

let state: State = { features: new Set(), loaded: false }
const listeners = new Set<() => void>()
let inFlight: Promise<void> | null = null

function setState(next: State) {
  state = next
  for (const l of listeners) l()
}

function load(): Promise<void> {
  if (inFlight) return inFlight
  inFlight = fetch('/api/entitlements')
    .then(async (r) => {
      if (!r.ok) throw new Error(`entitlements probe failed (${r.status})`)
      const data = (await r.json()) as { features?: string[] }
      setState({ features: new Set(data.features ?? []), loaded: true })
    })
    .catch(() => {
      // Fail closed: on any error keep the feature set empty but flip `loaded`
      // true, so EE surfaces stay hidden rather than flickering or hanging
      // unresolved. The probe is a core route that always answers in a healthy
      // deployment; a transient failure must not strand the gate.
      setState({ features: new Set(), loaded: true })
    })
    .finally(() => {
      inFlight = null
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

export interface Entitlements {
  /** Whether `feature` is licensed for this deployment. False for every
   *  feature until the probe resolves, and in any community / unlicensed
   *  build (features: []). */
  has: (feature: string) => boolean
  /** True once GET /api/entitlements has resolved (success or fail-closed).
   *  Gate EE surfaces on `loaded && has('x')` so they don't flash before the
   *  probe answers. */
  loaded: boolean
}

// useEntitlements exposes the licensed-feature check to EE frontend surfaces.
// The returned `has` reads the shared cache, so N consumers across the app
// trigger exactly one /api/entitlements fetch.
export function useEntitlements(): Entitlements {
  const snap = useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
  return {
    has: (feature: string) => snap.features.has(feature),
    loaded: snap.loaded,
  }
}

// resetEntitlementsForTest clears the module-level cache so each test starts
// from the pre-fetch state. Test-only — production never invalidates (the
// deployment's entitlements don't change within a page's lifetime). Mirrors
// the runmode.SetForTest convention of a small, clearly-named test seam.
export function resetEntitlementsForTest(): void {
  state = { features: new Set(), loaded: false }
  inFlight = null
  listeners.clear()
}
