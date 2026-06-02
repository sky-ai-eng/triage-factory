import { useEffect, useState } from 'react'
import type { Prompt } from '../types'
import PromptPicker from './PromptPicker'

interface Props {
  // The chain prompt's own id, so the picker can hide it from the
  // selectable set (a chain can't reference itself as a step).
  chainPromptId: string
  // Controlled step list. The parent owns the working draft so save
  // can PUT /api/prompts/{id}/chain-steps atomically with the prompt
  // PUT, and so resyncing from a refetch doesn't require an effect-
  // body setState in this component (the linter rightly objects).
  steps: ChainStepDraft[]
  onChange: (draft: ChainStepDraft[]) => void
  // True when the parent is currently saving — disables drag/drop and
  // the picker so the working state can't change mid-PUT.
  busy?: boolean
  // The active team on the single-team prompts page. Scopes both the
  // name-lookup fetch and the step picker so a chain on team A can only
  // reference team A's (and org-visible) leaf prompts — otherwise a
  // multi-team user could add a team-B step to a team-A chain, and the
  // chain would run another team's prompt. '' for solo/local (the server
  // resolves the sole team); the chain prompt itself is stamped to this
  // same team by PromptDrawer, so the steps must match.
  lockedTeamId?: string
}

export interface ChainStepDraft {
  step_prompt_id: string
  brief: string
}

export default function ChainStepEditor({
  chainPromptId,
  steps,
  onChange,
  busy,
  lockedTeamId,
}: Props) {
  const [allPrompts, setAllPrompts] = useState<Prompt[]>([])
  const [pickerOpen, setPickerOpen] = useState(false)
  const [dragIndex, setDragIndex] = useState<number | null>(null)

  // Cache the prompts list so we can show step names. The picker
  // re-fetches on its own (already a low-volume call), so this fetch
  // is purely for the per-step name lookup. Scoped to the locked team so
  // a step's name resolves only within the chain's own team set (matches
  // the picker's options); refetches if the team changes.
  useEffect(() => {
    let cancelled = false
    const q = lockedTeamId ? `?team_id=${encodeURIComponent(lockedTeamId)}` : ''
    fetch(`/api/prompts${q}`)
      .then((res) => res.json())
      .then((data: Prompt[]) => {
        if (!cancelled) setAllPrompts(data)
      })
      .catch(() => {})
    return () => {
      cancelled = true
    }
  }, [pickerOpen, lockedTeamId])

  const update = (next: ChainStepDraft[]) => onChange(next)

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
          No steps yet. Add a prompt to start the chain.
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
        title="Add a chain step"
        subtitle="Pick a leaf prompt to run as the next step in this chain"
        filter={(p) => p.id !== chainPromptId}
        // Scope the picker's options to the chain's team. teamValue without
        // onTeamChange scopes the fetch without rendering a header team
        // picker — the team is fixed by the page, not chosen here.
        teamValue={lockedTeamId}
      />
    </div>
  )
}
