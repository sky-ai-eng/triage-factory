import { useSyncExternalStore } from 'react'

// Shared store for GET /api/entitlements — the gated EE features licensed in
// this deployment (for the viewer's active org in multi mode). Mirrors the
// module-store + useSyncExternalStore + org-switch-invalidation shape of
// useTeams: one module-level cache means every EE surface shares one
// round-trip, and the fetch fires lazily on the first subscriber.
//
// This is a deliberately thin mirror of the probe. The entitlement check is
// mode-agnostic server-side — a feature's EE-ness never depends on local vs
// multi — so the hook carries no local/multi logic. In any unlicensed build
// (a local install included) the set is empty and has(x) is false for all; that
// is correct, not a withheld feature, because the EE surfaces are cross-team /
// org-wide lenses whose within-boundary core counterparts don't gate on
// entitlements at all. EE surfaces gate on `loaded && has('<feature>')`.
//
// The multi-mode answer is scoped to the ACTIVE ORG, so switching orgs drops
// this cache (org A may license governance, org B may not) — exactly like
// useTeams. OrgPicker calls invalidateEntitlements() on switch. Today a
// deployment license answers the same for every org so the refetch is a no-op
// in effect; the seam is in place for when per-org (Stripe) entitlements land.

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

// Feature ids gated by entitlements — mirrors the wire values of
// internal/entitlements Feature (they must match the strings the probe returns).
// EE surfaces gate with has(FeatureGovernance) rather than a bare 'governance'
// literal, so a typo is a compile/lint error and every gate is grep-able. Only
// governance is here: SSO has no FE entitlement consumer (its absence is a route
// 404 via runmode, not a has() gate), so a constant for it would imply a
// consumer that doesn't exist.
export const FeatureGovernance = 'governance' as const

export interface Entitlements {
  /** Whether `feature` is licensed for the viewer. False until the probe
   *  resolves, and false for any feature the deployment / active org isn't
   *  licensed for — including unlicensed and local builds, where the EE set is
   *  empty (the within-boundary core surfaces don't gate on this). Pass a
   *  Fe* constant (e.g. FeatureGovernance), not a bare string. */
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
