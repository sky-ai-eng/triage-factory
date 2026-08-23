// Team archive/restore lifecycle API (TFAC-448). Archiving a team soft-deletes
// it, force-stops every in-flight delegation, and blocks further writes — no
// "let it finish" branch. Restore flips it back (killed runs do NOT
// resurrect). Org-admin only, multi-mode only.

import { apiFetch, apiJSON, apiList, httpErrorMessage } from './apiClient'

// asError keeps a raw HttpError (message = the whole response body) out of the
// confirm modal, which renders `err.message` directly.
function asError(e: unknown, fallback: string): Error {
  return new Error(httpErrorMessage(e, fallback))
}

// ArchivePreview is GET /api/teams/{id}/archive/preview — the live-work counts
// the confirm modal surfaces before the destructive action.
export interface ArchivePreview {
  team_id: string
  name: string
  archived: boolean
  active_runs: number
}

// ArchiveResult is the POST /api/teams/{id}/archive response — the counts of
// work the cascade actually stopped.
export interface ArchiveResult {
  cancelled_runs: number
}

// ArchivedTeam is one row of POST /api/teams/archived/list — the org-admin restore
// surface.
export interface ArchivedTeam {
  id: string
  name: string
  slug: string
  archived_at: string
}

export async function fetchArchivePreview(teamId: string): Promise<ArchivePreview> {
  try {
    return await apiJSON<ArchivePreview>(`/api/teams/${encodeURIComponent(teamId)}/archive/preview`)
  } catch (e) {
    throw asError(e, 'Could not load the archive preview.')
  }
}

export async function archiveTeam(teamId: string): Promise<ArchiveResult> {
  try {
    return await apiJSON<ArchiveResult>(`/api/teams/${encodeURIComponent(teamId)}/archive`, {
      method: 'POST',
    })
  } catch (e) {
    throw asError(e, 'Could not archive the team.')
  }
}

export async function restoreTeam(teamId: string): Promise<void> {
  try {
    await apiFetch(`/api/teams/${encodeURIComponent(teamId)}/restore`, { method: 'POST' })
  } catch (e) {
    throw asError(e, 'Could not restore the team.')
  }
}

export async function fetchArchivedTeams(): Promise<ArchivedTeam[]> {
  try {
    // page_size at the route max: the restore surface renders the whole
    // archived set, which is bounded by how many teams an org ever had.
    const page = await apiList<ArchivedTeam>('/api/teams/archived/list', { page_size: 200 })
    return page.items
  } catch (e) {
    throw asError(e, 'Could not load the archived teams.')
  }
}
