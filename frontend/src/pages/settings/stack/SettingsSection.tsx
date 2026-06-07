// One collapsible row of the Settings stack. Unlike the setup wizard's steps
// (which hide the road ahead and advance linearly), every section here renders
// at once, collapsed by default; expanding one leaves its neighbours — before
// AND after — collapsed and visible. Sections are independent: each owns its
// own draft + Save, so this shell is deliberately dumb about what's inside it.
//
// Flush, not carded: the row + its body sit directly on the ambient
// GlassBackdrop (the stack separates rows with hairlines), and the field
// bodies compose the wizard's `bare`/glass primitives — so Settings reads as
// the same material as /setup, with no nested white cards.
//
// Two flavours, distinguished by whether `onSave` is supplied:
//   • Form sections — a draft the parent tracks as `dirty`, with a Save/Cancel
//     footer and an unsaved-edits guard when you collapse or cancel.
//   • Action sections — self-contained controls whose actions commit inline
//     (Connect, Disconnect, theme, Import), so there's no footer and no guard.

import { useState, type ReactNode } from 'react'
import { ChevronRight } from 'lucide-react'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import { bodyEase } from '../../setup/glassStyle'

export interface SettingsSectionProps {
  title: string
  // Secondary text shown on the collapsed row — the saved state at a glance.
  summary?: ReactNode
  children: ReactNode
  // ── Form-section props (omit all for an action section) ──
  // True when the draft differs from the last-saved baseline: lights the
  // unsaved dot, enables Save, and arms the discard guard on collapse/cancel.
  dirty?: boolean
  saving?: boolean
  // Extra block on Save beyond `dirty` (e.g. a half-filled Jira project rule).
  saveDisabled?: boolean
  saveLabel?: string
  // Returns true when the save landed, so the shell collapses the section.
  onSave?: () => Promise<boolean> | void
  // Reset the draft to baseline. Called by Cancel and by a confirmed discard.
  onCancel?: () => void
  defaultExpanded?: boolean
}

export default function SettingsSection({
  title,
  summary,
  children,
  dirty = false,
  saving = false,
  saveDisabled = false,
  saveLabel = 'Save',
  onSave,
  onCancel,
  defaultExpanded = false,
}: SettingsSectionProps) {
  const reduce = !!useReducedMotion()
  const [expanded, setExpanded] = useState(defaultExpanded)
  const isForm = !!onSave

  // Collapsing (or cancelling) a dirty form section confirms first so a
  // half-typed edit isn't silently dropped. Expanding is always free.
  const guardDiscard = (): boolean => {
    if (!dirty) return true
    return window.confirm('Discard unsaved changes in this section?')
  }

  const toggle = () => {
    // A save is in flight: don't let the header collapse the section (which
    // would fire onCancel/discard and reset the draft) while the request is
    // still committing the pre-cancel values — the footer Cancel is disabled
    // for the same reason, so the header mustn't be a backdoor around it.
    if (saving) return
    if (expanded) {
      if (!guardDiscard()) return
      onCancel?.()
      setExpanded(false)
    } else {
      setExpanded(true)
    }
  }

  const cancel = () => {
    if (!guardDiscard()) return
    onCancel?.()
    setExpanded(false)
  }

  const save = async () => {
    if (!onSave) return
    const ok = await onSave()
    if (ok) setExpanded(false)
  }

  return (
    <div>
      <button
        type="button"
        onClick={toggle}
        aria-expanded={expanded}
        className={`group flex w-full items-center gap-2.5 py-4 text-left ${saving ? 'cursor-default' : ''}`}
      >
        <ChevronRight
          size={15}
          aria-hidden
          className={`shrink-0 text-text-tertiary transition-transform ${expanded ? 'rotate-90' : ''}`}
        />
        <span className="text-[14px] font-medium text-text-primary">{title}</span>
        {/* Unsaved indicator — only meaningful while collapsed (the footer
            carries it when open). */}
        {dirty && !expanded && (
          <span
            aria-label="unsaved changes"
            className="h-1.5 w-1.5 shrink-0 rounded-full bg-accent"
          />
        )}
        {!expanded && summary && (
          <span className="ml-auto truncate text-[12px] text-text-tertiary">{summary}</span>
        )}
      </button>

      <AnimatePresence initial={false}>
        {expanded && (
          <motion.div
            key="body"
            initial={reduce ? false : { height: 0, opacity: 0 }}
            animate={{ height: 'auto', opacity: 1 }}
            exit={reduce ? { opacity: 0 } : { height: 0, opacity: 0 }}
            transition={reduce ? { duration: 0 } : bodyEase}
            style={{ overflow: 'hidden' }}
          >
            <div className="space-y-5 pb-6 pl-7 pr-1 pt-1">
              {children}

              {isForm && (
                <div className="flex items-center justify-end gap-2">
                  <button
                    type="button"
                    onClick={cancel}
                    disabled={saving}
                    className="rounded-xl px-3 py-2 text-[13px] font-medium text-text-tertiary transition-colors hover:text-text-secondary disabled:opacity-40"
                  >
                    Cancel
                  </button>
                  <button
                    type="button"
                    onClick={() => void save()}
                    disabled={!dirty || saving || saveDisabled}
                    className="rounded-full bg-accent px-6 py-2.5 text-[13px] font-medium text-white shadow-[0_10px_28px_-10px_var(--color-accent)] transition-all hover:bg-accent/90 disabled:opacity-40 disabled:shadow-none"
                  >
                    {saving ? 'Saving…' : saveLabel}
                  </button>
                </div>
              )}
            </div>
          </motion.div>
        )}
      </AnimatePresence>
    </div>
  )
}
