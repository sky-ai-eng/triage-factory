import { useEffect, useCallback, useState } from 'react'
import { ChevronDown, ExternalLink } from 'lucide-react'
import { readError } from '../lib/api'
import { toast } from './Toast/toastStore'
import { useWebSocket } from '../hooks/useWebSocket'

interface ProjectEntity {
  id: string
  source: string
  source_id: string
  kind: string
  title: string
  url: string
  state: string
  classification_rationale?: string
  last_polled_at: string | null
  created_at: string
}

interface Props {
  projectId: string
}

// ProjectEntitiesPanel renders below the knowledge-base panel on the
// project-detail page. Active-only entities scoped to this
// project, ordered most-recently-polled first. Each row collapses to
// a single line; click to expand and reveal the classifier's
// rationale.
export default function ProjectEntitiesPanel({ projectId }: Props) {
  const [entities, setEntities] = useState<ProjectEntity[] | null>(null)
  const [loadError, setLoadError] = useState('')
  const [expanded, setExpanded] = useState<Set<string>>(new Set())

  // Bumped to force a refetch on websocket events without needing a
  // setState-in-effect callback (which the react-hooks lint flags).
  const [refetchKey, setRefetchKey] = useState(0)

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      try {
        const res = await fetch(`/api/projects/${encodeURIComponent(projectId)}/entities`)
        if (cancelled) return
        if (!res.ok) {
          const msg = await readError(res, 'Failed to load entities')
          if (cancelled) return
          setLoadError(msg)
          toast.error(msg)
          return
        }
        const data = (await res.json()) as { entities: ProjectEntity[] }
        if (cancelled) return
        setEntities(data.entities ?? [])
        setLoadError('')
      } catch (err) {
        if (cancelled) return
        const msg = `Failed to load entities: ${(err as Error).message}`
        setLoadError(msg)
        toast.error(msg)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [projectId, refetchKey])

  // Refetch when the backfill popup applies assignments OR when the
  // classifier auto-claims something for this project. Both flows
  // emit `entities_assigned_to_project`. Bump the refetch key — the
  // effect above re-runs with a fresh cancelled flag.
  useWebSocket((event) => {
    if (event.type !== 'entities_assigned_to_project') return
    if (event.project_id !== projectId) return
    setRefetchKey((k) => k + 1)
  })

  const toggle = useCallback((id: string) => {
    setExpanded((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }, [])

  const isLoading = entities === null && !loadError
  const isEmpty = entities !== null && entities.length === 0

  return (
    <section className="rounded-2xl border border-border-glass bg-surface-raised/60 backdrop-blur-sm shadow-sm shadow-black/[0.02] flex flex-col max-h-[40vh] overflow-hidden">
      <header className="px-5 pt-4 pb-3 border-b border-border-subtle">
        <h2 className="text-[14px] font-semibold text-text-primary tracking-tight">
          Entities{' '}
          {entities && entities.length > 0 ? (
            <span className="text-text-tertiary font-normal">({entities.length})</span>
          ) : null}
        </h2>
        <p className="text-[12px] text-text-tertiary mt-0.5 leading-relaxed">
          Active work assigned to this project. Click a row to see why it was assigned.
        </p>
      </header>

      <div className="flex-1 overflow-y-auto px-2 py-2 min-h-0">
        {isLoading && (
          <p className="text-[12px] text-text-tertiary text-center py-8">Loading entities…</p>
        )}
        {loadError && !isLoading && (
          <p className="text-[12px] text-dismiss text-center py-8">{loadError}</p>
        )}
        {isEmpty && (
          <p className="text-[12px] text-text-tertiary text-center py-8 px-4 leading-relaxed">
            No entities yet. Auto-classified entities and entities you reclaim from the
            create-project popup will appear here.
          </p>
        )}
        {!isLoading && !loadError && (entities?.length ?? 0) > 0 && (
          <ul className="space-y-0.5">
            {(entities ?? []).map((e) => (
              <EntityRow
                key={e.id}
                entity={e}
                expanded={expanded.has(e.id)}
                onToggle={() => toggle(e.id)}
              />
            ))}
          </ul>
        )}
      </div>
    </section>
  )
}

interface RowProps {
  entity: ProjectEntity
  expanded: boolean
  onToggle: () => void
}

function EntityRow({ entity, expanded, onToggle }: RowProps) {
  const hasRationale = !!entity.classification_rationale
  // Unlike ProjectBackfillModal's row, this row has no inner native
  // control — it IS the toggle. Add keyboard equivalents (Enter/Space)
  // and the proper ARIA attrs so screen-reader + keyboard users get
  // the same expand-to-reveal-rationale interaction mouse users have.
  // The inner external-link <a> remains independently clickable; its
  // onClick stopPropagation prevents row toggle on link clicks.
  return (
    <li
      className="px-3 py-2 rounded-xl hover:bg-black/[0.02] focus:bg-black/[0.02] focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors cursor-pointer"
      onClick={onToggle}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault()
          onToggle()
        }
      }}
      role="button"
      tabIndex={0}
      aria-expanded={expanded}
    >
      <div className="flex items-center gap-2.5 text-[13px] min-w-0">
        <span className="text-text-tertiary text-[10px] uppercase tracking-wide w-9 shrink-0">
          {sourceShort(entity.source)}
        </span>
        <a
          href={entity.url}
          target="_blank"
          rel="noopener noreferrer"
          onClick={(e) => e.stopPropagation()}
          className="text-text-primary font-medium hover:text-accent transition-colors flex items-center gap-1 shrink-0 max-w-[40%] truncate"
          title={entity.source_id}
        >
          <span className="truncate">{entity.source_id}</span>
          <ExternalLink size={11} className="opacity-50 shrink-0" />
        </a>
        <span className="text-text-secondary truncate flex-1 min-w-0">{entity.title}</span>
        <span className="text-[10px] text-text-tertiary border border-border-subtle rounded-full px-2 py-0.5 whitespace-nowrap shrink-0">
          {entity.state}
        </span>
        <ChevronDown
          size={14}
          className={`text-text-tertiary shrink-0 transition-transform ${expanded ? 'rotate-180' : ''}`}
        />
      </div>
      {expanded && (
        <div className="mt-1.5 ml-11 text-[12px] text-text-tertiary leading-relaxed">
          {hasRationale ? (
            entity.classification_rationale
          ) : (
            <span className="italic">No rationale recorded for this entity.</span>
          )}
        </div>
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
