import { useEffect, useMemo, useState } from 'react'
import { apiFetch, apiJSON } from '../lib/apiClient'

// One stored mapping row: a fully-qualified GitHub team funneled into
// this TF team.
interface GitHubGroup {
  org_login: string
  team_slug: string
}

// One live GitHub team the admin can assign (name is display-only).
interface GitHubGroupCandidate {
  org_login: string
  team_slug: string
  name: string
}

interface GitHubGroupsResponse {
  groups: GitHubGroup[]
  candidates: GitHubGroupCandidate[]
  role: string
}

interface Props {
  // Team path segment — a UUID or the literal "default" (matches the
  // sibling team-settings calls).
  teamId: string
  canEdit: boolean
}

const keyOf = (org: string, slug: string) => `${org.toLowerCase()}/${slug.toLowerCase()}`

// GitHubGroupsEditor is the import-and-choose UI for the GitHub-team →
// TF-team review-request mapping (the twin of the Jira project rules
// editor). It owns its own GET/PUT against
// /api/settings/team/{teamId}/github-groups, deliberately separate from
// the big team-settings save-all flow so it can fetch the org's live
// GitHub teams (and trigger the deletion-reconcile floor) on open
// without coupling to the rest of the form.
export default function GitHubGroupsEditor({ teamId, canEdit }: Props) {
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [candidates, setCandidates] = useState<GitHubGroupCandidate[]>([])
  // Selected mappings keyed "org/slug" → the assignment under this team.
  const [selected, setSelected] = useState<Set<string>>(new Set())
  // Names for display, learned from candidates.
  const [names, setNames] = useState<Map<string, string>>(new Map())

  const base = `/api/settings/team/${encodeURIComponent(teamId)}/github-groups`

  useEffect(() => {
    let alive = true
    setLoading(true)
    setError(null)
    apiJSON<GitHubGroupsResponse>(base)
      .then((data) => {
        if (!alive) return
        setCandidates(data.candidates ?? [])
        const sel = new Set<string>()
        for (const g of data.groups ?? []) sel.add(keyOf(g.org_login, g.team_slug))
        setSelected(sel)
        const nm = new Map<string, string>()
        for (const c of data.candidates ?? []) nm.set(keyOf(c.org_login, c.team_slug), c.name)
        setNames(nm)
      })
      .catch((e) => {
        if (alive) setError(e instanceof Error ? e.message : 'Failed to load GitHub teams')
      })
      .finally(() => {
        if (alive) setLoading(false)
      })
    return () => {
      alive = false
    }
  }, [base])

  // The rows to render: every live candidate, plus any selected mapping
  // that GitHub didn't return (stale, or a repo whose org we couldn't
  // read). The admin can always un-assign a stale mapping even when it's
  // no longer a candidate.
  const rows = useMemo(() => {
    const seen = new Set<string>()
    const out: { org_login: string; team_slug: string; name: string; orphan: boolean }[] = []
    for (const c of candidates) {
      const k = keyOf(c.org_login, c.team_slug)
      seen.add(k)
      out.push({ org_login: c.org_login, team_slug: c.team_slug, name: c.name, orphan: false })
    }
    for (const k of selected) {
      if (seen.has(k)) continue
      const [org, slug] = k.split('/')
      out.push({ org_login: org, team_slug: slug, name: names.get(k) ?? '', orphan: true })
    }
    out.sort((a, b) =>
      keyOf(a.org_login, a.team_slug).localeCompare(keyOf(b.org_login, b.team_slug)),
    )
    return out
  }, [candidates, selected, names])

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
    setError(null)
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
      setError(e instanceof Error ? e.message : 'Failed to save')
    } finally {
      setSaving(false)
    }
  }

  return (
    <section className="border border-border rounded-xl p-4 space-y-3">
      <div>
        <h3 className="text-[13px] font-medium text-fg">GitHub teams</h3>
        <p className="text-[12px] text-fg-muted mt-1">
          Assign GitHub teams to this Triage Factory team. When a pull request requests review from
          one of these GitHub teams (via CODEOWNERS), the work routes to this team's board. Names
          only — no membership or nesting is read.
        </p>
      </div>

      {loading && <p className="text-[12px] text-fg-muted">Loading GitHub teams…</p>}

      {!loading && rows.length === 0 && (
        <p className="text-[12px] text-fg-muted">
          No GitHub teams found. Connect GitHub and configure repositories whose org exposes teams,
          then reopen this panel.
        </p>
      )}

      {!loading && rows.length > 0 && (
        <ul className="space-y-1.5">
          {rows.map((r) => {
            const k = keyOf(r.org_login, r.team_slug)
            return (
              <li key={k} className="flex items-center gap-2">
                <input
                  id={`ghteam-${k}`}
                  type="checkbox"
                  disabled={!canEdit || saving}
                  checked={selected.has(k)}
                  onChange={() => toggle(r.org_login, r.team_slug)}
                  className="accent-accent"
                />
                <label htmlFor={`ghteam-${k}`} className="text-[13px] text-fg cursor-pointer">
                  <span className="font-mono">
                    @{r.org_login}/{r.team_slug}
                  </span>
                  {r.name && <span className="text-fg-muted"> — {r.name}</span>}
                  {r.orphan && (
                    <span className="text-amber-500 ml-1 text-[11px]">(not found on GitHub)</span>
                  )}
                </label>
              </li>
            )
          })}
        </ul>
      )}

      {error && <p className="text-[12px] text-red-500">{error}</p>}

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
