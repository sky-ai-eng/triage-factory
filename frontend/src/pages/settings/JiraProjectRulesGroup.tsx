import { useEffect, useRef, useState } from 'react'
import { ChevronDown, ChevronRight, Trash2 } from 'lucide-react'
import { Section } from './primitives'
import JiraStatusRule from '../../components/JiraStatusRule'
import { emptyProject, projectIsComplete, type JiraProjectConfig } from './teamConfig'
import { apiJSON } from '../../lib/apiClient'

interface JiraStatus {
  id: string
  name: string
}

/**
 * JiraProjectRulesGroup is the team-scope Jira project-tracking field group:
 * the list of tracked projects and, per project, the pickup / in-progress /
 * done status rules. A controlled component — the container owns the
 * projects array (`value`) and the actual PUT /api/teams/{id}/jira-projects — so
 * the same editor serves the team Settings tab and the setup wizard's
 * team steps.
 *
 * It owns only its own presentational state: per-project expand/collapse and
 * the fetched status options (loaded on demand from /api/jira/statuses,
 * which intersects across the queried projects so the returned list is safe
 * to offer in every project's picker). Org-level Jira *access* (credentials,
 * connect/disconnect) lives in JiraAccessGroup; this is the team-scoped
 * project rules only, suppressed until Jira is connected.
 */
export default function JiraProjectRulesGroup({
  value,
  onChange,
  connected,
  bare = false,
}: {
  value: JiraProjectConfig[]
  onChange: (next: JiraProjectConfig[]) => void
  connected: boolean
  // The setup wizard composes this flush (no Section card, glass project rows);
  // Settings keeps the carded default.
  bare?: boolean
}) {
  // Statuses keyed by project key so each project's picker pulls from the
  // right per-project list. The backend intersects across the queried
  // projects, so the same list is mirrored under each requested key.
  const [statusesByProject, setStatusesByProject] = useState<Record<string, JiraStatus[]>>({})
  // The (trimmed) project keys whose status fetch is in flight. Per-key rather
  // than a single boolean so fetching project A then B doesn't re-enable B's
  // button when A's request settles first.
  const [loadingKeys, setLoadingKeys] = useState<Set<string>>(new Set())
  // Per-project expand/collapse keyed by index (the key field is editable
  // mid-render, so keying on it would drop open/closed state every
  // keystroke). For the common N=1 case the sole project starts expanded.
  const [expandedKeys, setExpandedKeys] = useState<Record<string, boolean>>({})

  // One-shot mount seed: expand the lone project and pull statuses for any
  // already-tracked projects. Guarded so editing `value` afterwards doesn't
  // re-run it (which would clobber the user's expand state / re-fetch).
  const seeded = useRef(false)
  useEffect(() => {
    if (seeded.current) return
    seeded.current = true
    // Expand the lone project. Key on idx_0 (not the project key) to match the
    // rest of the component — keying on the editable project key would drop
    // the open state the moment the user edits that key.
    if (value.length === 1) {
      setExpandedKeys({ idx_0: true })
    }
    if (connected) {
      const keys = value.map((p) => p.key).filter(Boolean)
      if (keys.length > 0) void fetchJiraStatuses(keys)
    }
    // Mount-only seed — deps intentionally omitted.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // fetchJiraStatuses queries the backend for statuses across the given
  // projects. The returned list is the intersection (a status not in every
  // queried project would fail TransitionTo), so it's mirrored under each
  // requested key. Keys are trimmed here regardless of source, so the query is
  // well-formed and the results are stored under the same trimmed key the
  // lookup uses (raw call-site keys would store under a key the picker's
  // trimmed lookup can't find).
  const fetchJiraStatuses = async (projectKeys?: string[]) => {
    const keys = (projectKeys ?? value.map((p) => p.key)).map((k) => k.trim()).filter(Boolean)
    if (keys.length === 0) return
    setLoadingKeys((prev) => new Set([...prev, ...keys]))
    try {
      const params = keys.map((p) => `project=${encodeURIComponent(p)}`).join('&')
      const statuses = await apiJSON<JiraStatus[]>(`/api/jira/statuses?${params}`)
      const next: Record<string, JiraStatus[]> = {}
      for (const k of keys) next[k] = statuses
      setStatusesByProject((current) => ({ ...current, ...next }))
    } catch {
      // Non-critical — the picker just shows no options until a retry.
    } finally {
      setLoadingKeys((prev) => {
        const n = new Set(prev)
        for (const k of keys) n.delete(k)
        return n
      })
    }
  }

  const updateProject = (i: number, patch: Partial<JiraProjectConfig>) => {
    const next = value.slice()
    next[i] = { ...next[i], ...patch }
    onChange(next)
  }

  const addProject = () => {
    // The appended section lives at index === current length; stamp that
    // index into expandedKeys so the same key isExpanded reads next render
    // starts open.
    const newIndex = value.length
    onChange([...value, emptyProject('')])
    setExpandedKeys((m) => ({ ...m, [`idx_${newIndex}`]: true }))
  }

  const removeProject = (i: number) => {
    const next = value.slice()
    next.splice(i, 1)
    onChange(next)
    // Shift idx_ entries above the removed slot down one; drop idx_i.
    setExpandedKeys((m) => {
      const out: Record<string, boolean> = {}
      for (const [k, v] of Object.entries(m)) {
        if (!k.startsWith('idx_')) {
          out[k] = v
          continue
        }
        const idx = Number(k.slice('idx_'.length))
        if (Number.isNaN(idx) || idx < i) out[k] = v
        else if (idx > i) out[`idx_${idx - 1}`] = v
      }
      return out
    })
  }

  const toggleExpanded = (i: number) => {
    const id = `idx_${i}`
    setExpandedKeys((m) => ({ ...m, [id]: !m[id] }))
  }

  const isExpanded = (i: number): boolean => expandedKeys[`idx_${i}`] === true

  const inner = (
    <>
      {bare ? (
        <div className="mb-4 space-y-1.5">
          <h2 className="text-[19px] font-medium tracking-tight text-text-primary">
            Jira projects
          </h2>
          <p className="text-[13px] leading-relaxed text-text-tertiary">
            Track Jira projects and map each one&rsquo;s statuses to pickup / in-progress / done.
          </p>
        </div>
      ) : (
        <h2 className="mb-4 text-[13px] font-medium text-text-secondary">Jira projects</h2>
      )}
      {!connected ? (
        <p className="text-[12px] text-text-tertiary italic">
          Connect Jira under Workspace settings before configuring tracked projects.
        </p>
      ) : (
        <div className="space-y-2">
          {value.length === 0 && (
            <p className="text-[12px] text-text-tertiary italic">
              No Jira projects configured. Click &ldquo;Add project&rdquo; to start.
            </p>
          )}
          {value.map((project, i) => {
            // Trimmed key drives the status lookup, the fetch arg, and the
            // disabled gate, so whitespace a user types around a key can't
            // desync them (fetchJiraStatuses stores under the trimmed key).
            const key = project.key.trim()
            const statuses = statusesByProject[key] || []
            const complete = projectIsComplete(project)
            const expanded = isExpanded(i)
            return (
              <div
                key={i}
                className={`rounded-xl border ${
                  bare
                    ? 'border-[var(--color-border-glass)] bg-[var(--color-surface-overlay)]/50'
                    : 'border-border-subtle bg-white/40'
                }`}
              >
                <div className="flex items-center gap-2 px-3 py-2">
                  <button
                    type="button"
                    onClick={() => toggleExpanded(i)}
                    className="text-text-tertiary hover:text-text-secondary"
                    aria-label={expanded ? 'Collapse project' : 'Expand project'}
                  >
                    {expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
                  </button>
                  <input
                    type="text"
                    placeholder="PROJ"
                    value={project.key}
                    onChange={(e) => updateProject(i, { key: e.target.value })}
                    className="flex-1 bg-transparent border-0 focus:outline-none text-[13px] font-medium text-text-primary placeholder-text-tertiary"
                  />
                  {project.key.trim() !== '' && (
                    <span
                      className={`text-[10px] uppercase tracking-wide ${
                        complete ? 'text-claim' : 'text-snooze'
                      }`}
                    >
                      {complete ? 'Ready' : 'Needs rules'}
                    </span>
                  )}
                  <button
                    type="button"
                    onClick={() => removeProject(i)}
                    className="text-text-tertiary hover:text-dismiss"
                    aria-label="Remove project"
                  >
                    <Trash2 size={14} />
                  </button>
                </div>

                {expanded && (
                  <div className="px-4 pb-4 pt-1 space-y-3">
                    <div className="flex items-center justify-between">
                      <p className="text-[11px] text-text-tertiary">
                        {statuses.length > 0
                          ? `${statuses.length} statuses available`
                          : 'Click Fetch Statuses to load options'}
                      </p>
                      <button
                        type="button"
                        onClick={() => fetchJiraStatuses([key].filter(Boolean))}
                        disabled={loadingKeys.has(key) || !key}
                        className="shrink-0 text-[11px] text-accent hover:text-accent/80 disabled:opacity-40 border border-accent/20 rounded-xl px-3 py-1 transition-colors"
                      >
                        {loadingKeys.has(key) ? 'Loading...' : 'Fetch Statuses'}
                      </button>
                    </div>

                    {statuses.length > 0 && (
                      <div className="space-y-4 pt-1">
                        <JiraStatusRule
                          label="Pickup"
                          description="Poll for unassigned tickets in these states."
                          allStatuses={statuses}
                          value={project.pickup}
                          onChange={(v) => updateProject(i, { pickup: v })}
                          requireCanonical={false}
                        />
                        <JiraStatusRule
                          label="In progress"
                          description="Count as actively being worked on."
                          allStatuses={statuses}
                          value={project.in_progress}
                          onChange={(v) => updateProject(i, { in_progress: v })}
                          requireCanonical={true}
                          canonicalPrompt="Claim →"
                        />
                        <JiraStatusRule
                          label="Done"
                          description="Count as complete (add every variant — e.g. Resolved + Verified)."
                          allStatuses={statuses}
                          value={project.done}
                          onChange={(v) => updateProject(i, { done: v })}
                          requireCanonical={true}
                          canonicalPrompt="Complete →"
                        />
                      </div>
                    )}
                  </div>
                )}
              </div>
            )
          })}
          <button
            type="button"
            onClick={addProject}
            className="w-full text-[12px] text-accent hover:text-accent/80 border border-dashed border-accent/30 rounded-xl px-3 py-2 transition-colors"
          >
            + Add project
          </button>
        </div>
      )}
    </>
  )

  return bare ? inner : <Section>{inner}</Section>
}
