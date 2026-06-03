import { useCallback, useEffect, useState } from 'react'
import { apiFetch, apiJSON } from '../lib/apiClient'
import GitHubTeamChecklist from './GitHubTeamChecklist'
import { keyOf, type GitHubTeamCandidate } from '../lib/githubTeams'

// One stored mapping row: a fully-qualified GitHub team funneled into
// this TF team.
interface GitHubGroup {
  org_login: string
  team_slug: string
}

interface GitHubGroupsResponse {
  groups: GitHubGroup[]
  candidates: GitHubTeamCandidate[]
  role: string
}

interface Props {
  // Team path segment — a UUID or the literal "default" (matches the
  // sibling team-settings calls).
  teamId: string
  canEdit: boolean
}

// GitHubGroupsEditor is the import-and-choose UI for the GitHub-team →
// TF-team review-request mapping (the twin of the Jira project rules
// editor). It owns its own GET/PUT against
// /api/settings/team/{teamId}/github-groups, deliberately separate from
// the big team-settings save-all flow so it can fetch the org's live
// GitHub teams (and trigger the deletion-reconcile floor) on open
// without coupling to the rest of the form.
//
// The list itself — search, the selected/unselected/all filter, the
// bounded scrollable height (the SKY-388 fix; this panel used to grow
// unbounded with the org's team count), the badges, and the stale-mapping
// flag — is the shared GitHubTeamChecklist, the same body the onboarding
// wizard renders. This wrapper owns only the section chrome and the Save
// button.
export default function GitHubGroupsEditor({ teamId, canEdit }: Props) {
  const [loading, setLoading] = useState(true)
  // Load and save failures are kept apart: a load error replaces the list
  // (the checklist renders it with a Retry), while a save error is a
  // transient banner by the button that must NOT blank the list the user
  // just edited.
  const [loadError, setLoadError] = useState<string | null>(null)
  const [saveError, setSaveError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [candidates, setCandidates] = useState<GitHubTeamCandidate[]>([])
  // Selected mappings keyed "org/slug" → the assignment under this team.
  const [selected, setSelected] = useState<Set<string>>(new Set())

  const base = `/api/settings/team/${encodeURIComponent(teamId)}/github-groups`

  const load = useCallback(async () => {
    setLoading(true)
    setLoadError(null)
    try {
      const data = await apiJSON<GitHubGroupsResponse>(base)
      setCandidates(data.candidates ?? [])
      const sel = new Set<string>()
      for (const g of data.groups ?? []) sel.add(keyOf(g.org_login, g.team_slug))
      setSelected(sel)
    } catch (e) {
      setLoadError(e instanceof Error ? e.message : 'Failed to load GitHub teams')
    } finally {
      setLoading(false)
    }
  }, [base])

  useEffect(() => {
    void load()
  }, [load])

  const toggle = (org: string, slug: string) => {
    const k = keyOf(org, slug)
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(k)) next.delete(k)
      else next.add(k)
      return next
    })
    setSaved(false)
  }

  const save = async () => {
    setSaving(true)
    setSaveError(null)
    setSaved(false)
    try {
      const groups: GitHubGroup[] = Array.from(selected).map((k) => {
        const [org_login, team_slug] = k.split('/')
        return { org_login, team_slug }
      })
      await apiFetch(base, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ groups }),
      })
      setSaved(true)
    } catch (e) {
      setSaveError(e instanceof Error ? e.message : 'Failed to save')
    } finally {
      setSaving(false)
    }
  }

  return (
    <section className="border border-border-subtle rounded-xl p-4 space-y-3">
      <div>
        <h3 className="text-[13px] font-medium text-text-primary">GitHub teams</h3>
        <p className="text-[12px] text-text-tertiary mt-1">
          Assign GitHub teams to this Triage Factory team. When a pull request requests review from
          one of these GitHub teams (via CODEOWNERS), the work routes to this team's board. Names
          only — no membership or nesting is read.
        </p>
      </div>

      {/* Shared body: search + selected/unselected/all filter + bounded
          scrollable list + badges + loading/error/empty states. */}
      <GitHubTeamChecklist
        candidates={candidates}
        selected={selected}
        onToggle={toggle}
        loading={loading}
        error={loadError}
        onRetry={() => void load()}
        disabled={!canEdit || saving}
        emptyLabel="No GitHub teams found. Connect GitHub and configure repositories whose org exposes teams, then reopen this panel."
      />

      {saveError && <p className="text-[12px] text-dismiss">{saveError}</p>}

      {canEdit && (
        <button
          type="button"
          onClick={() => void save()}
          disabled={saving || loading}
          className="bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-lg px-3 py-1.5 text-[12px] transition-colors"
        >
          {saving ? 'Saving…' : saved ? 'Saved' : 'Save GitHub teams'}
        </button>
      )}
    </section>
  )
}
