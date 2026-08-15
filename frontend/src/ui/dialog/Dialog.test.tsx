import { render, screen, fireEvent } from '@testing-library/react'
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
})
