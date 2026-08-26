import { useState, useEffect, useCallback, useId, useRef } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import { toast } from './Toast/toastStore'
import { apiFetch, apiJSON, httpErrorMessage } from '../lib/apiClient'
import { useFocusTrap } from '../hooks/useFocusTrap'
import TeamPicker from './TeamPicker'
import MarketplacePublishControl from './MarketplacePublishControl'
import { useTeams, pickerDefault, noteWrittenTeam } from '../hooks/useTeams'
import { useModelCatalog, modelCatalogEntry, modelDisplayName } from '../hooks/useModelCatalog'
import ModelPicker from './ModelPicker'
import { useApiOrgId } from '../hooks/useApiOrgId'
import { gateModelSave } from '../lib/modelGate'
import { promptsBase, blueprintsBase } from '../lib/scope'

interface Props {
  promptId: string | null
  isNew?: boolean
  // When set (the single-team prompts page), a new prompt is created under
  // this team and the per-modal TeamPicker is replaced by a read-only
  // label — the page's active team is the single source of truth. Empty /
  // undefined keeps the modal's own write picker (standalone / solo use).
  lockedTeamId?: string
  // When true the drawer is a read-only viewer of an existing prompt (TFAC-447):
  // inputs are disabled and the Save/Delete affordances are withheld, so a
  // team viewer can inspect a prompt without being offered writes that 403
  // server-side. Only meaningful in edit mode — viewers never reach the create
  // flow (the "New Prompt" affordance is hidden upstream).
  readOnly?: boolean
  onClose: () => void
  onSaved: () => void
  onDeleted?: () => void
}

interface PromptStatsData {
  total_runs: number
  completed_runs: number
  failed_runs: number
  success_rate: number
  avg_cost_usd: number
  avg_duration_ms: number
  total_cost_usd: number
  last_used_at: string | null
  runs_per_day: { date: string; count: number }[]
}

const MIN_WIDTH = 380
const MAX_WIDTH = 900
const STORAGE_KEY = 'prompt-drawer-width'

function loadWidth(): number {
  try {
    const stored = localStorage.getItem(STORAGE_KEY)
    if (stored) {
      const w = parseInt(stored, 10)
      if (w >= MIN_WIDTH && w <= MAX_WIDTH) return w
    }
  } catch {
    // best effort — localStorage may be disabled
  }
  return 520
}

export default function PromptDrawer({
  promptId,
  isNew,
  lockedTeamId,
  readOnly = false,
  onClose,
  onSaved,
  onDeleted,
}: Props) {
  const base = promptsBase()
  const [name, setName] = useState('')
  const [body, setBody] = useState('')
  const [source, setSource] = useState('user')
  // Acting team — create mode only; a prompt's team is fixed
  // after creation. TeamPicker renders only at ≥2 teams; below that
  // `team` stays '' and the server resolves the sole team. teamsLoaded
  // gates the create submit so a multi-team user can't submit team_id:''
  // in the cold-load window before /api/teams resolves.
  const { teams, lastActingTeamId, loaded: teamsLoaded } = useTeams()
  const [team, setTeam] = useState('')
  // When the page locks the team (single-team prompts page), create uses it
  // directly and the picker is hidden; otherwise the modal's own write
  // picker applies. lockedTeamId is non-empty only for a ≥2-team user on the
  // prompts page — by then teams are loaded, so no cold-load gate is needed.
  const effectiveTeam = lockedTeamId || team
  const [model, setModel] = useState('')
  const [defaultModel, setDefaultModel] = useState('')
  // The models this org offers. A prompt's Model is a pin drawn from the same
  // catalog the team default is, so the two pickers can never disagree about
  // what exists.
  const { models, loaded: modelsLoaded } = useModelCatalog()
  // The org whose catalog those models came from — what the save gate addresses
  // its test to.
  const orgId = useApiOrgId()
  const [stats, setStats] = useState<PromptStatsData | null>(null)
  const [saving, setSaving] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [error, setError] = useState('')
  const [width, setWidth] = useState(loadWidth)
  const dragging = useRef(false)
  const startX = useRef(0)
  const startWidth = useRef(0)

  // A pin the catalog no longer offers still has to render as the selection, or
  // the drawer would silently show this prompt inheriting a model it does not
  // inherit. It carries no availability field, which is the truth about it: the
  // build makes no claim on a model it no longer describes.
  const pinOptions =
    model !== '' && !models.some((m) => m.key === model)
      ? [...models, { key: model, display_name: modelDisplayName(model), enabled: true }]
      : models

  const open = promptId !== null || isNew

  // Trap keyboard focus inside the drawer while open and restore it to the
  // trigger on close (WCAG 2.1.2). Initial focus lands on the name field,
  // except read-only mode, where it's disabled — the default (first
  // focusable) applies there instead.
  const dialogRef = useRef<HTMLDivElement>(null)
  const nameRef = useRef<HTMLInputElement>(null)
  const titleId = useId()
  useFocusTrap(dialogRef, { active: open, initialFocus: readOnly ? undefined : nameRef })

  // Refreshes when the drawer (re)opens so a Settings change is
  // reflected in the "Default" option's label without a page reload.
  // Scoped to the prompt's own team (effectiveTeam) so the "Default"
  // label shows the model that team's setting resolves to — the prompt
  // is created under this team, and per-team DefaultModel can differ.
  // '' (solo/local) falls back to the "default" alias the backend accepts.
  useEffect(() => {
    if (!open) return
    let cancelled = false
    const settingsTeam = effectiveTeam || 'default'
    apiJSON<{ team_settings?: { DefaultModel?: string } }>(
      `/api/teams/${encodeURIComponent(settingsTeam)}/settings`,
    )
      .then((data) => {
        if (!cancelled && data?.team_settings?.DefaultModel)
          setDefaultModel(data.team_settings.DefaultModel)
      })
      .catch(() => {})
    return () => {
      cancelled = true
    }
  }, [open, effectiveTeam])

  useEffect(() => {
    if (isNew) {
      setName('')
      setBody('')
      setSource('user')
      setModel('')
      setStats(null)
      setError('')
      return
    }
    if (!promptId) return
    let cancelled = false
    apiJSON<{ name: string; body: string; source: string; model?: string }>(`${base}/${promptId}`)
      .then((data) => {
        if (cancelled) return
        setName(data.name)
        setBody(data.body)
        setSource(data.source)
        setModel(data.model ?? '')
        setError('')
      })
      .catch((err) => {
        if (!cancelled) setError(httpErrorMessage(err, 'Could not load the prompt.'))
      })

    apiJSON<PromptStatsData>(`${base}/${promptId}/stats`)
      .then((data) => {
        if (!cancelled) setStats(data)
      })
      .catch(() => {
        if (!cancelled) setStats(null)
      })

    return () => {
      cancelled = true
    }
  }, [promptId, isNew, base])

  // Seed the acting team for create mode once the teams load. Kept in its
  // own effect (not the form-reset above) so a late teams fetch reseeds
  // without re-running the whole reset and clobbering user input. Gated
  // on teamsLoaded so the seed reflects the real team set, not the empty
  // pre-fetch state.
  useEffect(() => {
    if (!isNew || !teamsLoaded) return
    setTeam(pickerDefault(teams, lastActingTeamId))
  }, [isNew, open, teamsLoaded, teams, lastActingTeamId])

  // Resize drag handlers
  const onMouseDown = useCallback(
    (e: React.MouseEvent) => {
      e.preventDefault()
      dragging.current = true
      startX.current = e.clientX
      startWidth.current = width
      document.body.style.cursor = 'col-resize'
      document.body.style.userSelect = 'none'
    },
    [width],
  )

  useEffect(() => {
    const onMouseMove = (e: MouseEvent) => {
      if (!dragging.current) return
      const delta = startX.current - e.clientX
      const newWidth = Math.min(MAX_WIDTH, Math.max(MIN_WIDTH, startWidth.current + delta))
      setWidth(newWidth)
    }
    const onMouseUp = () => {
      if (!dragging.current) return
      dragging.current = false
      document.body.style.cursor = ''
      document.body.style.userSelect = ''
      // Persist on release
      localStorage.setItem(STORAGE_KEY, String(width))
    }
    window.addEventListener('mousemove', onMouseMove)
    window.addEventListener('mouseup', onMouseUp)
    return () => {
      window.removeEventListener('mousemove', onMouseMove)
      window.removeEventListener('mouseup', onMouseUp)
    }
  }, [width])

  const save = async () => {
    if (!name.trim()) {
      setError('Name is required')
      return
    }
    if (!body.trim()) {
      setError('Body is required')
      return
    }
    // A pin is a model choice like any other, so it passes the same gate: one
    // nothing has established these credentials can run is tested first, and
    // the prompt is written only on green. An unpinned prompt (model === '')
    // inherits the team default and has nothing of its own to test.
    const picked = modelCatalogEntry(orgId, model)
    if (picked && !(await gateModelSave(orgId!, picked))) return

    setSaving(true)
    setError('')

    try {
      // Create under copy-only prompts: a new prompt is auto-wrapped in its own
      // 1-step blueprint so it lands on the canvas as a wireable node. We POST
      // first_prompt to the blueprint endpoint, which creates prompt +
      // blueprint + step atomically (the server defaults the blueprint name to
      // the prompt name). Edit (PUT) still targets the prompt directly.
      if (isNew) {
        const firstPrompt = { name, body, model }
        const createBody = { first_prompt: firstPrompt, team_id: effectiveTeam }
        await apiFetch(blueprintsBase(), {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(createBody),
        })
      } else {
        await apiFetch(`${base}/${promptId}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name, body, model }),
        })
      }
      if (isNew && effectiveTeam) noteWrittenTeam(effectiveTeam)

      onSaved()
    } catch (err) {
      toast.error(httpErrorMessage(err, `Could not ${isNew ? 'create' : 'save'} the prompt.`))
    } finally {
      setSaving(false)
    }
  }

  const handleDelete = async () => {
    if (!promptId) return
    setDeleting(true)
    try {
      // Deleting a prompt that sat mid-chain in a multi-step blueprint fragments
      // the chain: the steps after it become a new, trigger-less blueprint. The
      // server flags that so we can surface it — the downstream half no longer
      // fires on its own. (The canvas refetch via onDeleted re-renders the split.)
      const res = await apiFetch(`${base}/${promptId}`, { method: 'DELETE' })
      const deleteResp = (await res.json().catch(() => null)) as {
        orphaned_blueprint?: boolean
      } | null
      if (deleteResp?.orphaned_blueprint) {
        toast.info('Steps after the deleted prompt are now an untriggered blueprint.')
      }
      onDeleted?.()
    } catch (err) {
      toast.error(
        httpErrorMessage(err, `Could not ${source === 'user' ? 'delete' : 'hide'} the prompt.`),
      )
    } finally {
      setDeleting(false)
    }
  }

  return (
    <AnimatePresence>
      {open && (
        <>
          {/* Backdrop */}
          <motion.div
            className="fixed inset-0 bg-scrim backdrop-blur-sm z-40"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            onClick={onClose}
          />

          {/* Drawer */}
          <motion.div
            ref={dialogRef}
            tabIndex={-1}
            role="dialog"
            aria-modal="true"
            aria-labelledby={titleId}
            className="fixed top-0 right-0 bottom-0 z-50 bg-raised border-l border-line-1 shadow-float shadow-black/10 flex flex-col"
            style={{ width: Math.min(width, window.innerWidth * 0.9) }}
            initial={{ x: '100%' }}
            animate={{ x: 0 }}
            exit={{ x: '100%' }}
            transition={{ type: 'spring', damping: 30, stiffness: 300 }}
          >
            {/* Resize handle */}
            <div
              onMouseDown={onMouseDown}
              className="absolute left-0 top-0 bottom-0 w-1.5 cursor-col-resize hover:bg-warm/20 active:bg-warm/30 transition-colors z-10"
            />

            {/* Header */}
            <div className="px-6 py-5 border-b border-line-1 flex items-center justify-between shrink-0">
              <div className="flex items-center gap-3">
                <h2 id={titleId} className="text-column font-semibold text-ink-1">
                  {isNew ? 'New Prompt' : 'Edit Prompt'}
                </h2>
              </div>
              <button
                onClick={onClose}
                className="text-ink-3 hover:text-ink-2 transition-colors text-lg leading-none px-1"
              >
                &times;
              </button>
            </div>

            {/* Body — scrollable */}
            <div className="flex-1 overflow-y-auto px-6 py-5 space-y-5">
              {/* Team (create mode only — a prompt's team is fixed after creation).
                  Locked to the page's active team on the single-team prompts page;
                  otherwise the modal's own write picker (≥2 teams). */}
              {isNew &&
                (lockedTeamId ? (
                  <div className="text-ui text-ink-3">
                    Team:{' '}
                    <span className="font-medium text-ink-2">
                      {teams.find((t) => t.id === lockedTeamId)?.name ?? 'current team'}
                    </span>
                  </div>
                ) : (
                  <TeamPicker value={team} onChange={setTeam} label="Team" />
                ))}
              {/* Name */}
              <div>
                <label className="block text-ui font-medium text-ink-2 mb-1.5">Name</label>
                <input
                  ref={nameRef}
                  type="text"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  disabled={readOnly}
                  placeholder="e.g. Thorough Code Review"
                  className="w-full px-3 py-2 rounded-lg border border-line-1 bg-raised text-body text-ink-1 placeholder:text-ink-3 focus:outline-none focus:border-warm/40 focus:ring-1 focus:ring-warm/20 transition-colors disabled:opacity-60"
                />
              </div>

              {/* Body */}
              <div>
                <label className="block text-ui font-medium text-ink-2 mb-1.5">Prompt Body</label>
                <textarea
                  value={body}
                  onChange={(e) => setBody(e.target.value)}
                  disabled={readOnly}
                  placeholder="Describe what the agent should do..."
                  rows={16}
                  className="w-full px-3 py-2.5 rounded-lg border border-line-1 bg-raised text-body text-ink-1 font-mono leading-relaxed placeholder:text-ink-3 focus:outline-none focus:border-warm/40 focus:ring-1 focus:ring-warm/20 transition-colors resize-y disabled:opacity-60"
                />
                <p className="text-label text-ink-3 mt-1.5">
                  Every run automatically receives the task's context.
                </p>
              </div>

              <div>
                <label className="block text-ui font-medium text-ink-2 mb-1.5">Model</label>
                <ModelPicker
                  value={model}
                  onChange={setModel}
                  readOnly={readOnly}
                  models={pinOptions}
                  loaded={modelsLoaded}
                  ariaLabel="Prompt model"
                  unsetOption={{
                    label: 'Default',
                    detail: defaultModel
                      ? `Whatever this team runs on — ${modelDisplayName(defaultModel)} today.`
                      : 'Whatever this team runs on.',
                  }}
                />
                <p className="text-label text-ink-3 mt-1.5">
                  Default tracks the model chosen in Settings — change it there and every prompt
                  using Default follows.
                </p>
              </div>

              {/* Stats */}
              {!isNew && stats && stats.total_runs > 0 && (
                <div>
                  <label className="block text-ui font-medium text-ink-2 mb-2">Performance</label>
                  <div className="bg-tint-2 rounded-lg border border-line-1 p-3 space-y-3">
                    {/* Stat pills */}
                    <div className="flex gap-2 flex-wrap">
                      <StatPill label="Runs" value={String(stats.total_runs)} />
                      <StatPill label="Avg cost" value={`$${stats.avg_cost_usd.toFixed(3)}`} />
                      <StatPill
                        label="Success"
                        value={`${Math.round(stats.success_rate * 100)}%`}
                        color={
                          stats.success_rate >= 0.8
                            ? 'text-ink-2'
                            : stats.success_rate >= 0.5
                              ? 'text-warm'
                              : 'text-alarm'
                        }
                      />
                      <StatPill label="Avg time" value={formatDuration(stats.avg_duration_ms)} />
                    </div>

                    {/* Sparkline */}
                    {stats.runs_per_day.length > 0 && (
                      <div>
                        <div className="flex items-end gap-[2px] h-8">
                          {stats.runs_per_day.map((d, i) => {
                            const max = Math.max(...stats.runs_per_day.map((r) => r.count), 1)
                            const pct = d.count / max
                            return (
                              <div
                                key={i}
                                className="flex-1 rounded-sm transition-all"
                                style={{
                                  height: `${Math.max(pct * 100, 4)}%`,
                                  background:
                                    d.count > 0 ? 'var(--color-warm)' : 'var(--color-line-1)',
                                  opacity: d.count > 0 ? 0.7 : 0.3,
                                }}
                                title={`${d.date}: ${d.count} run${d.count !== 1 ? 's' : ''}`}
                              />
                            )
                          })}
                        </div>
                        <div className="flex justify-between mt-1">
                          <span className="text-label-sm text-ink-3">30d ago</span>
                          <span className="text-label-sm text-ink-3">today</span>
                        </div>
                      </div>
                    )}

                    {/* Last used + totals */}
                    <div className="flex justify-between text-label text-ink-3 pt-1 border-t border-line-1">
                      <span>Total spend: ${stats.total_cost_usd.toFixed(2)}</span>
                      {stats.last_used_at && <span>Last used {formatAge(stats.last_used_at)}</span>}
                    </div>
                  </div>
                </div>
              )}

              {/* Source badge */}
              {!isNew && source && (
                <div className="flex items-center gap-2">
                  <span
                    className={`text-label-sm font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded ${source === 'system' ? 'bg-tint-3 text-ink-3' : 'bg-warm/10 text-warm'}`}
                  >
                    {source}
                  </span>
                </div>
              )}

              {/* Marketplace publish/republish/delist (TFAC-536) — standalone
                  prompts are kind=prompt listings. Multi-mode only (the
                  control renders nothing in local mode). */}
              {!isNew && promptId && (
                <div>
                  <label className="block text-ui font-medium text-ink-2 mb-1.5">Marketplace</label>
                  <MarketplacePublishControl
                    kind="prompt"
                    sourceId={promptId}
                    defaultName={name}
                    readOnly={readOnly}
                  />
                </div>
              )}
            </div>

            {/* Footer */}
            <div className="px-6 py-4 border-t border-line-1 flex items-center justify-between shrink-0">
              <div>
                {!isNew && !readOnly && (
                  <button
                    onClick={handleDelete}
                    disabled={deleting}
                    title={
                      source === 'user'
                        ? 'Permanently delete this prompt'
                        : 'Hide this prompt — it will not reappear on import'
                    }
                    className="text-ui text-ink-3 hover:text-alarm font-medium transition-colors disabled:opacity-50"
                  >
                    {deleting
                      ? source === 'user'
                        ? 'Deleting...'
                        : 'Hiding...'
                      : source === 'user'
                        ? 'Delete'
                        : 'Hide'}
                  </button>
                )}
              </div>

              <div className="flex items-center gap-3">
                {error && <span className="text-ui text-alarm">{error}</span>}
                {readOnly ? (
                  // View-only: no Save/Delete; the only action is to dismiss the
                  // inspector. The label reads "Close" (not "Cancel") since there's
                  // nothing to cancel.
                  <>
                    <span className="text-reported font-medium text-ink-3">View only</span>
                    <button
                      onClick={onClose}
                      className="text-ui font-semibold text-warm-ink bg-warm hover:bg-warm/90 px-4 py-1.5 rounded-full transition-colors"
                    >
                      Close
                    </button>
                  </>
                ) : (
                  <>
                    <button
                      onClick={onClose}
                      className="text-ui text-ink-3 hover:text-ink-2 font-medium transition-colors"
                    >
                      Cancel
                    </button>
                    <button
                      onClick={save}
                      disabled={saving || (isNew && !lockedTeamId && !teamsLoaded)}
                      className="text-ui font-semibold text-warm-ink bg-warm hover:bg-warm/90 px-4 py-1.5 rounded-full transition-colors disabled:opacity-50"
                    >
                      {saving ? 'Saving...' : isNew ? 'Create' : 'Save'}
                    </button>
                  </>
                )}
              </div>
            </div>
          </motion.div>
        </>
      )}
    </AnimatePresence>
  )
}

function StatPill({ label, value, color }: { label: string; value: string; color?: string }) {
  return (
    <div className="bg-raised border border-line-1 rounded-md px-2 py-1">
      <div className={`text-ui font-semibold ${color || 'text-ink-1'}`}>{value}</div>
      <div className="text-label-sm text-ink-3">{label}</div>
    </div>
  )
}

function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`
  const s = Math.floor(ms / 1000)
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  const rem = s % 60
  return rem > 0 ? `${m}m ${rem}s` : `${m}m`
}

function formatAge(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime()
  const hours = Math.floor(diff / (1000 * 60 * 60))
  if (hours < 1) return 'just now'
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(hours / 24)
  return `${days}d ago`
}
