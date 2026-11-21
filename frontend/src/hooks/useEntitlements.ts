import { useEffect, useState } from 'react'
import { apiJSON } from '../lib/apiClient'
import { useApiOrgId } from './useApiOrgId'

// Shared store for GET /api/orgs/{org_id}/entitlements — the gated EE features
// licensed for an org. One module-level cache per org means every EE surface
// showing that org shares one round-trip, and the fetch fires lazily on the
// first subscriber for it.
//
// This is a deliberately thin mirror of the probe. The entitlement check is
// mode-agnostic server-side — a feature's EE-ness never depends on local vs
// multi — so the hook carries no local/multi logic beyond which org it reads
// (useApiOrgId). In any unlicensed build (a local install included) the set is
// empty and has(x) is false for all; that is correct, not a withheld feature,
// because the EE surfaces are cross-team / org-wide lenses whose
// within-boundary core counterparts don't gate on entitlements at all. EE
// surfaces gate on `loaded && has('<feature>')`.
//
// The answer is the org's, and the cache is keyed by it, so an org switch
// reads the new org's set and a slow answer for the previous org lands in that
// org's entry rather than over the new one.

const cache = new Map<string, Set<string>>()
const inFlight = new Map<string, Promise<void>>()
const listeners = new Set<() => void>()

function notify() {
  for (const l of listeners) l()
}

function load(orgId: string): Promise<void> {
  const pending = inFlight.get(orgId)
  if (pending) return pending
  const read = apiJSON<{ features?: string[] }>(
    `/api/orgs/${encodeURIComponent(orgId)}/entitlements`,
  )
    .then((data) => {
      cache.set(orgId, new Set(data.features ?? []))
    })
    .catch(() => {
      // Fail closed: keep the feature set empty but resolve the org, so EE
      // surfaces resolve (hidden) rather than flickering or hanging unresolved.
      // The probe is a core route that always answers in a healthy deployment;
      // a transient failure must not strand the gate.
      cache.set(orgId, new Set())
    })
    .finally(() => {
      inFlight.delete(orgId)
      notify()
    })
  inFlight.set(orgId, read)
  return read
}

// Feature ids gated by entitlements — mirrors the wire values of
// internal/entitlements Feature (they must match the strings the probe returns).
// EE surfaces gate with has(FeatureGovernance) / has(FeatureSSO) rather than a
// bare string literal, so a typo is a compile/lint error and every gate is
// grep-able.
export const FeatureGovernance = 'governance' as const
export const FeatureSSO = 'sso' as const
export const FeatureSlack = 'slack' as const
// FeatureFleet gates the sandbox-fleet administration console. Deployment-
// scoped server-side (resolves via entitlements.Active()); on self-host the
// entitlements probe reports it identically for every org, so the FE gate
// `has(FeatureFleet)` is correct here. The nav item additionally requires the
// operator flag from /api/me — the two compose exactly as the backend gate does.
export const FeatureFleet = 'fleet' as const

export interface Entitlements {
  /** Whether `feature` is licensed for the viewer. False until the probe
   *  resolves, and false for any feature the deployment / active org isn't
   *  licensed for — including unlicensed and local builds, where the EE set is
   *  empty (the within-boundary core surfaces don't gate on this). Pass a
   *  Fe* constant (e.g. FeatureGovernance), not a bare string. */
  has: (feature: string) => boolean
  /** True once the org's entitlements read has resolved (success or fail-closed).
   *  Gate EE surfaces on `loaded && has('x')` so they don't flash before the
   *  probe answers. */
  loaded: boolean
}

// useEntitlements exposes the available-feature check to EE frontend surfaces.
// The returned `has` reads the shared cache, so N consumers of one org trigger
// exactly one entitlements fetch.
export function useEntitlements(): Entitlements {
  const orgId = useApiOrgId()
  const [, forceRender] = useState(0)

  useEffect(() => {
    const cb = () => forceRender((n) => n + 1)
    listeners.add(cb)
    // Another consumer may have filled the entry between this render and the
    // effect, with no notify left to deliver; re-render to read it.
    if (orgId) {
      if (cache.has(orgId)) cb()
      else void load(orgId)
    }
    return () => {
      listeners.delete(cb)
    }
  }, [orgId])

  const features = orgId ? cache.get(orgId) : undefined
  return {
    has: (feature: string) => features?.has(feature) ?? false,
    loaded: features !== undefined,
  }
}

// resetEntitlementsForTest clears the module-level cache so each test starts
// from the pre-fetch state. Test-only.
export function resetEntitlementsForTest(): void {
  cache.clear()
  inFlight.clear()
  listeners.clear()
}
