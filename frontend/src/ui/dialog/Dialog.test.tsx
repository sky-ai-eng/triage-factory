import { render, screen, fireEvent, act } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import Dialog from './Dialog'

// The safety decision recorded in Dialog.md is the thing worth pinning: an
// irreversible verb is never one keystroke away. Everything else in this system
// makes a destructive commit a press-and-hold precisely so a reflex cannot fire
// it, and a focused Enter-bound Confirm would be that hazard restored.

const CONSEQUENCES = [
  { text: 'Every task this team owns is unassigned', tone: 'loss' as const },
  { text: 'Repositories stay connected to the organization', tone: 'keep' as const },
]

describe('Dialog', () => {
  it('states consequences and labels itself from its own head', () => {
    render(
      <Dialog
        open
        title="Delete platform?"
        body="This cannot be undone."
        consequences={CONSEQUENCES}
      />,
    )

    const panel = screen.getByRole('dialog', { name: 'Delete platform?' })
    expect(panel).toHaveAttribute('aria-modal', 'true')
    expect(screen.getByText('Every task this team owns is unassigned')).toBeInTheDocument()
  })

  it('cancels on escape and on the backdrop', () => {
    const onCancel = vi.fn()
    const { container } = render(<Dialog open title="Delete platform?" onCancel={onCancel} />)

    fireEvent.keyDown(window, { key: 'Escape' })
    expect(onCancel).toHaveBeenCalledTimes(1)

    // A dialog you cannot dismiss by looking away is a trap.
    fireEvent.mouseDown(container.querySelector('.dlg-back') as HTMLElement)
    expect(onCancel).toHaveBeenCalledTimes(2)
  })

  it('confirms on Enter, and puts focus on the verb', () => {
    const onConfirm = vi.fn()
    render(<Dialog open title="Archive project?" confirmLabel="Archive" onConfirm={onConfirm} />)

    // Landing on the panel would spend the first Tab getting to a verb.
    expect(screen.getByRole('button', { name: 'Archive' })).toHaveFocus()

    fireEvent.keyDown(window, { key: 'Enter' })
    expect(onConfirm).toHaveBeenCalledTimes(1)
  })

  describe('destructive', () => {
    it('does NOT confirm on Enter', () => {
      const onConfirm = vi.fn()
      render(
        <Dialog
          open
          kind="destructive"
          title="Delete platform?"
          confirmLabel="Delete"
          onConfirm={onConfirm}
        />,
      )

      fireEvent.keyDown(window, { key: 'Enter' })
      // The one verb that cannot be taken back is never a single keystroke away.
      expect(onConfirm).not.toHaveBeenCalled()
    })

    it('lands focus on Cancel, not on the verb', () => {
      render(
        <Dialog
          open
          kind="destructive"
          title="Delete platform?"
          confirmLabel="Delete"
          cancelLabel="Keep it"
        />,
      )

      expect(screen.getByRole('button', { name: 'Keep it' })).toHaveFocus()
      expect(screen.getByRole('button', { name: 'Delete' })).not.toHaveFocus()
    })

    it('still confirms when the verb is pressed deliberately', () => {
      const onConfirm = vi.fn()
      render(
        <Dialog
          open
          kind="destructive"
          title="Delete platform?"
          confirmLabel="Delete"
          onConfirm={onConfirm}
        />,
      )

      fireEvent.click(screen.getByRole('button', { name: 'Delete' }))
      expect(onConfirm).toHaveBeenCalledTimes(1)
    })
  })

  it('renders nothing at all when closed', () => {
    render(<Dialog open={false} title="Delete platform?" />)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  describe('a reading in a dialog', () => {
    it('lays children between the body and the note, in reading order', () => {
      const { container } = render(
        <Dialog open title="deploy" body="Acts as you." note="Shown once.">
          <div data-testid="reading">88d · 1d · 2d</div>
        </Dialog>,
      )

      const kids = Array.from((container.querySelector('.dlg') as HTMLElement).children).map(
        (el) => el.className,
      )
      // rule, head, body, slot, note, acts, caliper — the slot is a direct child
      // so the entrance stagger reaches it like any other line.
      expect(kids.indexOf('dlg-slot')).toBeGreaterThan(kids.indexOf('dlg-body'))
      expect(kids.indexOf('dlg-slot')).toBeLessThan(kids.indexOf('dlg-note'))
      expect(screen.getByTestId('reading')).toBeInTheDocument()
    })

    it('noConfirm leaves Close as the one control, with nothing for Enter to do', () => {
      const onConfirm = vi.fn()
      const onCancel = vi.fn()
      render(
        <Dialog
          open
          noConfirm
          title="deploy"
          cancelLabel="Close"
          onConfirm={onConfirm}
          onCancel={onCancel}
        >
          <div>reading</div>
        </Dialog>,
      )

      expect(screen.getAllByRole('button')).toHaveLength(1)
      expect(screen.getByRole('button', { name: 'Close' })).toHaveFocus()
      fireEvent.keyDown(window, { key: 'Enter' })
      expect(onConfirm).not.toHaveBeenCalled()
      fireEvent.keyDown(window, { key: 'Escape' })
      expect(onCancel).toHaveBeenCalledTimes(1)
    })
  })

  describe('a held confirm', () => {
    it('takes the destructive keyboard rules whatever kind says', () => {
      const onConfirm = vi.fn()
      render(
        <Dialog
          open
          title="deploy"
          confirmLabel="Hold to revoke"
          confirmHold={900}
          onConfirm={onConfirm}
        />,
      )

      // Focus lands on Cancel and Enter is unbound: the held verb is the one
      // that cannot be taken back, so it is never a keystroke away.
      expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus()
      fireEvent.keyDown(window, { key: 'Enter' })
      expect(onConfirm).not.toHaveBeenCalled()
      // A click is not the gesture either.
      fireEvent.click(screen.getByRole('button', { name: 'Hold to revoke' }))
      expect(onConfirm).not.toHaveBeenCalled()
    })

    it('fires only once the whole hold has been paid', () => {
      vi.useFakeTimers()
      try {
        const onConfirm = vi.fn()
        render(
          <Dialog
            open
            build="none"
            title="deploy"
            confirmLabel="Hold to revoke"
            confirmHold={900}
            onConfirm={onConfirm}
          />,
        )
        const verb = screen.getByRole('button', { name: 'Hold to revoke' })
        fireEvent.pointerDown(verb, { button: 0, clientX: 0, clientY: 0 })
        act(() => {
          vi.advanceTimersByTime(600)
        })
        expect(onConfirm).not.toHaveBeenCalled()
        act(() => {
          // The rest of the gesture, plus the Hold's settle beat.
          vi.advanceTimersByTime(300 + 220)
        })
        expect(onConfirm).toHaveBeenCalledTimes(1)
      } finally {
        vi.useRealTimers()
      }
    })
  })

  it('holds the focus ring back until a key moves focus', () => {
    const { container } = render(<Dialog open noConfirm title="deploy" cancelLabel="Close" />)
    const panel = container.querySelector('.dlg') as HTMLElement

    // The trap's initial focus is script focus, which a browser paints as a
    // keyboard ring; a reading opened by a click must not arrive wearing one.
    expect(panel).not.toHaveAttribute('data-kb')
    fireEvent.keyDown(window, { key: 'Tab' })
    expect(panel).toHaveAttribute('data-kb')
  })
})
