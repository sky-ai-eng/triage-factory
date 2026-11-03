import { useEffect, useState } from 'react'
import type { Prompt } from '../types'
import PromptPicker from './PromptPicker'
import { apiJSON } from '../lib/apiClient'

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
  // The active team on the single-team prompts page. Scopes the step
  // picker so a chain on team A can only
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
  const [names, setNames] = useState<Record<string, Prompt | null>>({})
  const [pickerOpen, setPickerOpen] = useState(false)
  const [dragIndex, setDragIndex] = useState<number | null>(null)

  // Each step's prompt, read by its id, for the name and body the step row
  // shows. The steps are the chain's own handful, so a read per id is the
  // whole cost; the picker reads its own options with search.
  const missing = steps
    .map((s) => s.step_prompt_id)
    .filter((id, i, all) => id && all.indexOf(id) === i && !(id in names))
    .join(',')
  useEffect(() => {
    if (!missing) return
    let cancelled = false
    const ids = missing.split(',')
    void Promise.all(
      ids.map((id) =>
        apiJSON<Prompt>(`/api/prompts/${encodeURIComponent(id)}`)
          .then((p) => [id, p] as const)
          .catch(() => [id, null] as const),
      ),
    ).then((pairs) => {
      if (!cancelled) setNames((prev) => ({ ...prev, ...Object.fromEntries(pairs) }))
    })
    return () => {
      cancelled = true
    }
  }, [missing])

  const update = (next: ChainStepDraft[]) => onChange(next)

  const promptById = (id: string) => names[id] ?? undefined

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
      <label className="block text-ui font-medium text-ink-2">Steps</label>
      {steps.length === 0 && (
        <div className="text-ui text-ink-3 border border-dashed border-line-1 rounded-lg px-3 py-4 text-center">
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
              className={`group flex items-start gap-3 rounded-lg border bg-raised px-3 py-2.5 transition-colors ${
                dragIndex === i
                  ? 'border-warm/40 ring-1 ring-warm/30'
                  : 'border-line-1 hover:border-line-2'
              } ${busy ? 'opacity-60' : ''}`}
            >
              <div className="cursor-grab text-ink-3 text-body leading-none mt-1 select-none">
                ⋮⋮
              </div>
              <div className="flex-1 min-w-0">
                <div className="flex items-center gap-2">
                  <span className="text-label font-semibold uppercase tracking-wider text-ink-3">
                    Step {i + 1}
                  </span>
                  <span className="text-body font-medium text-ink-1 truncate">
                    {prompt ? prompt.name : '(missing prompt)'}
                  </span>
                  {prompt && (
                    <span className="text-label-sm uppercase font-semibold tracking-wider text-ink-3 bg-tint-3 px-1.5 py-0.5 rounded">
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
                  className="mt-1.5 w-full px-2 py-1 rounded border border-line-1 bg-raised text-ui text-ink-2 placeholder:text-ink-3 focus:outline-none focus:border-warm/40 focus:ring-1 focus:ring-warm/20"
                />
              </div>
              <button
                onClick={() => update(steps.filter((_, j) => j !== i))}
                disabled={busy}
                className="text-ink-3 hover:text-alarm text-body px-1 leading-none transition-colors"
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
        className="text-ui font-medium text-warm hover:text-warm/80 transition-colors disabled:opacity-50"
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
        selectLabel="Add step"
        filter={(p) => p.id !== chainPromptId}
        // Scope the picker's options to the chain's team. teamValue without
        // onTeamChange scopes the fetch without rendering a header team
        // picker — the team is fixed by the page, not chosen here.
        teamValue={lockedTeamId}
      />
    </div>
  )
}
