import { useState, useEffect, useMemo, useCallback, useId, useRef } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import { Layers, RotateCw } from 'lucide-react'
import type { Blueprint, BlueprintStep, Prompt } from '../types'
import { useFocusTrap } from '../hooks/useFocusTrap'
import { apiListAll, httpErrorMessage } from '../lib/apiClient'
import TeamPicker from './TeamPicker'

interface Props {
  open: boolean
  onSelect: (promptId: string) => void
  onClose: () => void
  onEditPrompts?: () => void
  /** Optional override for the modal title. Defaults to "Choose a prompt"
   *  for backward compatibility with the delegation-strategy callers. */
  title?: string
  /** Optional override for the subtitle line under the title. */
  subtitle?: string
  /** Label for the primary commit button. The picker is browse-first: a click
   *  selects a row and fills the detail pane; this button (or double-click /
   *  Enter) commits the selection via onSelect. Defaults to "Select"; callers
   *  pass their verb ("Run", "Add step", "Use skill"). */
  selectLabel?: string
  /** When set, the matching row renders with an "in use" badge so users coming
   *  back to a settings-style picker see what's currently in effect, and it is
   *  the row auto-selected on open. The delegation-strategy callers leave this
   *  undefined because their picker fires once per task with no prior selection. */
  selectedId?: string
  /** Optional client-side filter applied after the list is fetched. Use this to
   *  hide items that aren't valid in the calling context — e.g. the chain step
   *  editor passes a filter that hides the chain itself. Operates on the Prompt
   *  shape; picker items extend it, so id/name/source filters work unchanged. */
  filter?: (p: Prompt) => boolean
  /** When the picker drives a *create* delegate (the factory
   *  drag-to-station flow synthesizes a task), wiring onTeamChange renders
   *  a required team picker in the header so the caller can stamp the
   *  acting team, and teamValue scopes the list to that team. The
   *  swipe-delegate callers (claiming an existing task, whose team is
   *  derived from the claim) leave both undefined → no picker, unscoped list. */
  teamValue?: string
  onTeamChange?: (teamId: string) => void
  /** Which list the picker draws from. Default 'prompts' is the leaf-prompt
   *  picker used by the chain step editor; onSelect yields a prompt_id and
   *  the detail pane shows the prompt body. The
   *  delegation callers pass 'blueprints' so the user picks the blueprint a
   *  manual delegate fires; onSelect yields a blueprint_id and the detail pane
   *  shows the blueprint's step composition (or its sole step's body when it is
   *  a 1-step blueprint). */
  source?: 'prompts' | 'blueprints'
  /** When true, the commit button is disabled and the list dimmed. The factory
   *  create-delegate caller sets this until /api/teams has loaded so a
   *  selection in the cold-load window can't post a blank/unresolved acting
   *  team. */
  selectionDisabled?: boolean
}

// One resolved step of a blueprint, for the detail pane. step_prompt_id is
// joined against the prompts list (fetched alongside) to recover the prompt's
// name + model; brief rides on the step row itself (often empty — the shipped
// pr-review blueprint ships no briefs, so the pane reads on the name alone).
interface PickerStep {
  index: number
  name: string
  model?: string
  brief?: string
}

// PickerItem is the unified row/detail model. It extends Prompt so the caller's
// `filter: (p: Prompt) => boolean` keeps working, and adds the blueprint step
// composition. `steps` is undefined for a leaf prompt; for a blueprint it is the
// ordered step list (length ≥ 1). The pane shows the step composition when there
// are ≥2 steps, otherwise the body (a leaf prompt's own, or a 1-step
// blueprint's sole step body).
interface PickerItem extends Prompt {
  steps?: PickerStep[]
}

function isMultiStep(it: PickerItem): boolean {
  return !!it.steps && it.steps.length > 1
}

// metaLine is the detail pane's mono readout under the name — step count (for a
// multi-step blueprint), source, and usage when non-zero.
function metaLine(it: PickerItem): string {
  const parts: string[] = []
  if (it.steps && it.steps.length > 1) parts.push(`${it.steps.length} steps`)
  parts.push(it.source || 'user')
  if (it.usage_count > 0) parts.push(`used ${it.usage_count}×`)
  return parts.join(' · ')
}

export default function PromptPicker({
  open,
  onSelect,
  onClose,
  onEditPrompts,
  title = 'Choose a prompt',
  subtitle = 'Select a delegation strategy for this task',
  selectLabel = 'Select',
  selectedId,
  filter,
  teamValue,
  onTeamChange,
  source = 'prompts',
  selectionDisabled,
}: Props) {
  const [items, setItems] = useState<PickerItem[]>([])
  // Why the last load failed, or null when it hasn't. A failed fetch and an
  // empty account both leave the picker with no rows, and only one of them
  // means the user should go make something — so the failure carries its
  // reason rather than collapsing into the same silence.
  const [loadError, setLoadError] = useState<string | null>(null)
  // Whether the current scope's read has answered. This, not the row count,
  // is what ends the skeleton: a read that succeeds with zero rows is an
  // answer — "you have none" — and has to render as one.
  const [loaded, setLoaded] = useState(false)
  // Bumped by the error pane's Retry button to re-fire the fetch effect.
  const [retryKey, setRetryKey] = useState(0)
  const [query, setQuery] = useState('')
  // The highlighted row. The picker is browse-first: this is the previewed
  // item, NOT a commit — onSelect fires only from the Run button / Enter /
  // double-click. null when nothing is selectable yet.
  const [selId, setSelId] = useState<string | null>(null)
  // Per-row refs so ↑/↓ can keep the active row in view, and a stable id base
  // for the combobox/listbox ARIA wiring on the filter input + rows below.
  const rowRefs = useRef(new Map<string, HTMLButtonElement>())
  const listboxId = useId()
  const optionId = (id: string) => `${listboxId}-opt-${id}`

  // Trap keyboard focus inside the dialog while open and restore it to the
  // trigger on close (WCAG 2.1.2); initial focus lands on the filter input.
  const dialogRef = useRef<HTMLDivElement>(null)
  const filterRef = useRef<HTMLInputElement>(null)
  useFocusTrap(dialogRef, { active: open, initialFocus: filterRef })

  // Derived: "loading" means open with a read still outstanding — neither
  // answered nor failed. Both flags survive a close, so a reopen shows the
  // cached rows instantly (intentional); only a team switch, which drops the
  // rows, and Retry put us back in the skeleton.
  const loading = open && !loaded && loadError === null

  // The read failed AND left nothing to show — the only case that should claim
  // a failure. A failed refetch that still has cached rows keeps rendering
  // them, so the no-match copy stays correct for a filter that hides them all.
  const failedEmpty = loadError !== null && items.length === 0

  // The word the empty and error copy uses for whatever this picker lists.
  const noun = source === 'blueprints' ? 'blueprints' : 'prompts'

  // Reset the type-to-filter query each time the picker opens so a stale filter
  // from a prior open doesn't hide everything on reopen.
  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect
    if (open) setQuery('')
  }, [open])

  useEffect(() => {
    if (!open) return
    let cancelled = false
    // Scope the list to the acting team when the caller drives one. Empty/absent
    // teamValue → unscoped (a solo user's sole team, or the swipe / chain
    // callers that don't scope). Refetching on teamValue keeps rows matched
    // to the header's team so a user can't preview another team's item.
    const teamFilter = teamValue ? { team_id: teamValue } : {}
    // Clear the prior failure so this attempt (first open, reopen, team switch,
    // Retry) shows the loading skeleton again rather than getting stuck on the
    // error message while the refetch is in flight; the .catch below re-raises
    // it if this attempt also fails.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setLoadError(null)
    // On a team switch, drop the prior team's rows so the skeleton shows during
    // the refetch and a stale cross-team item can't be committed. Only when
    // team-scoped — unscoped callers keep their across-opens cache.
    if (teamValue !== undefined) {
      setItems([])
      setLoaded(false)
    }

    const load = async (): Promise<PickerItem[]> => {
      // The 'prompts' source is already a flat list carrying bodies + models.
      if (source !== 'blueprints') {
        const data = await apiListAll<Prompt>('/api/prompts/list', teamFilter)
        return data.map((p) => ({ ...p }))
      }

      // A blueprint has no body of its own. Pull the blueprint list, the prompt
      // list (for step names + bodies + models), and ALL steps for the scope in
      // ONE bulk read (grouped client-side by blueprint_id) — replacing the
      // former per-blueprint N+1. A failed steps fetch degrades to empty
      // compositions rather than breaking the picker.
      const [blueprints, promptList, allSteps] = await Promise.all([
        apiListAll<Blueprint>('/api/blueprints/list', teamFilter),
        apiListAll<Prompt>('/api/prompts/list', teamFilter),
        apiListAll<BlueprintStep>('/api/blueprint-steps/list', teamFilter).catch(
          () => [] as BlueprintStep[],
        ),
      ])
      const promptById = new Map(promptList.map((p): [string, Prompt] => [p.id, p]))
      const stepsByBlueprint = new Map<string, BlueprintStep[]>()
      for (const st of allSteps) {
        const arr = stepsByBlueprint.get(st.blueprint_id)
        if (arr) arr.push(st)
        else stepsByBlueprint.set(st.blueprint_id, [st])
      }

      const normalized: PickerItem[] = blueprints.map((b) => {
        const ordered = [...(stepsByBlueprint.get(b.id) ?? [])].sort(
          (x, y) => x.step_index - y.step_index,
        )
        const steps: PickerStep[] = ordered.map((s, i) => {
          const p = promptById.get(s.step_prompt_id)
          return {
            index: i + 1,
            name: p?.name ?? '(missing prompt)',
            model: p?.model,
            brief: s.brief,
          }
        })
        // A 1-step blueprint borrows its sole step's prompt body for the pane,
        // so it reads like that one prompt (its pre-blueprint behavior).
        const sole = ordered.length === 1 ? promptById.get(ordered[0].step_prompt_id) : undefined
        return {
          id: b.id,
          name: b.name,
          body: sole?.body ?? '',
          source: b.source ?? 'user',
          model: sole?.model,
          usage_count: b.usage_count ?? 0,
          created_at: b.created_at,
          updated_at: b.updated_at,
          steps,
        }
      })
      return normalized
    }

    load()
      .then((rows) => {
        if (cancelled) return
        setItems(rows)
        setLoaded(true)
      })
      .catch((e) => {
        if (!cancelled) setLoadError(httpErrorMessage(e, 'The request failed.'))
      })

    return () => {
      cancelled = true
    }
  }, [open, teamValue, source, retryKey])

  // The visible rows: caller filter (e.g. hide the chain itself), then the
  // type-to-filter query against the name.
  const visible = useMemo(() => {
    let list = filter ? items.filter(filter) : items
    const q = query.trim().toLowerCase()
    if (q) list = list.filter((it) => it.name.toLowerCase().includes(q))
    return list
  }, [items, filter, query])

  // Keep a valid selection: hold the current row if it's still visible;
  // otherwise prefer the caller's selectedId (settings-style "currently in
  // effect"), else the first row — so the detail pane is never empty.
  useEffect(() => {
    if (!open) return
    // Hold the current row while it's still visible.
    if (selId && visible.some((it) => it.id === selId)) return
    // Otherwise reconcile: the caller's selectedId when present + visible, else
    // the first row, else nothing (empty list) — so the pane is never stale.
    const next =
      visible.length === 0
        ? null
        : selectedId && visible.some((it) => it.id === selectedId)
          ? selectedId
          : visible[0].id
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setSelId(next)
  }, [open, visible, selId, selectedId])

  // Keep the highlighted row visible when ↑/↓ moves the selection past the
  // fold. block:'nearest' is a no-op when the row is already in view (a click).
  useEffect(() => {
    if (selId) rowRefs.current.get(selId)?.scrollIntoView({ block: 'nearest' })
  }, [selId])

  const selected = useMemo(() => visible.find((it) => it.id === selId) ?? null, [visible, selId])

  const commit = useCallback(
    (id?: string) => {
      if (selectionDisabled) return
      const target = id ?? selId
      if (target) onSelect(target)
    },
    [selectionDisabled, selId, onSelect],
  )

  // Keyboard: ↑/↓ move the highlight, Enter commits, Esc closes. Bound on the
  // panel so it works whether focus sits in the filter input or the list.
  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Escape') {
      e.preventDefault()
      onClose()
      return
    }
    if (visible.length === 0) return
    const idx = Math.max(
      0,
      visible.findIndex((it) => it.id === selId),
    )
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setSelId(visible[Math.min(visible.length - 1, idx + 1)].id)
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      setSelId(visible[Math.max(0, idx - 1)].id)
    } else if (e.key === 'Enter') {
      e.preventDefault()
      commit()
    }
  }

  return (
    <AnimatePresence>
      {open && (
        <>
          {/* Backdrop */}
          <motion.div
            className="fixed inset-0 bg-scrim backdrop-blur-sm z-50"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            onClick={onClose}
          />

          {/* Picker panel */}
          <motion.div
            className="fixed inset-0 z-50 flex items-center justify-center pointer-events-none"
            initial={{ opacity: 0, scale: 0.95 }}
            animate={{ opacity: 1, scale: 1 }}
            exit={{ opacity: 0, scale: 0.95 }}
            transition={{ duration: 0.15 }}
          >
            <div
              ref={dialogRef}
              tabIndex={-1}
              role="dialog"
              aria-modal="true"
              aria-label={title}
              onKeyDown={onKeyDown}
              className="pointer-events-auto bg-raised/95 backdrop-blur-2xl border border-line-1 rounded-2xl shadow-float shadow-black/10 w-[760px] max-w-[calc(100vw-2rem)] h-[460px] max-h-[calc(100vh-4rem)] flex flex-col overflow-hidden"
            >
              {/* Header */}
              <div className="px-5 pt-5 pb-3 flex items-start justify-between gap-3 shrink-0 border-b border-line-1">
                <div className="min-w-0">
                  <h2 className="text-column font-semibold text-ink-1 truncate">{title}</h2>
                  <p className="text-ui text-ink-3 mt-0.5">{subtitle}</p>
                </div>
                <div className="flex items-center gap-2 shrink-0">
                  {onTeamChange && (
                    <TeamPicker
                      value={teamValue ?? ''}
                      onChange={onTeamChange}
                      label="Team"
                      className="w-40"
                    />
                  )}
                  <button
                    onClick={onClose}
                    aria-label="Close"
                    className="text-ink-3 hover:text-ink-2 transition-colors text-lg leading-none px-1"
                  >
                    &times;
                  </button>
                </div>
              </div>

              {/* Body — master list + detail pane */}
              <div className="flex flex-1 min-h-0">
                {/* List rail */}
                <div className="flex w-[244px] shrink-0 flex-col border-r border-line-1">
                  <div className="shrink-0 px-3 py-2.5 border-b border-line-1">
                    <input
                      ref={filterRef}
                      type="text"
                      value={query}
                      onChange={(e) => setQuery(e.target.value)}
                      placeholder="Filter…"
                      // Combobox over the listbox below: the input keeps focus
                      // while ↑/↓ moves the active option (aria-activedescendant),
                      // so AT announces the highlighted row without focus leaving
                      // the field.
                      role="combobox"
                      aria-expanded={visible.length > 0}
                      aria-controls={listboxId}
                      aria-activedescendant={selId ? optionId(selId) : undefined}
                      aria-autocomplete="list"
                      className="w-full px-2.5 py-1.5 rounded-lg border border-line-1 bg-raised text-ui text-ink-1 placeholder:text-ink-3 focus:outline-none focus:border-warm/40 focus:ring-1 focus:ring-warm/20 transition-colors"
                    />
                  </div>
                  <div
                    id={listboxId}
                    role="listbox"
                    aria-label={title}
                    className={`flex-1 overflow-y-auto px-2 py-2 space-y-1 ${
                      selectionDisabled ? 'opacity-50' : ''
                    }`}
                    aria-busy={loading}
                  >
                    {loading ? (
                      [...Array(6)].map((_, i) => (
                        <div key={i} className="h-9 rounded-lg bg-tint-2 animate-pulse" />
                      ))
                    ) : visible.length === 0 ? (
                      <p className="px-2 py-6 text-center text-ui text-ink-3">
                        {failedEmpty
                          ? 'Failed to load.'
                          : query
                            ? 'No matches.'
                            : 'Nothing here yet.'}
                      </p>
                    ) : (
                      visible.map((it) => (
                        <button
                          key={it.id}
                          ref={(el) => {
                            if (el) rowRefs.current.set(it.id, el)
                            else rowRefs.current.delete(it.id)
                          }}
                          id={optionId(it.id)}
                          role="option"
                          aria-selected={selId === it.id}
                          onClick={() => setSelId(it.id)}
                          onDoubleClick={() => commit(it.id)}
                          className={`group flex w-full items-center gap-2 rounded-lg border px-2.5 py-2 text-left transition-all duration-150 ${
                            selId === it.id
                              ? 'border-warm/60 bg-warm/[0.06] ring-1 ring-warm/20'
                              : 'border-transparent hover:bg-tint-2'
                          }`}
                        >
                          <span className="min-w-0 flex-1 truncate text-body font-medium text-ink-1">
                            {it.name}
                          </span>
                          {it.steps && it.steps.length > 1 && (
                            <span
                              className="inline-flex shrink-0 items-center gap-0.5 text-label font-semibold tabular-nums text-ink-3"
                              title={`${it.steps.length} steps`}
                            >
                              <Layers size={10} />
                              {it.steps.length}
                            </span>
                          )}
                          {it.id === selectedId && (
                            <span
                              aria-hidden
                              className="h-1.5 w-1.5 shrink-0 rounded-full bg-warm"
                              title="In use"
                            />
                          )}
                        </button>
                      ))
                    )}
                  </div>
                </div>

                {/* Detail pane */}
                <div className="flex flex-1 min-w-0 flex-col">
                  {loading ? (
                    <div className="flex-1 px-5 py-4 space-y-3">
                      <div className="h-5 w-40 rounded bg-tint-3 animate-pulse" />
                      <div className="h-3 w-24 rounded bg-tint-2 animate-pulse" />
                      <div className="h-24 rounded-lg bg-tint-2 animate-pulse" />
                    </div>
                  ) : selected ? (
                    <div className="flex-1 overflow-y-auto px-5 py-4">
                      <div className="flex items-center gap-2">
                        <h3 className="text-column font-semibold text-ink-1">{selected.name}</h3>
                        {selected.id === selectedId && (
                          <span className="text-label-sm font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded bg-warm/10 text-warm shrink-0">
                            In use
                          </span>
                        )}
                      </div>
                      <div className="mt-1 font-mono text-label uppercase tracking-[0.1em] text-ink-3">
                        {metaLine(selected)}
                      </div>

                      {isMultiStep(selected) ? (
                        <>
                          {/* Chain echo — a segmented bar that rhymes with the
                              board card / run station ChainTrack, so the picker
                              previews the shape of the run it will spawn. */}
                          <div className="mt-3 flex items-center gap-1.5">
                            {selected.steps!.map((s) => (
                              <span
                                key={s.index}
                                className="h-[3px] flex-1 rounded-full"
                                style={{ background: 'var(--color-warm)', opacity: 0.5 }}
                              />
                            ))}
                          </div>
                          <ol className="mt-4 space-y-3">
                            {selected.steps!.map((s) => (
                              <li key={s.index} className="flex gap-3">
                                <span className="mt-0.5 inline-flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-tint-3 font-mono text-label font-semibold tabular-nums text-ink-3">
                                  {s.index}
                                </span>
                                <div className="min-w-0 flex-1">
                                  <div className="flex items-center gap-2">
                                    <span className="truncate text-card-title font-medium text-ink-1">
                                      {s.name}
                                    </span>
                                    {s.model && (
                                      <span className="shrink-0 rounded bg-tint-3 px-1.5 py-0.5 font-mono text-label-sm uppercase tracking-wider text-ink-3">
                                        {s.model}
                                      </span>
                                    )}
                                  </div>
                                  {s.brief && (
                                    <p className="mt-0.5 text-reported leading-snug text-ink-3">
                                      {s.brief}
                                    </p>
                                  )}
                                </div>
                              </li>
                            ))}
                          </ol>
                        </>
                      ) : selected.body.trim() ? (
                        <p className="mt-3 whitespace-pre-wrap font-mono text-secondary leading-relaxed text-ink-2">
                          {selected.body}
                        </p>
                      ) : (
                        <p className="mt-3 text-ui italic text-ink-3">No description.</p>
                      )}
                    </div>
                  ) : failedEmpty ? (
                    /* The pane is the roomy half, so the failure states its
                       case here and the rail keeps its one-line echo. Saying
                       the rows didn't load — rather than that there are none —
                       is the whole point: the user's prompts are still there. */
                    <div
                      role="alert"
                      className="flex flex-1 flex-col items-center justify-center gap-2 px-5 text-center"
                    >
                      <p className="text-body text-ink-2">Couldn't load {noun}.</p>
                      <p className="text-ui text-ink-3">{loadError}</p>
                      <button
                        type="button"
                        onClick={() => setRetryKey((k) => k + 1)}
                        className="mt-1 inline-flex items-center gap-1.5 text-ui font-medium text-warm hover:text-warm/80 transition-colors"
                      >
                        <RotateCw size={13} />
                        Retry
                      </button>
                    </div>
                  ) : (
                    <div className="flex flex-1 items-center justify-center px-5">
                      <p className="text-ui text-ink-3">
                        {query ? `No ${noun} match "${query}".` : `No ${noun} yet.`}
                      </p>
                    </div>
                  )}
                </div>
              </div>

              {/* Footer — secondary edit link + primary commit */}
              <div className="px-5 py-3 border-t border-line-1 flex items-center justify-between shrink-0">
                {onEditPrompts ? (
                  <button
                    onClick={onEditPrompts}
                    className="text-ui text-ink-3 hover:text-warm font-medium transition-colors"
                  >
                    Edit Prompts
                  </button>
                ) : (
                  <span />
                )}
                <button
                  onClick={() => commit()}
                  disabled={selectionDisabled || !selected}
                  className="inline-flex items-center gap-1.5 text-ui font-semibold text-warm-ink bg-warm hover:bg-warm/90 px-4 py-1.5 rounded-full transition-colors disabled:opacity-40 disabled:cursor-not-allowed"
                >
                  {selectLabel}
                  <span aria-hidden>→</span>
                </button>
              </div>
            </div>
          </motion.div>
        </>
      )}
    </AnimatePresence>
  )
}
