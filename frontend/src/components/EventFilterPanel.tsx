import { useState, useEffect } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import type { EventType } from '../types'
import {
  DndContext,
  closestCenter,
  PointerSensor,
  useSensor,
  useSensors,
  type DragEndEvent,
} from '@dnd-kit/core'
import {
  SortableContext,
  verticalListSortingStrategy,
  useSortable,
  arrayMove,
} from '@dnd-kit/sortable'
import { CSS } from '@dnd-kit/utilities'

interface Props {
  open: boolean
  onToggle: () => void
  onChange: () => void
}

export default function EventFilterPanel({ open, onToggle, onChange }: Props) {
  const [eventTypes, setEventTypes] = useState<EventType[]>([])
  const [loadError, setLoadError] = useState(false)

  useEffect(() => {
    let cancelled = false
    fetch('/api/event-types')
      .then((res) => {
        if (!res.ok) throw new Error(`HTTP ${res.status}`)
        return res.json()
      })
      .then((data: EventType[]) => {
        if (!cancelled) {
          setEventTypes(data.filter((et) => et.source !== 'system'))
          setLoadError(false)
        }
      })
      .catch(() => {
        if (!cancelled) setLoadError(true)
      })
    return () => {
      cancelled = true
    }
  }, [])

  const handleToggle = async (id: string, enabled: boolean) => {
    setEventTypes((prev) => prev.map((et) => (et.id === id ? { ...et, enabled } : et)))
    await fetch(`/api/event-types/${encodeURIComponent(id)}/toggle`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ enabled }),
    })
    onChange()
  }

  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 3 } }))

  const handleDragEnd = async (event: DragEndEvent) => {
    const { active, over } = event
    if (!over || active.id === over.id) return

    const oldIndex = eventTypes.findIndex((et) => et.id === active.id)
    const newIndex = eventTypes.findIndex((et) => et.id === over.id)
    const reordered = arrayMove(eventTypes, oldIndex, newIndex)
    setEventTypes(reordered)

    await fetch('/api/event-types/reorder', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(reordered.map((et) => et.id)),
    })
    onChange()
  }

  const enabledCount = eventTypes.filter((et) => et.enabled).length

  return (
    <>
      {/* Toggle button */}
      <button
        onClick={onToggle}
        className={`flex items-center gap-1.5 text-[12px] font-medium px-3 py-1.5 rounded-full border transition-colors ${
          open
            ? 'bg-accent/10 text-accent border-accent/20'
            : 'text-text-tertiary border-border-subtle hover:text-text-secondary hover:border-border-subtle'
        }`}
        title="Filter event types"
      >
        <svg
          width="14"
          height="14"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="2"
          strokeLinecap="round"
          strokeLinejoin="round"
        >
          <polygon points="22 3 2 3 10 12.46 10 19 14 21 14 12.46 22 3" />
        </svg>
        <span>
          {enabledCount}/{eventTypes.length}
        </span>
      </button>

      {/* Side panel */}
      <AnimatePresence>
        {open && (
          <>
            {/* Invisible backdrop — click to close */}
            <motion.div
              className="fixed inset-0 z-40"
              initial={{ opacity: 0, pointerEvents: 'none' as const }}
              animate={{ opacity: 1, pointerEvents: 'auto' as const }}
              exit={{ opacity: 0, pointerEvents: 'none' as const }}
              onClick={onToggle}
            />
            <motion.div
              initial={{ x: '-100%', opacity: 0 }}
              animate={{ x: 0, opacity: 1 }}
              exit={{ x: '-100%', opacity: 0 }}
              transition={{ type: 'spring', damping: 28, stiffness: 300 }}
              className="fixed top-20 left-4 bottom-4 z-50 w-[240px] bg-surface-raised border border-border-glass rounded-2xl shadow-xl shadow-black/[0.08] flex flex-col overflow-hidden"
            >
              <div className="px-4 py-3.5 border-b border-border-subtle flex items-center justify-between shrink-0">
                <span className="text-[13px] font-semibold text-text-primary">Event Filters</span>
                <button
                  onClick={onToggle}
                  className="text-text-tertiary hover:text-text-secondary transition-colors text-lg leading-none px-1"
                >
                  &times;
                </button>
              </div>
              <div className="flex-1 overflow-y-auto px-3 py-3">
                {loadError && (
                  <div className="mb-2 px-2 py-1.5 rounded text-[11px] text-dismiss bg-dismiss/[0.06] border border-dismiss/20">
                    Failed to load event types
                  </div>
                )}
                <div className="flex items-center justify-between mb-2 px-1">
                  <span className="text-[10px] text-text-tertiary">
                    {enabledCount} of {eventTypes.length} enabled
                  </span>
                  <span className="text-[10px] text-text-tertiary">Drag to reorder</span>
                </div>
                <DndContext
                  sensors={sensors}
                  collisionDetection={closestCenter}
                  onDragEnd={handleDragEnd}
                >
                  <SortableContext
                    items={eventTypes.map((et) => et.id)}
                    strategy={verticalListSortingStrategy}
                  >
                    <div className="space-y-1">
                      {eventTypes.map((et) => (
                        <SortableEventItem key={et.id} eventType={et} onToggle={handleToggle} />
                      ))}
                    </div>
                  </SortableContext>
                </DndContext>
              </div>
            </motion.div>
          </>
        )}
      </AnimatePresence>
    </>
  )
}

function SortableEventItem({
  eventType,
  onToggle,
}: {
  eventType: EventType
  onToggle: (id: string, enabled: boolean) => void
}) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({
    id: eventType.id,
  })

  const style: React.CSSProperties = {
    transform: CSS.Transform.toString(transform),
    transition,
    zIndex: isDragging ? 10 : undefined,
  }

  const sourceColor = eventType.source === 'github' ? 'border-l-black/20' : 'border-l-blue-500/40'

  return (
    <div
      ref={setNodeRef}
      style={style}
      {...attributes}
      {...listeners}
      className={`flex items-center gap-2 px-2.5 py-1.5 rounded-lg border-l-2 ${sourceColor} bg-white/50 cursor-grab active:cursor-grabbing touch-none transition-opacity ${
        isDragging ? 'opacity-60 shadow-md' : eventType.enabled ? 'opacity-100' : 'opacity-40'
      }`}
    >
      {/* Toggle — stops propagation so clicks don't start a drag */}
      <button
        onPointerDown={(e) => e.stopPropagation()}
        onClick={() => onToggle(eventType.id, !eventType.enabled)}
        className={`w-3.5 h-3.5 rounded border shrink-0 flex items-center justify-center transition-colors ${
          eventType.enabled ? 'bg-accent border-accent text-white' : 'border-border-subtle bg-white'
        }`}
      >
        {eventType.enabled && (
          <svg width="8" height="8" viewBox="0 0 8 8" fill="none">
            <path
              d="M1.5 4L3 5.5L6.5 2"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
        )}
      </button>

      {/* Label */}
      <span className="text-[12px] text-text-primary truncate flex-1">{eventType.label}</span>

      {/* Source badge */}
      <span className="text-[9px] text-text-tertiary font-medium uppercase tracking-wider shrink-0">
        {eventType.source === 'github' ? 'GH' : 'Jira'}
      </span>
    </div>
  )
}
