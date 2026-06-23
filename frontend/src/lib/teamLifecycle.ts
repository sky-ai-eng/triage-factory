// Team archive/restore lifecycle API (TFAC-448). Archiving a team soft-deletes
// it, force-stops every in-flight delegation + curator session, and blocks
// further writes — no "let it finish" branch. Restore flips it back (killed runs
// do NOT resurrect). Org-admin only, multi-mode only.

import { readError } from './api'

// ArchivePreview is GET /api/teams/{id}/archive/preview — the live-work counts
// the confirm modal surfaces before the destructive action.
export interface ArchivePreview {
  team_id: string
  name: string
  archived: boolean
  active_runs: number
  active_curator_sessions: number
}

// ArchiveResult is the POST /api/teams/{id}/archive response — the counts of
// work the cascade actually stopped.
export interface ArchiveResult {
  cancelled_runs: number
  cancelled_curator_sessions: number
}

// ArchivedTeam is one entry of GET /api/teams/archived — the org-admin restore
// surface.
export interface ArchivedTeam {
  id: string
  name: string
  slug: string
  archived_at: string
}

export async function fetchArchivePreview(teamId: string): Promise<ArchivePreview> {
  const res = await fetch(`/api/teams/${encodeURIComponent(teamId)}/archive/preview`, {
    credentials: 'include',
  })
  if (!res.ok) throw new Error(await readError(res, 'Failed to load archive preview'))
  return (await res.json()) as ArchivePreview
}

export async function archiveTeam(teamId: string): Promise<ArchiveResult> {
  const res = await fetch(`/api/teams/${encodeURIComponent(teamId)}/archive`, {
    method: 'POST',
    credentials: 'include',
  })
  if (!res.ok) throw new Error(await readError(res, 'Failed to archive team'))
  return (await res.json()) as ArchiveResult
}

export async function restoreTeam(teamId: string): Promise<void> {
  const res = await fetch(`/api/teams/${encodeURIComponent(teamId)}/restore`, {
    method: 'POST',
    credentials: 'include',
  })
  if (!res.ok) throw new Error(await readError(res, 'Failed to restore team'))
}

export async function fetchArchivedTeams(): Promise<ArchivedTeam[]> {
  const res = await fetch('/api/teams/archived', { credentials: 'include' })
  if (!res.ok) throw new Error(await readError(res, 'Failed to load archived teams'))
  const data = (await res.json()) as { teams?: ArchivedTeam[] }
  return data.teams ?? []
}
