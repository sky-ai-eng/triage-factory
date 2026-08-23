import { useEffect, type RefObject } from 'react'

// Elements that can take keyboard focus. Disabled controls and tabindex="-1"
// are excluded — the standard focus-trap set.
const FOCUSABLE_SELECTOR = [
  'a[href]',
  'button:not([disabled])',
  'input:not([disabled])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(',')

function focusable(node: HTMLElement): HTMLElement[] {
  return Array.from(node.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR))
}

interface FocusTrapOptions {
  /** When false the trap is inert (no focus moves, no listener). Default true. */
  active?: boolean
  /** Element to focus on activate; defaults to the first focusable in the
   *  container. Pass the primary field (e.g. an email input) for a nicer
   *  landing than the close button. */
  initialFocus?: RefObject<HTMLElement | null>
}

/**
 * useFocusTrap keeps keyboard focus inside `containerRef` while the trap is
 * active — the WCAG 2.1.2 expectation for modal dialogs: Tab / Shift+Tab cycle
 * within the overlay instead of escaping to the background page. On activate it
 * moves focus to `initialFocus` (or the first focusable); on deactivate it
 * restores focus to whatever was focused before (the trigger), so keyboard users
 * don't lose their place.
 *
 * Everything happens in an effect (no ref reads during render, per
 * react-hooks/refs). The keydown listener is document-level + capture so a Tab
 * is caught and re-wrapped even if focus has slipped outside the container.
 *
 * The project's modals are hand-rolled (role="dialog" + aria-modal +
 * Escape/backdrop) rather than @radix-ui/react-dialog — which is NOT a
 * dependency — so this is the shared, no-dependency primitive they adopt for
 * the focus-management piece with no visual change. The container should carry
 * tabIndex={-1} for the empty-container fallback.
 *
 * Lives in src/ui/shared rather than src/hooks because it knows nothing about
 * the product: given a ref it manages focus, and that is all. src/ui/dialog
 * needs it, and the boundary lint forbids reaching back into hooks/.
 */
export function useFocusTrap(
  containerRef: RefObject<HTMLElement | null>,
  { active = true, initialFocus }: FocusTrapOptions = {},
) {
  useEffect(() => {
    if (!active) return
    const node = containerRef.current
    if (!node) return

    // Captured here (in the effect, before we move focus) rather than during
    // render: nothing inside has stolen focus yet, so this is the trigger.
    const previouslyFocused = document.activeElement as HTMLElement | null

    ;(initialFocus?.current ?? focusable(node)[0] ?? node).focus()

    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key !== 'Tab') return
      const items = focusable(node)
      if (items.length === 0) {
        e.preventDefault() // nothing to move to — keep focus on the container
        return
      }
      const first = items[0]
      const last = items[items.length - 1]
      const current = document.activeElement
      if (e.shiftKey) {
        if (current === first || !node.contains(current)) {
          e.preventDefault()
          last.focus()
        }
      } else if (current === last || !node.contains(current)) {
        e.preventDefault()
        first.focus()
      }
    }

    document.addEventListener('keydown', onKeyDown, true)
    return () => {
      document.removeEventListener('keydown', onKeyDown, true)
      previouslyFocused?.focus?.()
    }
  }, [containerRef, active, initialFocus])
}
