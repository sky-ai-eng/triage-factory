import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { ExternalLink } from 'lucide-react'
import { toast } from './Toast/toastStore'
import { apiJSON, httpErrorMessage } from '../lib/apiClient'
import { useFocusTrap } from '../hooks/useFocusTrap'

interface BackfillCandidate {
  id: string
  source: string
  source_id: string
  kind: string
  title: string
  url: string
  state: string
  current_project_id: string
  current_project_name: string
}

interface BackfillFailure {
  entity_id: string
  error: string
}

interface Props {
  projectId: string
  projectName: string
  // onClose fires on skip OR successful submit. Caller decides what
  // happens next (stay on grid for create, navigate to /projects/:id
  // for import). Failure-only states keep the modal open so the user
  // can adjust selections.
  onClose: () => void
}

export default function ProjectBackfillModal({ projectId, projectName, onClose }: Props) {
  const [candidates, setCandidates] = useState<BackfillCandidate[] | null>(null)
  const [loadError, setLoadError] = useState('')
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [failures, setFailures] = useState<Record<string, string>>({})
  const [saving, setSaving] = useState(false)
  const mountedRef = useRef(true)
  // Trap keyboard focus inside the dialog and restore it to the trigger on
  // close (WCAG 2.1.2).
  const dialogRef = useRef<HTMLDivElement>(null)
  useFocusTrap(dialogRef)

  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
    }
  }, [])

  // Escape closes unless a submit is in flight. Mirrors
  // ProjectCreateModal's contract — keep the user from dismissing
  // mid-request, since the dismiss leaves no UI to surface a
  // partial-success response.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !saving) onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose, saving])

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      try {
        const data = await apiJSON<{ candidates: BackfillCandidate[] }>(
          `/api/projects/${encodeURIComponent(projectId)}/backfill-candidates`,
        )
        if (cancelled) return
        const list = data.candidates ?? []
        setCandidates(list)
      } catch (err) {
        if (!cancelled) setLoadError(httpErrorMessage(err, 'Could not load the candidates.'))
      }
    })()
    return () => {
      cancelled = true
    }
  }, [projectId])

  const toggle = useCallback((id: string) => {
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
    setFailures((prev) => {
      if (!(id in prev)) return prev
      const next = { ...prev }
      delete next[id]
      return next
    })
  }, [])

  // Partition candidates into the two sections rendered in the body.
  // Mirrors CarryOverList's "your tickets" / "available to claim" split:
  // unclaimed entities go first since they're the common case; entities
  // already in another project are surfaced below with a caption that
  // calls out the override behavior.
  const { unclaimed, claimedElsewhere } = useMemo(() => {
    const list = candidates ?? []
    const a: BackfillCandidate[] = []
    const b: BackfillCandidate[] = []
    for (const c of list) {
      if (c.current_project_id) b.push(c)
      else a.push(c)
    }
    return { unclaimed: a, claimedElsewhere: b }
  }, [candidates])

  const selectionCount = selected.size
  const handleBackdropClick = () => {
    if (saving) return
    onClose()
  }

  const handleSubmit = async () => {
    if (selectionCount === 0 || saving) return
    setSaving(true)
    setFailures({})
    try {
      const data = await apiJSON<{ applied: number; failed: BackfillFailure[] }>(
        `/api/projects/${encodeURIComponent(projectId)}/backfill`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ entity_ids: Array.from(selected) }),
        },
      )
      const failed = data.failed ?? []
      if (failed.length === 0) {
        if (data.applied > 0) {
          toast.success(
            `Assigned ${data.applied} ${data.applied === 1 ? 'entity' : 'entities'} to ${projectName}`,
          )
        }
        onClose()
        return
      }
      // Partial success: keep the modal open with per-row errors so
      // the user can adjust and retry. Successful rows have already
      // landed server-side; drop them from the list + selection so
      // re-submitting only retries the failures.
      const failedIDs = new Set(failed.map((f) => f.entity_id))
      setCandidates((prev) => (prev ?? []).filter((c) => failedIDs.has(c.id)))
      setSelected(new Set(failed.map((f) => f.entity_id)))
      setFailures(
        failed.reduce<Record<string, string>>((acc, f) => {
          acc[f.entity_id] = f.error
          return acc
        }, {}),
      )
    } catch (err) {
      toast.error(`Failed to assign entities: ${(err as Error).message}`)
    } finally {
      if (mountedRef.current) setSaving(false)
    }
  }

  const isLoading = candidates === null && !loadError
  const isEmpty = candidates !== null && candidates.length === 0

  // Skip the modal entirely on empty candidates — there's nothing to
  // claim, so render nothing and close on next tick. Triggers from
  // useEffect rather than render-time so React doesn't complain.
  useEffect(() => {
    if (isEmpty) onClose()
  }, [isEmpty, onClose])
  if (isEmpty) return null

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/30 backdrop-blur-sm"
      onClick={handleBackdropClick}
    >
      <div
        ref={dialogRef}
        tabIndex={-1}
        className="w-full max-w-2xl backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl shadow-lg shadow-black/[0.04] overflow-hidden flex flex-col max-h-[85vh]"
        role="dialog"
        aria-modal="true"
        aria-labelledby="project-backfill-modal-title"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="px-6 pt-6 pb-4">
          <h2
            id="project-backfill-modal-title"
            className="text-[18px] font-semibold text-text-primary tracking-tight"
          >
            Claim entities for {projectName}
          </h2>
          <p className="text-[13px] text-text-tertiary mt-1 leading-relaxed">
            Pick the entities you want to claim for this project. Unclaimed entities are listed
            first; reclaiming one already in another project moves it.
          </p>
        </div>

        <div className="flex-1 overflow-y-auto px-6 min-h-0">
          {isLoading && (
            <p className="text-[13px] text-text-tertiary text-center py-12">Loading candidates…</p>
          )}
          {loadError && !isLoading && (
            <p className="text-[13px] text-dismiss text-center py-12">{loadError}</p>
          )}
          {!isLoading && !loadError && (candidates?.length ?? 0) > 0 && (
            <div className="py-2 space-y-4">
              {unclaimed.length > 0 && (
                <Section
                  title="Unclaimed"
                  caption="Open entities not yet assigned to any project."
                  candidates={unclaimed}
                  selected={selected}
                  failures={failures}
                  onToggle={toggle}
                />
              )}
              {claimedElsewhere.length > 0 && (
                <Section
                  title="In another project"
                  caption="Already assigned elsewhere — checking one moves it to this project."
                  candidates={claimedElsewhere}
                  selected={selected}
                  failures={failures}
                  onToggle={toggle}
                />
              )}
            </div>
          )}
        </div>

        <div className="px-6 py-4 border-t border-border-subtle flex items-center justify-between">
          <button
            type="button"
            onClick={onClose}
            disabled={saving}
            className="text-[12px] text-text-tertiary hover:text-text-secondary transition-colors"
          >
            Skip for now
          </button>
          <div className="flex items-center gap-3">
            <span className="text-[11px] text-text-tertiary">{selectionCount} selected</span>
            <button
              type="button"
              onClick={handleSubmit}
              disabled={selectionCount === 0 || saving || isLoading || !!loadError}
              className="bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-5 py-2 text-[13px] transition-colors"
            >
              {saving ? 'Assigning…' : 'Assign'}
            </button>
          </div>
        </div>
      </div>
    </div>
  )
}

function Section({
  title,
  caption,
  candidates,
  selected,
  failures,
  onToggle,
}: {
  title: string
  caption: string
  candidates: BackfillCandidate[]
  selected: Set<string>
  failures: Record<string, string>
  onToggle: (id: string) => void
}) {
  return (
    <div>
      <div className="px-1 pb-1.5">
        <h3 className="text-[12px] font-semibold text-text-secondary uppercase tracking-wide">
          {title}
        </h3>
        <p className="text-[11px] text-text-tertiary mt-0.5">{caption}</p>
      </div>
      <ul className="space-y-0.5">
        {candidates.map((c) => (
          <CandidateRow
            key={c.id}
            candidate={c}
            checked={selected.has(c.id)}
            failure={failures[c.id]}
            onToggle={() => onToggle(c.id)}
          />
        ))}
      </ul>
    </div>
  )
}

interface RowProps {
  candidate: BackfillCandidate
  checked: boolean
  failure?: string
  onToggle: () => void
}

function CandidateRow({ candidate, checked, failure, onToggle }: RowProps) {
  const errored = !!failure
  const sourceLabel = useMemo(() => sourceShort(candidate.source), [candidate.source])
  return (
    <li
      className={`
        flex items-center gap-3 px-3 py-2.5 rounded-xl transition-colors cursor-pointer
        ${errored ? 'bg-dismiss/[0.04]' : 'hover:bg-black/[0.02]'}
      `}
      onClick={onToggle}
    >
      <input
        type="checkbox"
        checked={checked}
        onChange={onToggle}
        onClick={(e) => e.stopPropagation()}
        className="h-4 w-4 accent-accent cursor-pointer"
        aria-label={`Toggle ${candidate.source_id}`}
      />
      <div className="flex-1 min-w-0">
        <div className="flex items-center gap-2 text-[13px]">
          <span className="text-text-tertiary text-[11px] uppercase tracking-wide">
            {sourceLabel}
          </span>
          <a
            href={candidate.url}
            target="_blank"
            rel="noopener noreferrer"
            onClick={(e) => e.stopPropagation()}
            className="text-text-primary font-medium hover:text-accent transition-colors flex items-center gap-1"
          >
            {candidate.source_id}
            <ExternalLink size={11} className="opacity-50" />
          </a>
        </div>
        <div className="text-[13px] text-text-secondary truncate">{candidate.title}</div>
        {failure && <div className="text-[11px] text-dismiss mt-1">{failure}</div>}
      </div>
      {candidate.current_project_name && (
        <span className="text-[11px] text-text-tertiary border border-border-subtle rounded-full px-2 py-0.5 whitespace-nowrap">
          in: {candidate.current_project_name}
        </span>
      )}
    </li>
  )
}

function sourceShort(source: string): string {
  switch (source) {
    case 'github':
      return 'PR'
    case 'jira':
      return 'JIRA'
    default:
      return source.toUpperCase()
  }
}
