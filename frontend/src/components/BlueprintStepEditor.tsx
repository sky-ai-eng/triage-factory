import { useEffect, useState } from 'react'
import type { Prompt } from '../types'
import PromptPicker from './PromptPicker'
import { promptsBase } from '../lib/scope'

interface Props {
  // Controlled step list. The parent (BlueprintDrawer) owns the working draft
  // so save can PUT /api/blueprints/{id}/steps atomically alongside the
  // header PUT, and so resyncing from a refetch doesn't require an effect-body
  // setState here (the linter rightly objects).
  steps: BlueprintStepDraft[]
  onChange: (draft: BlueprintStepDraft[]) => void
  // True while the parent is saving — disables drag/drop and the picker so the
  // working state can't change mid-PUT.
  busy?: boolean
  // Team scope: the active team. Scopes both the name-lookup fetch and the
  // step picker so a team-A blueprint can only reference team-A's (and
  // org-visible) prompts — the backend enforces the same-team guard on the
  // steps PUT, so the options must match. '' for solo/local (the server
  // resolves the sole team). Ignored when templateScope is set.
  lockedTeamId?: string
  // Template scope (org-template editor): steps reference org-template prompts
  // (/api/org-template/prompts), which are org-scoped — no team_id.
  templateScope?: boolean
  // Opens the per-prompt editor for an existing step's prompt (edit
  // body/model/tools). The blueprint editor owns the drawer; this just asks it
  // to open against a prompt id.
  onEditPrompt?: (promptId: string) => void
  // Creates a brand-new prompt to use as a step — opens the per-prompt editor
  // in create mode. Wired to the picker's "+ New Prompt" affordance so an
  // author with no prompts yet can still build their first step.
  onNewPrompt?: () => void
}

export interface BlueprintStepDraft {
  step_prompt_id: string
  brief: string
}

export default function BlueprintStepEditor({
  steps,
  onChange,
  busy,
  lockedTeamId,
  templateScope = false,
  onEditPrompt,
  onNewPrompt,
}: Props) {
  const [allPrompts, setAllPrompts] = useState<Prompt[]>([])
  const [pickerOpen, setPickerOpen] = useState(false)
  const [dragIndex, setDragIndex] = useState<number | null>(null)

  // Cache the prompt list for per-step name lookup. The picker re-fetches on
  // its own; this fetch is purely so a step row can show its prompt's name.
  // Scoped to match the picker's option set (locked team, or org-template) so
  // a step's name resolves within the same set the user picked from.
  useEffect(() => {
    let cancelled = false
    const base = promptsBase(templateScope)
    const q = !templateScope && lockedTeamId ? `?team_id=${encodeURIComponent(lockedTeamId)}` : ''
    fetch(`${base}${q}`)
      .then((res) => res.json())
      .then((data: Prompt[]) => {
        if (!cancelled) setAllPrompts(data)
      })
      .catch(() => {})
    return () => {
      cancelled = true
    }
  }, [pickerOpen, lockedTeamId, templateScope])

  const update = (next: BlueprintStepDraft[]) => onChange(next)

  const promptById = (id: string) => allPrompts.find((p) => p.id === id)

  const onDragStart = (i: number) => () => setDragIndex(i)
  const onDragOver = (i: number) => (e: React.DragEvent) => {
    e.preventDefault()
    if (dragIndex === null || dragIndex === i) return
    const next = [...steps]
    const [moved] = next.splice(dragIndex, 1)
    next.splice(i, 0, moved)
    setDragIndex(i)
    update(next)
  }
  const onDragEnd = () => setDragIndex(null)

  return (
    <div className="space-y-3">
      <label className="block text-[12px] font-medium text-text-secondary">Steps</label>
      {steps.length === 0 && (
        <div className="text-[12px] text-text-tertiary border border-dashed border-border-subtle rounded-lg px-3 py-4 text-center">
          A blueprint needs at least one step — step&nbsp;1 is the entry the trigger fires. Add a
          prompt to begin.
        </div>
      )}

      <ol className="space-y-2">
        {steps.map((step, i) => {
          const prompt = promptById(step.step_prompt_id)
          return (
            <li
              key={i}
              draggable={!busy}
              onDragStart={onDragStart(i)}
              onDragOver={onDragOver(i)}
              onDragEnd={onDragEnd}
              className={`group flex items-start gap-3 rounded-lg border bg-white/60 px-3 py-2.5 transition-colors ${
                dragIndex === i
                  ? 'border-accent/40 ring-1 ring-accent/30'
                  : 'border-border-subtle hover:border-border-glass'
              } ${busy ? 'opacity-60' : ''}`}
            >
              <div className="cursor-grab text-text-tertiary text-[14px] leading-none mt-1 select-none">
                ⋮⋮
              </div>
              <div className="flex-1 min-w-0">
                <div className="flex items-center gap-2">
                  <span className="text-[10px] font-semibold uppercase tracking-wider text-text-tertiary">
                    Step {i + 1}
                  </span>
                  <span className="text-[13px] font-medium text-text-primary truncate">
                    {prompt ? prompt.name : '(missing prompt)'}
                  </span>
                  {prompt && (
                    <span className="text-[9px] uppercase font-semibold tracking-wider text-text-tertiary bg-black/[0.04] px-1.5 py-0.5 rounded">
                      {prompt.source}
                    </span>
                  )}
                  {onEditPrompt && (
                    <button
                      type="button"
                      onClick={() => onEditPrompt(step.step_prompt_id)}
                      disabled={busy}
                      className="ml-auto text-[11px] font-medium text-accent hover:text-accent/80 transition-colors disabled:opacity-50"
                      title="Edit this step's prompt (body, model, tools)"
                    >
                      Edit prompt
                    </button>
                  )}
                </div>
                <input
                  type="text"
                  value={step.brief}
                  disabled={busy}
                  onChange={(e) => {
                    const next = [...steps]
                    next[i] = { ...next[i], brief: e.target.value }
                    update(next)
                  }}
                  placeholder="Optional one-line brief shown to the agent"
                  className="mt-1.5 w-full px-2 py-1 rounded border border-border-subtle bg-white/70 text-[12px] text-text-secondary placeholder:text-text-tertiary focus:outline-none focus:border-accent/40 focus:ring-1 focus:ring-accent/20"
                />
              </div>
              <button
                onClick={() => update(steps.filter((_, j) => j !== i))}
                disabled={busy}
                className="text-text-tertiary hover:text-red-500 text-[14px] px-1 leading-none transition-colors"
                title="Remove step"
              >
                &times;
              </button>
            </li>
          )
        })}
      </ol>

      <button
        onClick={() => setPickerOpen(true)}
        disabled={busy}
        className="text-[12px] font-medium text-accent hover:text-accent/80 transition-colors disabled:opacity-50"
      >
        + Add step
      </button>

      <PromptPicker
        open={pickerOpen}
        onSelect={(promptID: string) => {
          update([...steps, { step_prompt_id: promptID, brief: '' }])
          setPickerOpen(false)
        }}
        onClose={() => setPickerOpen(false)}
        title="Add a blueprint step"
        subtitle="Pick a prompt to run as the next step in this blueprint"
        // Scope the picker to the blueprint's team (team scope) or the
        // org-template prompts (template scope). teamValue without onTeamChange
        // scopes the fetch without rendering a header team picker — the team is
        // fixed by the page, not chosen here.
        teamValue={templateScope ? undefined : lockedTeamId}
        templateScope={templateScope}
        // "+ New Prompt" opens the per-prompt editor in create mode so a new
        // prompt can be authored without leaving the blueprint flow. Close the
        // picker first so the per-prompt drawer doesn't stack over it.
        onEditPrompts={
          onNewPrompt
            ? () => {
                setPickerOpen(false)
                onNewPrompt()
              }
            : undefined
        }
      />
    </div>
  )
}
