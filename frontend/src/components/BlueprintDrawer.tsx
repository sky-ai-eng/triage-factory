import { useState, useEffect, useCallback, useRef } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import { toast } from './Toast/toastStore'
import { readError } from '../lib/api'
import { useTeams, noteWrittenTeam } from '../hooks/useTeams'
import { blueprintsBase } from '../lib/scope'
import type { Blueprint, BlueprintStep } from '../types'
import BlueprintStepEditor, { type BlueprintStepDraft } from './BlueprintStepEditor'
import PromptDrawer from './PromptDrawer'

interface Props {
  // The blueprint id to edit (from a binding-graph node click), or null in
  // create mode.
  blueprintId: string | null
  isNew?: boolean
  // The single-team prompts page locks new blueprints to its active team and
  // shows a read-only label instead of a picker. '' for solo/local (the server
  // resolves the sole team). Mutually exclusive with templateScope.
  lockedTeamId?: string
  // Org-template editor scope: CRUD targets /api/org-template/blueprints
  // (org-scoped, no team_id). Mutually exclusive with lockedTeamId.
  templateScope?: boolean
  onClose: () => void
  onSaved: () => void
  onDeleted?: () => void
}

const MIN_WIDTH = 380
const MAX_WIDTH = 900
const STORAGE_KEY = 'blueprint-drawer-width'

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

export default function BlueprintDrawer({
  blueprintId,
  isNew,
  lockedTeamId,
  templateScope = false,
  onClose,
  onSaved,
  onDeleted,
}: Props) {
  // REST root for this scope — /api/blueprints (team) or
  // /api/org-template/blueprints.
  const base = blueprintsBase(templateScope)
  const [name, setName] = useState('')
  const [source, setSource] = useState('user')
  const [steps, setSteps] = useState<BlueprintStepDraft[]>([])
  const [saving, setSaving] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [error, setError] = useState('')
  const [width, setWidth] = useState(loadWidth)
  const dragging = useRef(false)
  const startX = useRef(0)
  const startWidth = useRef(0)

  // Nested per-prompt editor. A step references a prompt; "Edit prompt" opens
  // the existing prompt, "+ New Prompt" (from the step picker) opens create
  // mode. stepEditorKey remounts the step editor after a save so step names
  // refresh and a freshly created prompt shows up in the picker.
  const [editPromptId, setEditPromptId] = useState<string | null>(null)
  const [creatingPrompt, setCreatingPrompt] = useState(false)
  const [stepEditorKey, setStepEditorKey] = useState(0)

  const { teams, loaded: teamsLoaded } = useTeams()

  const open = blueprintId !== null || isNew

  useEffect(() => {
    if (isNew) {
      setName('')
      setSource('user')
      setSteps([])
      setError('')
      return
    }
    if (!blueprintId) return
    let cancelled = false

    fetch(`${base}/${blueprintId}`)
      .then((res) => {
        if (!res.ok) throw new Error(`HTTP ${res.status}`)
        return res.json()
      })
      .then((data: Blueprint) => {
        if (cancelled) return
        setName(data.name)
        setSource(data.source ?? 'user')
        setError('')
      })
      .catch(() => {
        if (!cancelled) setError('Failed to load blueprint')
      })

    fetch(`${base}/${blueprintId}/steps`)
      .then((res) => {
        if (!res.ok) throw new Error(`HTTP ${res.status}`)
        return res.json()
      })
      .then((data: BlueprintStep[]) => {
        if (cancelled) return
        setSteps(data.map((s) => ({ step_prompt_id: s.step_prompt_id, brief: s.brief })))
      })
      .catch(() => {
        // Header load already surfaces a load error; steps default to empty.
      })

    return () => {
      cancelled = true
    }
  }, [blueprintId, isNew, base])

  // Resize drag handlers (mirrors PromptDrawer).
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
    const trimmed = name.trim()
    if (!trimmed) {
      setError('Name is required')
      return
    }
    // A blueprint always has ≥1 step (step 1 is the entry the trigger fires) —
    // a zero-step blueprint can't run, so the editor refuses to save one.
    if (steps.length === 0) {
      setError('A blueprint needs at least one step')
      return
    }
    if (steps.some((s) => !s.step_prompt_id)) {
      setError('Every step needs a prompt')
      return
    }
    setSaving(true)
    setError('')

    const stepsBody = {
      steps: steps.map((s) => ({ step_prompt_id: s.step_prompt_id, brief: s.brief })),
    }

    try {
      // Two-phase save: the header (POST create / PUT rename) resolves the id,
      // then the ordered step list is replaced via the dedicated steps PUT.
      let id = blueprintId
      if (isNew) {
        const createBody = templateScope
          ? { name: trimmed }
          : { name: trimmed, team_id: lockedTeamId ?? '' }
        const res = await fetch(base, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(createBody),
        })
        if (!res.ok) {
          toast.error(await readError(res, 'Failed to create blueprint'))
          return
        }
        const created = (await res.json()) as Blueprint
        id = created.id
      } else {
        const res = await fetch(`${base}/${blueprintId}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name: trimmed }),
        })
        if (!res.ok) {
          toast.error(await readError(res, 'Failed to save blueprint'))
          return
        }
      }

      const stepsRes = await fetch(`${base}/${id}/steps`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(stepsBody),
      })
      if (!stepsRes.ok) {
        toast.error(await readError(stepsRes, 'Failed to save blueprint steps'))
        return
      }

      if (!templateScope && isNew && lockedTeamId) noteWrittenTeam(lockedTeamId)
      onSaved()
    } catch (err) {
      toast.error(`Failed to save blueprint: ${(err as Error).message}`)
    } finally {
      setSaving(false)
    }
  }

  const handleDelete = async () => {
    if (!blueprintId) return
    setDeleting(true)
    try {
      const res = await fetch(`${base}/${blueprintId}`, { method: 'DELETE' })
      if (!res.ok) {
        toast.error(await readError(res, 'Failed to delete blueprint'))
        return
      }
      onDeleted?.()
    } catch (err) {
      toast.error(`Failed to delete blueprint: ${(err as Error).message}`)
    } finally {
      setDeleting(false)
    }
  }

  // Refresh step names + the picker's prompt list after the nested per-prompt
  // editor saves (a renamed/created prompt should show through immediately).
  const onNestedPromptSaved = () => {
    setEditPromptId(null)
    setCreatingPrompt(false)
    setStepEditorKey((k) => k + 1)
  }
  const closeNestedPrompt = () => {
    setEditPromptId(null)
    setCreatingPrompt(false)
  }

  const teamLabel = lockedTeamId
    ? (teams.find((t) => t.id === lockedTeamId)?.name ?? 'current team')
    : 'current team'

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
              <h2 className="text-[15px] font-semibold text-text-primary">
                {isNew ? 'New Blueprint' : 'Edit Blueprint'}
              </h2>
              <button
                onClick={onClose}
                className="text-text-tertiary hover:text-text-secondary transition-colors text-lg leading-none px-1"
              >
                &times;
              </button>
            </div>

            {/* Body — scrollable */}
            <div className="flex-1 overflow-y-auto px-6 py-5 space-y-5">
              {/* Scope line */}
              {templateScope ? (
                <div className="text-[12px] text-text-tertiary">
                  Scope: <span className="font-medium text-text-secondary">Org template</span>
                </div>
              ) : (
                isNew && (
                  <div className="text-[12px] text-text-tertiary">
                    Team: <span className="font-medium text-text-secondary">{teamLabel}</span>
                  </div>
                )
              )}

              {/* Name */}
              <div>
                <label className="block text-[12px] font-medium text-text-secondary mb-1.5">
                  Name
                </label>
                <input
                  type="text"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="e.g. Review & Fix"
                  className="w-full px-3 py-2 rounded-lg border border-border-subtle bg-white/50 text-[13px] text-text-primary placeholder:text-text-tertiary focus:outline-none focus:border-accent/40 focus:ring-1 focus:ring-accent/20 transition-colors"
                />
              </div>

              <p className="text-[11px] text-text-tertiary leading-relaxed">
                A blueprint is an ordered list of prompt steps — what a trigger fires. A single-step
                blueprint runs exactly like a single prompt; add steps to hand off between agents,
                each with its own context window over a shared workspace.
              </p>

              {/* Ordered steps */}
              <BlueprintStepEditor
                key={stepEditorKey}
                steps={steps}
                onChange={setSteps}
                busy={saving}
                lockedTeamId={lockedTeamId}
                templateScope={templateScope}
                onEditPrompt={(pid) => setEditPromptId(pid)}
                onNewPrompt={() => setCreatingPrompt(true)}
              />

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
                    title="Delete this blueprint — its steps and any triggers bound to it are removed"
                    className="text-[12px] text-text-tertiary hover:text-red-500 font-medium transition-colors disabled:opacity-50"
                  >
                    {deleting ? 'Deleting...' : 'Delete'}
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
                  disabled={saving || (isNew && !templateScope && !lockedTeamId && !teamsLoaded)}
                  className="text-[12px] font-semibold text-white bg-accent hover:bg-accent/90 px-4 py-1.5 rounded-full transition-colors disabled:opacity-50"
                >
                  {saving ? 'Saving...' : isNew ? 'Create' : 'Save'}
                </button>
              </div>
            </div>
          </motion.div>

          {/* Nested per-prompt editor — reached from a step's "Edit prompt" or
              the step picker's "+ New Prompt". Renders above this drawer. */}
          <PromptDrawer
            promptId={editPromptId}
            isNew={creatingPrompt}
            lockedTeamId={templateScope ? undefined : lockedTeamId}
            templateScope={templateScope}
            onClose={closeNestedPrompt}
            onSaved={onNestedPromptSaved}
            onDeleted={onNestedPromptSaved}
          />
        </>
      )}
    </AnimatePresence>
  )
}
