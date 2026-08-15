import { render, screen, act, fireEvent } from '@testing-library/react'
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import SelectionBar from './SelectionBar'

// What matters here is the naming and the pressure, both of which were wrong in
// an earlier version in ways nothing visual would have caught: the bar mounted
// Hold in surface mode while Hold still claimed an accessible name, so a reader
// pressing and holding the control that REMOVES a teammate was told they were
// saving.

const OPTIONS = [
  { id: 'admin', name: 'Team admin', note: 'can add, remove and configure' },
  { id: 'member', name: 'Member' },
  { id: 'org', name: 'Organization admin', blocked: true },
]

describe('SelectionBar', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('names each verb with the gesture it actually costs', () => {
    render(
      <SelectionBar
        count={2}
        actions={[{ label: 'Watch' }]}
        danger={{ label: 'Hold to remove', onConfirm: vi.fn() }}
      />,
    )

    // A verb at 0 is a plain name; one that must be leaned on says so. The
    // wrapper claims nothing — the triggers own their names.
    expect(screen.getByRole('button', { name: 'Watch' })).toBeInTheDocument()
    expect(
      screen.getByRole('button', { name: 'Hold to remove — press and hold to confirm' }),
    ).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Hold to save/ })).not.toBeInTheDocument()
  })

  it('fires an ordinary verb on press, after the rings settle', () => {
    const onSelect = vi.fn()
    render(<SelectionBar count={2} actions={[{ label: 'Watch', onSelect }]} />)

    fireEvent.pointerDown(screen.getByRole('button', { name: 'Watch' }), {
      button: 0,
      clientX: 0,
      clientY: 0,
    })
    // holdMs 0 is the gesture's shortest form, not its absence.
    expect(onSelect).not.toHaveBeenCalled()

    act(() => {
      vi.advanceTimersByTime(260)
    })
    expect(onSelect).toHaveBeenCalledTimes(1)
  })

  it('makes the destructive verb cost more than the ordinary one', () => {
    const watch = vi.fn()
    const remove = vi.fn()
    render(
      <SelectionBar
        count={2}
        actions={[{ label: 'Watch', onSelect: watch }]}
        danger={{ label: 'Hold to remove', ms: 900, onConfirm: remove }}
      />,
    )

    fireEvent.pointerDown(
      screen.getByRole('button', { name: 'Hold to remove — press and hold to confirm' }),
      { button: 0, clientX: 0, clientY: 0 },
    )
    act(() => {
      vi.advanceTimersByTime(300)
    })
    // Long past the point an ordinary verb would have fired.
    expect(remove).not.toHaveBeenCalled()

    act(() => {
      vi.advanceTimersByTime(900)
    })
    expect(remove).toHaveBeenCalledTimes(1)
    expect(watch).not.toHaveBeenCalled()
  })

  it('applies a reversible edit immediately and offers a window to take it back', () => {
    const onPick = vi.fn()
    const onRevert = vi.fn()
    render(
      <SelectionBar
        count={2}
        picker={{ label: 'Role', value: 'member', options: OPTIONS, onPick, onRevert }}
      />,
    )

    fireEvent.click(screen.getByRole('button', { name: 'Change role' }))
    fireEvent.click(screen.getByRole('option', { name: /Team admin/ }))

    // Applied, not staged: a pending state that has not landed yet makes the
    // table lie for the length of the window.
    expect(onPick).toHaveBeenCalledWith(expect.objectContaining({ id: 'admin' }))
    const undo = screen.getByRole('button', { name: /Undo/ })
    expect(undo).toBeInTheDocument()

    fireEvent.click(undo)
    expect(onRevert).toHaveBeenCalledTimes(1)
  })

  it('lists a blocked option rather than hiding it', () => {
    const onPick = vi.fn()
    render(<SelectionBar count={1} picker={{ label: 'Role', options: OPTIONS, onPick }} />)

    fireEvent.click(screen.getByRole('button', { name: 'Change role' }))

    // Hiding it leaves the reader wondering whether the product forgot.
    const blocked = screen.getByRole('option', { name: /Organization admin/ })
    expect(blocked).toBeDisabled()

    fireEvent.click(blocked)
    expect(onPick).not.toHaveBeenCalled()
  })

  it('lets a host drive the undo window it already owns', () => {
    const onUndo = vi.fn()
    render(
      <SelectionBar count={2} undo={{ msg: '2 rows are now Team admin.', frac: 0.4, onUndo }} />,
    )

    // A table holding the snapshot must own the clock, since the same timer
    // fires the commit — one undo treatment, not one per table.
    expect(screen.getByText('2 rows are now Team admin.')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /Undo/ }))
    expect(onUndo).toHaveBeenCalledTimes(1)
  })
})
