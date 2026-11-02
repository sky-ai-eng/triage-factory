import { useEffect, useLayoutEffect, useRef } from 'react'
import type { KeyboardEvent } from 'react'
import { createPortal } from 'react-dom'

// A row's ⋯ menu: its secondary actions and the viewer's own marks. Portaled
// to the body and placed against the button's box, so no ancestor's overflow
// or transform can clip or shift it. It closes on a pointer outside it and on
// Escape, which hands focus back to the button that opened it.

export type RowMenuEntry =
  | { kind: 'item'; id: string; label: string; disabled?: boolean }
  | { kind: 'rule' }

const WIDTH = 200
/** The space between the button and the menu. */
const GAP = 4
/** The least room left between the menu and the window's edge. */
const EDGE = 8

export function RowMenu({
  anchor,
  entries,
  onPick,
  onClose,
}: {
  /** The ⋯ button the menu belongs to. */
  anchor: HTMLElement
  entries: RowMenuEntry[]
  /** An entry was chosen, by its id. The menu has closed. */
  onPick: (id: string) => void
  onClose: () => void
}) {
  const box = useRef<HTMLDivElement | null>(null)
  const close = useRef(onClose)
  useEffect(() => {
    close.current = onClose
  })

  // Below the button, unless the window has no room for it there and has
  // room above: a menu that runs off the bottom cannot be scrolled to, since
  // a scroll closes it.
  useLayoutEffect(() => {
    const el = box.current
    if (!el) return
    const b = anchor.getBoundingClientRect()
    const h = el.offsetHeight
    if (b.bottom + GAP + h > window.innerHeight - EDGE && b.top - GAP - h >= EDGE) {
      el.style.top = `${b.top - GAP - h}px`
    }
  }, [anchor])

  useEffect(() => {
    box.current?.querySelector<HTMLElement>('[role="menuitem"]:not([aria-disabled])')?.focus()
    const away = (e: PointerEvent) => {
      const t = e.target as Node
      // The button that opened the menu toggles it itself.
      if (box.current?.contains(t) || anchor.contains(t)) return
      close.current()
    }
    // Placed once, against where the button was: a scroll would leave it
    // floating beside a different row.
    const scrolled = (e: Event) => {
      if (!box.current?.contains(e.target as Node)) close.current()
    }
    document.addEventListener('pointerdown', away)
    document.addEventListener('scroll', scrolled, true)
    return () => {
      document.removeEventListener('pointerdown', away)
      document.removeEventListener('scroll', scrolled, true)
    }
  }, [anchor])

  const r = anchor.getBoundingClientRect()
  const items = () =>
    Array.from(
      box.current?.querySelectorAll<HTMLElement>('[role="menuitem"]:not([aria-disabled])') ?? [],
    )
  const onKeyDown = (e: KeyboardEvent) => {
    if (e.key === 'Escape') {
      e.preventDefault()
      close.current()
      anchor.focus()
      return
    }
    if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return
    e.preventDefault()
    const all = items()
    const at = all.indexOf(document.activeElement as HTMLElement)
    const next = e.key === 'ArrowDown' ? at + 1 : at - 1
    all[(next + all.length) % all.length]?.focus()
  }

  return createPortal(
    <div
      ref={box}
      className="nt-menu"
      role="menu"
      aria-label="Notification actions"
      style={{ left: Math.max(EDGE, r.right - WIDTH), top: r.bottom + GAP, width: WIDTH }}
      onKeyDown={onKeyDown}
    >
      {entries.map((e, i) =>
        e.kind === 'rule' ? (
          <span key={i} className="nt-menu-rule" role="separator" />
        ) : (
          <button
            key={e.id}
            type="button"
            role="menuitem"
            className="nt-menu-item"
            aria-disabled={e.disabled || undefined}
            onClick={() => {
              if (e.disabled) return
              close.current()
              onPick(e.id)
            }}
          >
            {e.label}
          </button>
        ),
      )}
    </div>,
    document.body,
  )
}
