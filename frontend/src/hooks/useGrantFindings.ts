// useGrantFindings loads the two grant findings for the Settings "GitHub
// access" panel — repositories the App reaches that nobody tracks, and
// repositories a team tracks that the App cannot reach — through the one
// paging hook every list surface uses. Both are computed server-side from the
// reachable-repo mirror, so loading them asks GitHub nothing.

import { useEffect } from 'react'
import { usePagedList, type PagedList } from './usePagedList'
import {
  reachWithoutPurposeListPath,
  scopeDriftListPath,
  type ReachWithoutPurposeItem,
  type ScopeDriftItem,
} from '../lib/githubApp'

// The finding routes 404 for a workspace that holds no grant (a PAT one), and
// a role that lapsed mid-session earns a 403; neither is a load failure. A
// module constant, because the hook memoizes on it.
const findingsListOptions = { emptyOnStatus: [403, 404] }

export interface GrantFindings {
  reach: PagedList<ReachWithoutPurposeItem>
  drift: PagedList<ScopeDriftItem>
}

// useGrantFindings loads both findings when `enabled`, and again whenever
// `bindingsKey` changes — the bound set moved (an account disconnected), so the
// grant the findings describe moved with it. Disabled, it loads nothing: a PAT
// workspace has no grant to have findings about, and the empty lists it holds
// are never rendered.
export function useGrantFindings(
  orgId: string | null,
  enabled: boolean,
  bindingsKey: string,
): GrantFindings {
  const reach = usePagedList<ReachWithoutPurposeItem>(
    reachWithoutPurposeListPath(orgId ?? ''),
    'Could not load the repositories nobody tracks.',
    findingsListOptions,
  )
  const drift = usePagedList<ScopeDriftItem>(
    scopeDriftListPath(orgId ?? ''),
    'Could not load the repositories outside the grant.',
    findingsListOptions,
  )
  const loadReach = reach.load
  const loadDrift = drift.load
  useEffect(() => {
    if (!orgId || !enabled) return
    void loadReach({})
    void loadDrift({})
  }, [orgId, enabled, bindingsKey, loadReach, loadDrift])
  return { reach, drift }
}
