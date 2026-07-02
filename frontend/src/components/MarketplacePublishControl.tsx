import { useState, useEffect, useCallback } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import { Store, X } from 'lucide-react'
import { toast } from './Toast/toastStore'
import { readError } from '../lib/api'
import { useOptionalAuth } from '../contexts/AuthContext'
import type { EventType } from '../types'

// MarketplaceListing mirrors the wire shape of domain.MarketplaceListing
// (internal/domain/marketplace.go) — only the fields this control reads.
interface MarketplaceListing {
  id: string
  kind: 'prompt' | 'blueprint'
  status: 'published' | 'delisted'
  name: string
  description: string
  current_version: number
}

interface MarketplacePublishControlProps {
  kind: 'prompt' | 'blueprint'
  sourceId: string
  defaultName: string
  // Prefills the dialog's event-type multi-select from the object's attached
  // trigger (BindingGraph already loads event_handlers for the canvas — no
  // extra fetch needed to derive this upstream).
  suggestedEventTypes?: string[]
  // Withholds the publish/republish/delist affordance without hiding the
  // "Published" badge — a viewer can see marketplace state but not act on it.
  readOnly?: boolean
  // Compact renders a single small pill (for tight chrome like the blueprint
  // details popup); the default renders a labeled row (for the prompt drawer).
  compact?: boolean
}

// MarketplacePublishControl is the publish/republish/delist affordance
// shared between the blueprint canvas (BindingGraph's box menu) and the
// standalone prompt drawer (PromptDrawer) — TFAC-536. It renders nothing in
// local mode (the marketplace is multi-mode only) or while the org hasn't
// enabled the marketplace (the by-source lookup 404s and the control just
// stays hidden; TFAC-539 owns exposing an explicit "coming soon" affordance).
export default function MarketplacePublishControl({
  kind,
  sourceId,
  defaultName,
  suggestedEventTypes,
  readOnly = false,
  compact = false,
}: MarketplacePublishControlProps) {
  const isMulti = useOptionalAuth() !== null
  const [listing, setListing] = useState<MarketplaceListing | null | undefined>(undefined)
  const [dialogOpen, setDialogOpen] = useState(false)

  const refresh = useCallback(() => {
    if (!isMulti || !sourceId) return
    fetch(`/api/marketplace/listings/by-source/${encodeURIComponent(sourceId)}`)
      .then((res) => (res.ok ? res.json() : null))
      .then((data) => setListing(data ?? null))
      .catch(() => setListing(null))
  }, [isMulti, sourceId])

  useEffect(() => {
    refresh()
  }, [refresh])

  const setStatus = async (action: 'delist' | 'relist') => {
    if (!listing) return
    try {
      const res = await fetch(`/api/marketplace/listings/${listing.id}/${action}`, {
        method: 'POST',
      })
      if (!res.ok) {
        toast.error(await readError(res, `Failed to ${action}`))
        return
      }
      toast.info(
        action === 'delist' ? 'Delisted from the marketplace' : 'Relisted on the marketplace',
      )
      refresh()
    } catch (err) {
      toast.error(`Failed to ${action}: ${(err as Error).message}`)
    }
  }

  if (!isMulti || !sourceId || listing === undefined) return null

  const published = listing?.status === 'published'
  const delisted = listing?.status === 'delisted'

  const badgeLabel = published ? 'Published' : delisted ? 'Delisted' : null
  const badgeClass = published ? 'bg-accent/10 text-accent' : 'bg-black/[0.04] text-text-tertiary'

  return (
    <div className={compact ? 'flex items-center gap-1.5' : 'flex flex-wrap items-center gap-1.5'}>
      {badgeLabel && (
        <span
          className={`inline-flex items-center gap-1 text-[9px] font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded ${badgeClass}`}
        >
          <Store size={9} className="shrink-0" />
          {badgeLabel}
        </span>
      )}
      {!readOnly && (
        <>
          <button
            onClick={() => setDialogOpen(true)}
            className="text-[11px] font-medium text-text-secondary hover:text-text-primary hover:bg-black/[0.04] px-2 py-1 rounded-md border border-border-subtle transition-colors"
          >
            {listing ? 'Publish update' : 'Publish to marketplace'}
          </button>
          {published && (
            <button
              onClick={() => void setStatus('delist')}
              className="text-[11px] font-medium text-text-tertiary hover:text-red-500 px-2 py-1 rounded-md transition-colors"
            >
              Delist
            </button>
          )}
          {delisted && (
            <button
              onClick={() => void setStatus('relist')}
              className="text-[11px] font-medium text-text-secondary hover:text-text-primary px-2 py-1 rounded-md transition-colors"
            >
              Relist
            </button>
          )}
        </>
      )}
      <PublishDialog
        open={dialogOpen}
        kind={kind}
        sourceId={sourceId}
        defaultName={defaultName}
        suggestedEventTypes={suggestedEventTypes}
        existingListing={listing}
        onClose={() => setDialogOpen(false)}
        onPublished={() => {
          setDialogOpen(false)
          refresh()
        }}
      />
    </div>
  )
}

interface PublishDialogProps {
  open: boolean
  kind: 'prompt' | 'blueprint'
  sourceId: string
  defaultName: string
  suggestedEventTypes?: string[]
  existingListing: MarketplaceListing | null | undefined
  onClose: () => void
  onPublished: () => void
}

function PublishDialog({
  open,
  kind,
  sourceId,
  defaultName,
  suggestedEventTypes,
  existingListing,
  onClose,
  onPublished,
}: PublishDialogProps) {
  const isRepublish = !!existingListing
  const [name, setName] = useState(defaultName)
  const [description, setDescription] = useState('')
  const [eventTypes, setEventTypes] = useState<string[]>(suggestedEventTypes ?? [])
  const [catalog, setCatalog] = useState<EventType[]>([])
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  // Fetch the event-type catalog once per dialog open.
  useEffect(() => {
    if (!open) return
    let cancelled = false
    fetch('/api/event-types')
      .then((r) => (r.ok ? r.json() : []))
      .then((data) => {
        if (!cancelled && Array.isArray(data)) setCatalog(data)
      })
      .catch(() => {})
    return () => {
      cancelled = true
    }
  }, [open])

  // Seed the form fresh each time the dialog opens — republish prefills from
  // the existing listing; a first publish prefills name from the object and
  // event types from the suggestion (the attached trigger's event_type).
  useEffect(() => {
    if (!open) return
    setError('')
    if (existingListing) {
      setName(existingListing.name)
      setDescription(existingListing.description)
    } else {
      setName(defaultName)
      setDescription('')
      setEventTypes(suggestedEventTypes ?? [])
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, existingListing?.id])

  const toggleEventType = (id: string) => {
    setEventTypes((prev) => (prev.includes(id) ? prev.filter((e) => e !== id) : [...prev, id]))
  }

  const submit = async () => {
    if (!name.trim()) {
      setError('Name is required')
      return
    }
    setSaving(true)
    setError('')
    try {
      const res = isRepublish
        ? await fetch(`/api/marketplace/listings/${existingListing!.id}/versions`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ name, description, event_types: eventTypes }),
          })
        : await fetch('/api/marketplace/listings', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
              kind,
              source_id: sourceId,
              name,
              description,
              event_types: eventTypes,
            }),
          })
      if (!res.ok) {
        toast.error(
          await readError(res, isRepublish ? 'Failed to publish update' : 'Failed to publish'),
        )
        return
      }
      toast.info(
        isRepublish ? 'Published an update to the marketplace' : 'Published to the marketplace',
      )
      onPublished()
    } catch (err) {
      toast.error(`Failed to publish: ${(err as Error).message}`)
    } finally {
      setSaving(false)
    }
  }

  return (
    <AnimatePresence>
      {open && (
        <>
          <motion.div
            className="fixed inset-0 bg-black/20 backdrop-blur-sm z-[70]"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            onClick={onClose}
          />
          <motion.div
            className="fixed inset-0 z-[70] flex items-center justify-center pointer-events-none"
            initial={{ opacity: 0, scale: 0.95 }}
            animate={{ opacity: 1, scale: 1 }}
            exit={{ opacity: 0, scale: 0.95 }}
            transition={{ duration: 0.15 }}
          >
            <div
              className="pointer-events-auto bg-surface-raised/95 backdrop-blur-2xl border border-border-glass rounded-2xl shadow-2xl shadow-black/10 w-[480px] max-h-[85vh] flex flex-col overflow-hidden"
              onClick={(e) => e.stopPropagation()}
            >
              <div className="px-6 pt-5 pb-3 flex items-center justify-between shrink-0">
                <h2 className="text-[15px] font-semibold text-text-primary flex items-center gap-2">
                  <Store size={15} className="text-accent" />
                  {isRepublish ? 'Publish update' : 'Publish to marketplace'}
                </h2>
                <button
                  onClick={onClose}
                  className="text-text-tertiary hover:text-text-secondary transition-colors"
                >
                  <X size={18} />
                </button>
              </div>

              <div className="flex-1 overflow-y-auto px-6 py-4 space-y-4 min-h-0">
                {isRepublish && (
                  <div className="text-[11px] text-text-tertiary bg-black/[0.02] border border-border-subtle rounded-lg px-3 py-2 leading-snug">
                    The listing is a snapshot — your local edits since v
                    {existingListing?.current_version} aren&rsquo;t shared until you publish this
                    update.
                  </div>
                )}

                <div>
                  <label className="block text-[12px] font-medium text-text-secondary mb-1.5">
                    Name
                  </label>
                  <input
                    type="text"
                    value={name}
                    onChange={(e) => setName(e.target.value)}
                    className="w-full px-3 py-2 rounded-lg border border-border-subtle bg-white/50 text-[13px] text-text-primary focus:outline-none focus:border-accent/40 focus:ring-1 focus:ring-accent/20 transition-colors"
                  />
                </div>

                <div>
                  <label className="block text-[12px] font-medium text-text-secondary mb-1.5">
                    Description
                  </label>
                  <textarea
                    value={description}
                    onChange={(e) => setDescription(e.target.value)}
                    rows={3}
                    placeholder="What does this help with?"
                    className="w-full px-3 py-2 rounded-lg border border-border-subtle bg-white/50 text-[13px] text-text-primary placeholder:text-text-tertiary focus:outline-none focus:border-accent/40 focus:ring-1 focus:ring-accent/20 transition-colors resize-y"
                  />
                </div>

                <div>
                  <label className="block text-[12px] font-medium text-text-secondary mb-1.5">
                    Event types
                  </label>
                  <div className="max-h-[160px] overflow-y-auto space-y-1 border border-border-subtle rounded-lg p-2 bg-black/[0.02]">
                    {catalog.length === 0 && (
                      <div className="text-[11px] text-text-tertiary px-1 py-1">Loading…</div>
                    )}
                    {catalog.map((et) => (
                      <label
                        key={et.id}
                        className="flex items-center gap-2 text-[12px] text-text-secondary px-1 py-1 rounded hover:bg-black/[0.03] cursor-pointer"
                      >
                        <input
                          type="checkbox"
                          checked={eventTypes.includes(et.id)}
                          onChange={() => toggleEventType(et.id)}
                          className="accent-accent"
                        />
                        <span className="truncate">{et.label}</span>
                      </label>
                    ))}
                  </div>
                  <p className="text-[10px] text-text-tertiary mt-1.5">
                    How other teams find this when browsing — which event it&rsquo;s meant to
                    handle.
                  </p>
                </div>

                <div className="text-[11px] text-amber-700 bg-amber-50 border border-amber-200 rounded-lg px-3 py-2 leading-snug">
                  Publishing makes the full prompt bod{kind === 'blueprint' ? 'ies' : 'y'} visible
                  to every team in the org.
                </div>
              </div>

              <div className="px-6 py-4 border-t border-border-subtle flex items-center justify-end gap-3 shrink-0">
                {error && <span className="text-[12px] text-red-500">{error}</span>}
                <button
                  onClick={onClose}
                  className="text-[12px] text-text-tertiary hover:text-text-secondary font-medium transition-colors"
                >
                  Cancel
                </button>
                <button
                  onClick={submit}
                  disabled={saving}
                  className="text-[12px] font-semibold text-white bg-accent hover:bg-accent/90 px-4 py-1.5 rounded-full transition-colors disabled:opacity-50"
                >
                  {saving ? 'Publishing…' : isRepublish ? 'Publish update' : 'Publish'}
                </button>
              </div>
            </div>
          </motion.div>
        </>
      )}
    </AnimatePresence>
  )
}
