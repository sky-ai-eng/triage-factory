import { useState, useEffect, useCallback } from 'react'
import { X } from 'lucide-react'
import { readError } from '../lib/api'
import { toast } from './Toast/toastStore'
import { blueprintsBase } from '../lib/scope'
import type { Blueprint } from '../types'

interface Props {
  // Non-null opens the popup and loads that blueprint; null keeps it closed.
  blueprintId: string | null
  // Template scope (org-template editor) targets /api/org-template/blueprints
  // and is title-only — that table has no description column. Team scope (the
  // default) targets /api/blueprints and edits title + description.
  templateScope?: boolean
  onClose: () => void
  onSaved: () => void
  onDeleted: () => void
}

// BlueprintMetaPopup edits a blueprint's metadata — title (+ description at team
// scope) — in a small modal. It is deliberately NOT a side drawer and NOT a
// step list: blueprint *structure* (steps, order, trigger binding) is a canvas
// concern, and prompt *content* stays in PromptDrawer. This popup is the only
// surface that edits a blueprint's name/description.
export default function BlueprintMetaPopup({
  blueprintId,
  templateScope = false,
  onClose,
  onSaved,
  onDeleted,
}: Props) {
  const base = blueprintsBase(templateScope)
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [source, setSource] = useState('user')
  const [loading, setLoading] = useState(false)
  const [saving, setSaving] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState(false)

  const open = blueprintId !== null
  const busy = saving || deleting

  // Load the blueprint header when the popup opens (GET /api/blueprints/{id}).
  useEffect(() => {
    if (!blueprintId) return
    let cancelled = false
    setLoading(true)
    setConfirmDelete(false)
    fetch(`${base}/${blueprintId}`)
      .then((res) => {
        if (!res.ok) throw new Error(`HTTP ${res.status}`)
        return res.json()
      })
      .then((data: Blueprint) => {
        if (cancelled) return
        setName(data.name ?? '')
        setDescription(data.description ?? '')
        setSource(data.source ?? 'user')
      })
      .catch(() => {
        if (!cancelled) toast.error('Failed to load blueprint')
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [blueprintId, base])

  // Escape closes unless a write is in flight.
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !busy) onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, busy, onClose])

  const save = useCallback(async () => {
    if (!blueprintId || !name.trim()) {
      if (!name.trim()) toast.error('Name is required')
      return
    }
    setSaving(true)
    try {
      // Template scope is title-only (no description column).
      const body = templateScope
        ? { name: name.trim() }
        : { name: name.trim(), description: description.trim() }
      const res = await fetch(`${base}/${blueprintId}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
      if (!res.ok) {
        toast.error(await readError(res, 'Failed to save blueprint'))
        return
      }
      onSaved()
    } catch (err) {
      toast.error(`Failed to save blueprint: ${err instanceof Error ? err.message : String(err)}`)
    } finally {
      setSaving(false)
    }
  }, [blueprintId, name, description, templateScope, base, onSaved])

  const handleDelete = useCallback(async () => {
    if (!blueprintId) return
    setDeleting(true)
    try {
      const res = await fetch(`${base}/${blueprintId}`, { method: 'DELETE' })
      if (!res.ok) {
        toast.error(await readError(res, 'Failed to delete blueprint'))
        return
      }
      onDeleted()
    } catch (err) {
      toast.error(`Failed to delete blueprint: ${err instanceof Error ? err.message : String(err)}`)
    } finally {
      setDeleting(false)
    }
  }, [blueprintId, base, onDeleted])

  if (!open) return null

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/30 backdrop-blur-sm"
      onClick={() => {
        if (!busy) onClose()
      }}
    >
      <div
        className="
          relative w-full max-w-md
          rounded-2xl border border-border-glass
          bg-gradient-to-br from-white/95 via-white/90 to-white/85
          shadow-xl shadow-black/[0.08] backdrop-blur-xl
          p-6
        "
        role="dialog"
        aria-modal="true"
        aria-labelledby="blueprint-meta-title"
        onClick={(e) => e.stopPropagation()}
      >
        <header className="flex items-start justify-between mb-5">
          <div>
            <h2
              id="blueprint-meta-title"
              className="text-lg font-semibold tracking-tight text-text-primary"
            >
              Edit blueprint
            </h2>
            <p className="text-[12px] text-text-tertiary mt-0.5">
              {templateScope
                ? 'Rename this template blueprint.'
                : 'Name and describe this blueprint.'}
            </p>
          </div>
          <button
            type="button"
            onClick={onClose}
            disabled={busy}
            className="text-text-tertiary hover:text-text-secondary p-1 rounded-full hover:bg-black/[0.03] disabled:opacity-50"
            aria-label="Close"
          >
            <X size={16} />
          </button>
        </header>

        {loading ? (
          <div className="py-8 text-center text-[13px] text-text-tertiary">Loading…</div>
        ) : (
          <div className="space-y-4">
            <label className="block">
              <span className="block text-[12px] font-medium text-text-secondary mb-1.5">
                Name<span className="text-accent ml-0.5">*</span>
              </span>
              <input
                type="text"
                value={name}
                onChange={(e) => setName(e.target.value)}
                autoFocus
                className="
                  w-full rounded-lg border border-border-subtle
                  bg-white/60 px-3 py-2 text-[13px] text-text-primary
                  placeholder:text-text-tertiary
                  focus:outline-none focus:border-accent focus:bg-white
                "
                placeholder="e.g. Thorough Review"
              />
            </label>

            {!templateScope && (
              <label className="block">
                <span className="block text-[12px] font-medium text-text-secondary mb-1.5">
                  Description
                </span>
                <textarea
                  value={description}
                  onChange={(e) => setDescription(e.target.value)}
                  rows={3}
                  className="
                    w-full rounded-lg border border-border-subtle
                    bg-white/60 px-3 py-2 text-[13px] text-text-primary
                    placeholder:text-text-tertiary resize-none
                    focus:outline-none focus:border-accent focus:bg-white
                  "
                  placeholder="What this blueprint does (optional)"
                />
              </label>
            )}

            {source === 'system' && (
              <div>
                <span className="text-[9px] font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded bg-black/[0.04] text-text-tertiary">
                  system
                </span>
              </div>
            )}

            <div className="flex items-center justify-between pt-2">
              {/* Delete (two-step confirm) */}
              <div>
                {confirmDelete ? (
                  <div className="flex items-center gap-2">
                    <span className="text-[12px] text-text-secondary">Delete?</span>
                    <button
                      onClick={handleDelete}
                      disabled={busy}
                      className="text-[12px] font-semibold text-white bg-red-500 hover:bg-red-600 px-2.5 py-1 rounded-md transition-colors disabled:opacity-50"
                    >
                      {deleting ? 'Deleting…' : 'Confirm'}
                    </button>
                    <button
                      onClick={() => setConfirmDelete(false)}
                      disabled={busy}
                      className="text-[12px] text-text-tertiary hover:text-text-secondary disabled:opacity-50"
                    >
                      Cancel
                    </button>
                  </div>
                ) : (
                  <button
                    onClick={() => setConfirmDelete(true)}
                    disabled={busy}
                    className="text-[12px] text-text-tertiary hover:text-red-500 font-medium transition-colors disabled:opacity-50"
                  >
                    Delete
                  </button>
                )}
              </div>

              {/* Save / cancel */}
              <div className="flex items-center gap-2">
                <button
                  onClick={onClose}
                  disabled={busy}
                  className="
                    rounded-full px-4 py-2 text-[13px]
                    text-text-secondary hover:text-text-primary hover:bg-black/[0.03]
                    transition-all disabled:opacity-50
                  "
                >
                  Cancel
                </button>
                <button
                  onClick={save}
                  disabled={busy || !name.trim()}
                  className="
                    rounded-full px-4 py-2 text-[13px] font-medium
                    bg-accent text-white hover:opacity-90
                    disabled:opacity-50 transition-all
                  "
                >
                  {saving ? 'Saving…' : 'Save'}
                </button>
              </div>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
