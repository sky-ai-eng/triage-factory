import { useState, useEffect, useCallback, useRef } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import { toast } from './Toast/toastStore'
import { readError } from '../lib/api'
import type { ChainStep, PromptKind } from '../types'
import ChainStepEditor, { type ChainStepDraft } from './ChainStepEditor'

interface Props {
  promptId: string | null
  isNew?: boolean
  onClose: () => void
  onSaved: () => void
  onDeleted?: () => void
}

// TEMPLATE_VAR_GROUPS is the mission-facing subset of what
// internal/delegate/placeholders.go interpolates at runtime. If a prompt
// references a placeholder not listed here, it falls through as a literal
// {{X}} string at spawn time — agents see it and fail loudly, which is
// the right signal. Placeholders that don't apply to the task's event
// type render empty (e.g. {{WORKFLOW_RUN_ID}} on a Jira task).
//
// Deliberately omitted: {{SCOPE}} and {{TOOLS_REFERENCE}}. The replacer
// populates them because the envelope template references them, but they
// are envelope-internal — an author referencing either in their mission
// body would just duplicate what the envelope already contains.
type TemplateVar = { name: string; desc: string }
type TemplateVarGroup = { label: string; caption: string; vars: TemplateVar[] }

const TEMPLATE_VAR_GROUPS: TemplateVarGroup[] = [
  {
    label: 'Always available',
    caption: 'Populated on every delegation regardless of task source.',
    vars: [
      { name: '{{RUN_ID}}', desc: 'Current delegation run ID' },
      { name: '{{BINARY_PATH}}', desc: 'Absolute path to the triagefactory CLI' },
      { name: '{{TASK_TITLE}}', desc: 'Task title' },
      { name: '{{EVENT_TYPE}}', desc: 'e.g. github:pr:ci_check_failed' },
      {
        name: '{{EVENT_METADATA_JSON}}',
        desc: 'Raw event metadata as JSON — escape hatch for custom fields',
      },
    ],
  },
  {
    label: 'GitHub PR tasks',
    caption: 'Populated when the task is on a GitHub PR. Empty on Jira tasks.',
    vars: [
      { name: '{{OWNER}}', desc: 'Repo owner (e.g. sky-ai-eng)' },
      { name: '{{REPO}}', desc: 'Repo name (e.g. triage-factory)' },
      { name: '{{PR_NUMBER}}', desc: 'Pull request number' },
      { name: '{{HEAD_SHA}}', desc: 'Commit SHA the event fired on' },
    ],
  },
  {
    label: 'GitHub CI events',
    caption: 'Populated on ci_check_failed / ci_check_passed events.',
    vars: [
      {
        name: '{{WORKFLOW_RUN_ID}}',
        desc: 'GitHub Actions run ID — empty for third-party CI (Supabase, Circle, etc.)',
      },
      { name: '{{CHECK_NAME}}', desc: 'Failing check name (e.g. test, build)' },
      { name: '{{CHECK_RUN_ID}}', desc: 'Check run ID' },
      { name: '{{CHECK_URL}}', desc: "Link to the check's details page" },
      { name: '{{CONCLUSION}}', desc: 'success / failure / neutral / skipped / stale' },
    ],
  },
  {
    label: 'GitHub review events',
    caption: 'Populated on review_* events.',
    vars: [
      { name: '{{REVIEWER}}', desc: 'Login of the user who submitted the review' },
      { name: '{{REVIEW_TYPE}}', desc: 'approved / commented / changes_requested / dismissed' },
    ],
  },
  {
    label: 'Jira issue tasks',
    caption: 'Populated when the task is on a Jira issue. Empty on GitHub tasks.',
    vars: [
      { name: '{{ISSUE_KEY}}', desc: 'Full issue key (e.g. SKY-123)' },
      { name: '{{PROJECT}}', desc: 'Project prefix (e.g. SKY)' },
      { name: '{{ASSIGNEE}}', desc: 'Issue assignee display name' },
      { name: '{{STATUS}}', desc: 'Current issue status (e.g. To Do)' },
      { name: '{{PRIORITY}}', desc: 'Issue priority (e.g. High)' },
      { name: '{{ISSUE_TYPE}}', desc: 'Issue type (e.g. Task, Bug, Story)' },
      { name: '{{SUMMARY}}', desc: 'Issue summary / title' },
    ],
  },
]

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

export default function PromptDrawer({ promptId, isNew, onClose, onSaved, onDeleted }: Props) {
  const [name, setName] = useState('')
  const [body, setBody] = useState('')
  const [source, setSource] = useState('user')
  const [kind, setKind] = useState<PromptKind>('leaf')
  const [chainDraft, setChainDraft] = useState<ChainStepDraft[]>([])
  const [model, setModel] = useState('')
  const [defaultModel, setDefaultModel] = useState('')
  const [stats, setStats] = useState<PromptStatsData | null>(null)
  const [saving, setSaving] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [error, setError] = useState('')
  const [width, setWidth] = useState(loadWidth)
  const dragging = useRef(false)
  const startX = useRef(0)
  const startWidth = useRef(0)

  const open = promptId !== null || isNew

  // Refreshes when the drawer (re)opens so a Settings change is
  // reflected in the "Default" option's label without a page reload.
  useEffect(() => {
    if (!open) return
    let cancelled = false
    fetch('/api/settings/team/default')
      .then((res) => (res.ok ? res.json() : null))
      .then((data) => {
        if (!cancelled && data?.team_settings?.DefaultModel)
          setDefaultModel(data.team_settings.DefaultModel)
      })
      .catch(() => {})
    return () => {
      cancelled = true
    }
  }, [open])

  useEffect(() => {
    if (isNew) {
      setName('')
      setBody('')
      setSource('user')
      setKind('leaf')
      setChainDraft([])
      setModel('')
      setStats(null)
      setError('')
      return
    }
    if (!promptId) return
    let cancelled = false
    fetch(`/api/prompts/${promptId}`)
      .then((res) => {
        if (!res.ok) throw new Error(`HTTP ${res.status}`)
        return res.json()
      })
      .then((data) => {
        if (cancelled) return
        setName(data.name)
        setBody(data.body)
        setSource(data.source)
        setKind(data.kind === 'chain' ? 'chain' : 'leaf')
        setModel(data.model ?? '')
        setError('')
      })
      .catch(() => {
        if (!cancelled) setError('Failed to load prompt')
      })

    // Always fetch chain steps too — for leaf prompts the response is
    // an empty array, which keeps the toggle from chain → leaf safe
    // (clicking back to chain shows zero steps as expected).
    fetch(`/api/prompts/${promptId}/chain-steps`)
      .then((res) => (res.ok ? res.json() : []))
      .then((data: ChainStep[]) => {
        if (cancelled) return
        setChainDraft(data.map((s) => ({ step_prompt_id: s.step_prompt_id, brief: s.brief })))
      })
      .catch(() => {})

    fetch(`/api/prompts/${promptId}/stats`)
      .then((res) => {
        if (!res.ok) throw new Error(`HTTP ${res.status}`)
        return res.json()
      })
      .then((data) => {
        if (!cancelled) setStats(data)
      })
      .catch(() => {
        if (!cancelled) setStats(null)
      })

    return () => {
      cancelled = true
    }
  }, [promptId, isNew])

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
    if (kind === 'leaf' && !body.trim()) {
      setError('Body is required for leaf prompts')
      return
    }
    if (kind === 'chain' && chainDraft.length === 0) {
      setError('A chain must have at least one step')
      return
    }
    setSaving(true)
    setError('')

    try {
      const res = isNew
        ? await fetch('/api/prompts', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ name, body, kind, model }),
          })
        : await fetch(`/api/prompts/${promptId}`, {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ name, body, kind, model }),
          })
      if (!res.ok) {
        toast.error(await readError(res, `Failed to ${isNew ? 'create' : 'save'} prompt`))
        return
      }

      // For chain prompts, persist the step list immediately after the
      // prompt itself. We use the id from the response so it works for
      // both create (server-assigned id) and update flows.
      if (kind === 'chain') {
        const created = (await res.json()) as { id: string }
        const stepsRes = await fetch(`/api/prompts/${created.id}/chain-steps`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ steps: chainDraft }),
        })
        if (!stepsRes.ok) {
          toast.error(await readError(stepsRes, 'Failed to save chain steps'))
          return
        }
      }
      onSaved()
    } catch (err) {
      toast.error(`Failed to save prompt: ${(err as Error).message}`)
    } finally {
      setSaving(false)
    }
  }

  const handleDelete = async () => {
    if (!promptId) return
    setDeleting(true)
    try {
      const res = await fetch(`/api/prompts/${promptId}`, { method: 'DELETE' })
      if (!res.ok) {
        toast.error(
          await readError(res, `Failed to ${source === 'user' ? 'delete' : 'hide'} prompt`),
        )
        return
      }
      onDeleted?.()
    } catch (err) {
      toast.error(`Failed to delete prompt: ${(err as Error).message}`)
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
            className="fixed inset-0 bg-black/10 backdrop-blur-sm z-40"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            onClick={onClose}
          />

          {/* Drawer */}
          <motion.div
            className="fixed top-0 right-0 bottom-0 z-50 bg-surface-raised border-l border-border-glass shadow-2xl shadow-black/10 flex flex-col"
            style={{ width: Math.min(width, window.innerWidth * 0.9) }}
            initial={{ x: '100%' }}
            animate={{ x: 0 }}
            exit={{ x: '100%' }}
            transition={{ type: 'spring', damping: 30, stiffness: 300 }}
          >
            {/* Resize handle */}
            <div
              onMouseDown={onMouseDown}
              className="absolute left-0 top-0 bottom-0 w-1.5 cursor-col-resize hover:bg-accent/20 active:bg-accent/30 transition-colors z-10"
            />

            {/* Header */}
            <div className="px-6 py-5 border-b border-border-subtle flex items-center justify-between shrink-0">
              <div className="flex items-center gap-3">
                <h2 className="text-[15px] font-semibold text-text-primary">
                  {isNew ? 'New Prompt' : 'Edit Prompt'}
                </h2>
                <div className="inline-flex rounded-md border border-border-subtle overflow-hidden text-[11px]">
                  <button
                    onClick={() => setKind('leaf')}
                    className={`px-2.5 py-1 transition-colors ${
                      kind === 'leaf'
                        ? 'bg-accent text-white'
                        : 'bg-white/50 text-text-secondary hover:bg-white'
                    }`}
                  >
                    Leaf
                  </button>
                  <button
                    onClick={() => setKind('chain')}
                    className={`px-2.5 py-1 transition-colors border-l border-border-subtle ${
                      kind === 'chain'
                        ? 'bg-accent text-white'
                        : 'bg-white/50 text-text-secondary hover:bg-white'
                    }`}
                  >
                    Chain
                  </button>
                </div>
              </div>
              <button
                onClick={onClose}
                className="text-text-tertiary hover:text-text-secondary transition-colors text-lg leading-none px-1"
              >
                &times;
              </button>
            </div>

            {/* Body — scrollable */}
            <div className="flex-1 overflow-y-auto px-6 py-5 space-y-5">
              {/* Name */}
              <div>
                <label className="block text-[12px] font-medium text-text-secondary mb-1.5">
                  Name
                </label>
                <input
                  type="text"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="e.g. Thorough Code Review"
                  className="w-full px-3 py-2 rounded-lg border border-border-subtle bg-white/50 text-[13px] text-text-primary placeholder:text-text-tertiary focus:outline-none focus:border-accent/40 focus:ring-1 focus:ring-accent/20 transition-colors"
                />
              </div>

              {/* Body — leaf prompts only. Chain prompts replace this
                  with the step editor; their body is unused (the chain
                  is defined by the step list). */}
              {kind === 'leaf' && (
                <div>
                  <label className="block text-[12px] font-medium text-text-secondary mb-1.5">
                    Prompt Body
                  </label>
                  <textarea
                    value={body}
                    onChange={(e) => setBody(e.target.value)}
                    placeholder="Describe what the agent should do..."
                    rows={16}
                    className="w-full px-3 py-2.5 rounded-lg border border-border-subtle bg-white/50 text-[13px] text-text-primary font-mono leading-relaxed placeholder:text-text-tertiary focus:outline-none focus:border-accent/40 focus:ring-1 focus:ring-accent/20 transition-colors resize-y"
                  />
                </div>
              )}

              <div>
                <label className="block text-[12px] font-medium text-text-secondary mb-1.5">
                  Model
                </label>
                <select
                  value={model}
                  onChange={(e) => setModel(e.target.value)}
                  className="w-full px-3 py-2 rounded-lg border border-border-subtle bg-white/50 text-[13px] text-text-primary focus:outline-none focus:border-accent/40 focus:ring-1 focus:ring-accent/20 transition-colors"
                >
                  <option value="">Default{defaultModel ? ` (${defaultModel})` : ''}</option>
                  <option value="haiku">Haiku (fast, cheap)</option>
                  <option value="sonnet">Sonnet (balanced)</option>
                  <option value="opus">Opus (most capable)</option>
                </select>
                <p className="text-[10px] text-text-tertiary mt-1.5">
                  Default tracks the model chosen in Settings — change it there and every prompt
                  using Default follows.
                </p>
              </div>

              {kind === 'chain' && (
                <ChainStepEditor
                  chainPromptId={promptId ?? ''}
                  steps={chainDraft}
                  onChange={setChainDraft}
                  busy={saving}
                />
              )}

              {/* Template variables reference — leaf prompts only.
                  Chain prompts don't reference task placeholders directly;
                  the wrapper user prompt the spawner builds carries
                  task context for each step. */}
              {kind === 'leaf' && (
                <div>
                  <label className="block text-[12px] font-medium text-text-secondary mb-1.5">
                    Template Variables
                  </label>
                  <div className="bg-black/[0.02] rounded-lg border border-border-subtle p-3 space-y-4">
                    {TEMPLATE_VAR_GROUPS.map((group) => (
                      <div key={group.label}>
                        <div className="text-[11px] font-semibold text-text-secondary">
                          {group.label}
                        </div>
                        <div className="text-[10px] text-text-tertiary mb-1.5">{group.caption}</div>
                        <div className="space-y-1">
                          {group.vars.map((v) => (
                            <div key={v.name} className="flex items-start gap-3">
                              <code className="text-[11px] font-mono text-accent bg-accent/[0.06] px-1.5 py-0.5 rounded shrink-0">
                                {v.name}
                              </code>
                              <span className="text-[11px] text-text-tertiary leading-snug">
                                {v.desc}
                              </span>
                            </div>
                          ))}
                        </div>
                      </div>
                    ))}
                    <p className="text-[10px] text-text-tertiary pt-2 border-t border-border-subtle">
                      Tool guidance and completion format are injected automatically. You only need
                      to write the mission. Placeholders that don&rsquo;t apply to the task&rsquo;s
                      event type render empty.
                    </p>
                  </div>
                </div>
              )}

              {/* Stats */}
              {!isNew && stats && stats.total_runs > 0 && (
                <div>
                  <label className="block text-[12px] font-medium text-text-secondary mb-2">
                    Performance
                  </label>
                  <div className="bg-black/[0.02] rounded-lg border border-border-subtle p-3 space-y-3">
                    {/* Stat pills */}
                    <div className="flex gap-2 flex-wrap">
                      <StatPill label="Runs" value={String(stats.total_runs)} />
                      <StatPill label="Avg cost" value={`$${stats.avg_cost_usd.toFixed(3)}`} />
                      <StatPill
                        label="Success"
                        value={`${Math.round(stats.success_rate * 100)}%`}
                        color={
                          stats.success_rate >= 0.8
                            ? 'text-claim'
                            : stats.success_rate >= 0.5
                              ? 'text-amber-600'
                              : 'text-dismiss'
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
                                    d.count > 0
                                      ? 'var(--color-accent)'
                                      : 'var(--color-border-subtle)',
                                  opacity: d.count > 0 ? 0.7 : 0.3,
                                }}
                                title={`${d.date}: ${d.count} run${d.count !== 1 ? 's' : ''}`}
                              />
                            )
                          })}
                        </div>
                        <div className="flex justify-between mt-1">
                          <span className="text-[9px] text-text-tertiary">30d ago</span>
                          <span className="text-[9px] text-text-tertiary">today</span>
                        </div>
                      </div>
                    )}

                    {/* Last used + totals */}
                    <div className="flex justify-between text-[10px] text-text-tertiary pt-1 border-t border-border-subtle">
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
                    className={`text-[9px] font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded ${source === 'system' ? 'bg-black/[0.04] text-text-tertiary' : 'bg-accent/10 text-accent'}`}
                  >
                    {source}
                  </span>
                </div>
              )}
            </div>

            {/* Footer */}
            <div className="px-6 py-4 border-t border-border-subtle flex items-center justify-between shrink-0">
              <div>
                {!isNew && (
                  <button
                    onClick={handleDelete}
                    disabled={deleting}
                    title={
                      source === 'user'
                        ? 'Permanently delete this prompt'
                        : 'Hide this prompt — it will not reappear on import'
                    }
                    className="text-[12px] text-text-tertiary hover:text-red-500 font-medium transition-colors disabled:opacity-50"
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
                {error && <span className="text-[12px] text-red-500">{error}</span>}
                <button
                  onClick={onClose}
                  className="text-[12px] text-text-tertiary hover:text-text-secondary font-medium transition-colors"
                >
                  Cancel
                </button>
                <button
                  onClick={save}
                  disabled={saving}
                  className="text-[12px] font-semibold text-white bg-accent hover:bg-accent/90 px-4 py-1.5 rounded-full transition-colors disabled:opacity-50"
                >
                  {saving ? 'Saving...' : isNew ? 'Create' : 'Save'}
                </button>
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
    <div className="bg-white/60 border border-border-subtle rounded-md px-2 py-1">
      <div className={`text-[12px] font-semibold ${color || 'text-text-primary'}`}>{value}</div>
      <div className="text-[9px] text-text-tertiary">{label}</div>
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
