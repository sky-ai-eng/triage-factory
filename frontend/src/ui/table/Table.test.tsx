import { render, screen, within, act, fireEvent } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import Table from './Table'
import { tablePages, ago } from './cells'

// The contracts worth pinning are the ones a screenshot cannot show: that the
// undo window holds the request rather than sending and reversing it, that a
// page is a fixed frame, that blanks sort last in BOTH directions, and that a
// verb the selection cannot use is dropped rather than disabled.

const COLS = [
  { key: 'name', label: 'NAME' },
  { key: 'team', label: 'TEAM', align: 'end' as const },
]

const ROWS = [
  { id: '1', name: 'ana', team: 'platform' },
  { id: '2', name: 'bo', team: '' },
  { id: '3', name: 'cass', team: 'infra' },
]

describe('tablePages', () => {
  it('draws every page up to seven, then a window of five', () => {
    expect(tablePages(0, 5)).toEqual([0, 1, 2, 3, 4])
    expect(tablePages(0, 7)).toHaveLength(7)
    // Forty chips is not navigation.
    expect(tablePages(10, 40)).toEqual([8, 9, 10, 11, 12])
  })

  it('keeps the window inside the range at either end', () => {
    expect(tablePages(0, 40)).toEqual([0, 1, 2, 3, 4])
    expect(tablePages(39, 40)).toEqual([35, 36, 37, 38, 39])
  })
})

describe('ago', () => {
  it('prints elapsed time as a measurement, never as a word', () => {
    expect(ago(40)).toBe('40m')
    expect(ago(60 * 6)).toBe('6h')
    expect(ago(60 * 24 * 5)).toBe('5d')
    expect(ago(60 * 24 * 21)).toBe('3w')
    // The days branch stops at a week, so anything longer reads in weeks —
    // Table.md's "12d" example is not a string this can produce.
    expect(ago(60 * 24 * 12)).toBe('2w')
  })
})

describe('Table — sorting', () => {
  it('sorts blanks last in both directions', async () => {
    const user = userEvent.setup()
    render(<Table columns={COLS} rows={ROWS} sortKey="team" />)

    const teamHeader = screen.getByRole('columnheader', { name: /TEAM/ })
    const order = () =>
      screen
        .getAllByRole('row')
        .slice(1)
        .map((r) => within(r).getAllByRole('cell')[1]?.textContent?.trim())

    // No primary team is the absence of a value, not a value below 'a', and it
    // should not fill the top of the column you just asked to rank.
    expect(order().at(-1)).toBe('')
    await user.click(teamHeader)
    expect(order().at(-1)).toBe('')
  })
})

describe('Table — the undo model', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('applies to what you see and holds the request for the window', () => {
    const onCommit = vi.fn()
    render(
      <Table
        columns={COLS}
        rows={ROWS}
        barPosition="inline"
        bar={{ actions: [{ id: 'drop', label: 'Stop watching' }] }}
        mutate={(row, id) => (id === 'drop' ? null : row)}
        onCommit={onCommit}
      />,
    )

    fireEvent.click(screen.getAllByRole('checkbox')[1])
    fireEvent.pointerDown(screen.getByRole('button', { name: 'Stop watching' }), {
      button: 0,
      clientX: 0,
      clientY: 0,
    })
    act(() => {
      vi.advanceTimersByTime(300)
    })

    // Applied immediately — the row is gone from the screen…
    expect(screen.queryByText('ana')).not.toBeInTheDocument()
    // …and the request has NOT been sent. Undo means it never is: no
    // compensating write, no window where the server disagrees with the screen.
    expect(onCommit).not.toHaveBeenCalled()

    act(() => {
      vi.advanceTimersByTime(10_000)
    })
    expect(onCommit).toHaveBeenCalledWith(
      'drop',
      ['1'],
      expect.objectContaining({ reason: 'window' }),
    )
  })

  it('undo puts the rows back and never sends', () => {
    const onCommit = vi.fn()
    render(
      <Table
        columns={COLS}
        rows={ROWS}
        barPosition="inline"
        bar={{ actions: [{ id: 'drop', label: 'Stop watching' }] }}
        mutate={() => null}
        onCommit={onCommit}
      />,
    )

    fireEvent.click(screen.getAllByRole('checkbox')[1])
    fireEvent.pointerDown(screen.getByRole('button', { name: 'Stop watching' }), {
      button: 0,
      clientX: 0,
      clientY: 0,
    })
    act(() => {
      vi.advanceTimersByTime(300)
    })
    expect(screen.queryByText('ana')).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: /Undo/ }))
    expect(screen.getByText('ana')).toBeInTheDocument()

    act(() => {
      vi.advanceTimersByTime(15_000)
    })
    expect(onCommit).not.toHaveBeenCalled()
  })
})

describe('Table — the floating bar', () => {
  it('drops a verb the whole selection cannot use rather than disabling it', () => {
    render(
      <Table
        columns={COLS}
        rows={ROWS}
        barPosition="inline"
        bar={{
          actions: [
            { id: 'watch', label: 'Watch' },
            {
              id: 'claim',
              label: 'Claim as primary',
              enabledFor: (rows) => rows.every((r) => !r.team),
            },
          ],
        }}
      />,
    )

    // 'ana' has a team, so Claim cannot apply — a disabled entry in a floating
    // bar reads as a bug, and the reason is rarely legible from the row.
    fireEvent.click(screen.getAllByRole('checkbox')[1])
    expect(screen.getByRole('button', { name: 'Watch' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Claim as primary' })).not.toBeInTheDocument()
  })

  it('takes the header checkbox to mixed on a partial selection', () => {
    render(<Table columns={COLS} rows={ROWS} barPosition="inline" bar={{ actions: [] }} />)

    const boxes = screen.getAllByRole('checkbox')
    fireEvent.click(boxes[1])
    expect(boxes[0]).toHaveAttribute('aria-checked', 'mixed')

    fireEvent.click(boxes[0])
    expect(boxes[0]).toHaveAttribute('aria-checked', 'true')
  })
})

describe('Table — a row that opens', () => {
  it('gives the row click to onRowOpen and leaves selection to the checkbox', () => {
    const onRowOpen = vi.fn()
    render(<Table columns={COLS} rows={ROWS} onRowOpen={onRowOpen} />)

    const rows = screen.getAllByRole('row').slice(1)
    fireEvent.click(within(rows[0]).getAllByRole('cell')[0])
    expect(onRowOpen).toHaveBeenCalledWith(expect.objectContaining({ id: '1', name: 'ana' }))
    // Opening is not selecting.
    expect(rows[0]).toHaveAttribute('aria-selected', 'false')

    // The checkbox still selects, and selecting is not opening.
    fireEvent.click(within(rows[1]).getByRole('checkbox'))
    expect(rows[1]).toHaveAttribute('aria-selected', 'true')
    expect(onRowOpen).toHaveBeenCalledTimes(1)
  })

  it('leaves the row click as a selection toggle when nothing opens', () => {
    render(<Table columns={COLS} rows={ROWS} />)
    const row = screen.getAllByRole('row')[1]
    fireEvent.click(within(row).getAllByRole('cell')[0])
    expect(row).toHaveAttribute('aria-selected', 'true')
  })
})

describe('Table — the draft page', () => {
  it('is a page in the pager, and only while it is open', async () => {
    const user = userEvent.setup()
    render(
      <Table
        columns={COLS}
        rows={ROWS}
        pageSize={2}
        add={{ label: 'add teammate', options: [{ id: 'd', name: 'dara' }], onAdd: vi.fn() }}
      />,
    )

    // A pager reports where you can go, and there is nowhere to go until the
    // row is open. Queried by [data-draft] rather than by name: the chip is
    // correctly labelled with the add verb, which the title-row button also
    // carries, so the name alone does not identify it.
    const draftChip = () => document.querySelector('.tb-page[data-draft]')
    expect(draftChip()).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: /add teammate/ }))
    expect(draftChip()).toBeInTheDocument()
    expect(screen.getByRole('combobox')).toBeInTheDocument()
  })

  it('offers nobody until three characters are typed', async () => {
    const user = userEvent.setup()
    render(
      <Table
        columns={COLS}
        rows={ROWS}
        add={{
          label: 'add teammate',
          options: [
            { id: 'd', name: 'dara' },
            { id: 'e', name: 'devi' },
          ],
          onAdd: vi.fn(),
        }}
      />,
    )

    await user.click(screen.getByRole('button', { name: /add teammate/ }))
    const field = screen.getByRole('combobox')

    await user.type(field, 'da')
    // A directory dumped the moment the row opens invites picking a name off a
    // list; the reader is meant to know who they are adding.
    expect(screen.queryByRole('option')).not.toBeInTheDocument()

    await user.type(field, 'r')
    expect(screen.getByRole('option', { name: /dara/ })).toBeInTheDocument()
  })

  it('commits every staged name on leaving the page, in the order they were named', async () => {
    const onAdd = vi.fn()
    const user = userEvent.setup()
    render(
      <Table
        columns={COLS}
        rows={ROWS}
        add={{
          label: 'add teammate',
          options: [
            { id: 'd', name: 'dara' },
            { id: 'e', name: 'devi' },
          ],
          onAdd,
        }}
      />,
    )

    await user.click(screen.getByRole('button', { name: /add teammate/ }))
    const field = screen.getByRole('combobox')

    await user.type(field, 'dar')
    await user.click(screen.getByRole('option', { name: /dara/ }))
    await user.clear(field)
    await user.type(field, 'dev')
    await user.click(screen.getByRole('option', { name: /devi/ }))

    // Nothing is applied until you leave the page — the whole page is one
    // reversible act.
    expect(onAdd).not.toHaveBeenCalled()

    await user.keyboard('{Escape}')
    expect(onAdd).toHaveBeenCalledTimes(2)
    expect(onAdd.mock.calls[0][0].name).toBe('dara')
    expect(onAdd.mock.calls[1][0].name).toBe('devi')
  })
})

describe('Table — what is owed gets sent', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  function selectAndAct(label: string) {
    fireEvent.click(screen.getAllByRole('checkbox')[1])
    fireEvent.pointerDown(screen.getByRole('button', { name: label }), {
      button: 0,
      clientX: 0,
      clientY: 0,
    })
    act(() => {
      vi.advanceTimersByTime(300)
    })
  }

  it('hands the picked option to onCommit, so a caller can send what it applied', () => {
    const onCommit = vi.fn()
    render(
      <Table
        columns={COLS}
        rows={ROWS}
        barPosition="inline"
        bar={{
          picker: {
            label: 'Role',
            options: [{ id: 'admin', name: 'Team admin' }],
            action: { id: 'role', label: 'role' },
          },
        }}
        mutate={(row, _id, pick) => ({ ...row, team: pick ? pick.id : row.team })}
        onCommit={onCommit}
      />,
    )

    fireEvent.click(screen.getAllByRole('checkbox')[1])
    fireEvent.click(screen.getByRole('button', { name: 'Change role' }))
    fireEvent.click(screen.getByRole('option', { name: /Team admin/ }))
    act(() => {
      vi.advanceTimersByTime(10_500)
    })

    // Without the pick, a caller can apply a role change and has no way to send
    // it — the screen says it happened and the server never hears.
    expect(onCommit).toHaveBeenCalledWith(
      'role',
      ['1'],
      expect.objectContaining({ pick: expect.objectContaining({ id: 'admin' }), reason: 'window' }),
    )
  })

  it('sends a still-open window when the table unmounts', () => {
    const onCommit = vi.fn()
    const { unmount } = render(
      <Table
        columns={COLS}
        rows={ROWS}
        barPosition="inline"
        bar={{ actions: [{ id: 'drop', label: 'Stop watching' }] }}
        mutate={() => null}
        onCommit={onCommit}
      />,
    )

    selectAndAct('Stop watching')
    expect(onCommit).not.toHaveBeenCalled()

    // Navigating away mid-window used to drop it silently — the reader watched
    // the row go and the server never heard.
    unmount()
    expect(onCommit).toHaveBeenCalledWith(
      'drop',
      ['1'],
      expect.objectContaining({ reason: 'navigation' }),
    )
  })

  it('sends a still-open window when the page is going away', () => {
    const onCommit = vi.fn()
    render(
      <Table
        columns={COLS}
        rows={ROWS}
        barPosition="inline"
        bar={{ actions: [{ id: 'drop', label: 'Stop watching' }] }}
        mutate={() => null}
        onCommit={onCommit}
      />,
    )

    selectAndAct('Stop watching')
    fireEvent(window, new Event('pagehide'))

    // The reason is what lets the caller switch transport: an ordinary fetch is
    // killed with the document.
    expect(onCommit).toHaveBeenCalledWith(
      'drop',
      ['1'],
      expect.objectContaining({ reason: 'unload' }),
    )
  })

  it('sends nothing at all once undone, however the window ends', () => {
    const onCommit = vi.fn()
    const { unmount } = render(
      <Table
        columns={COLS}
        rows={ROWS}
        barPosition="inline"
        bar={{ actions: [{ id: 'drop', label: 'Stop watching' }] }}
        mutate={() => null}
        onCommit={onCommit}
      />,
    )

    selectAndAct('Stop watching')
    fireEvent.click(screen.getByRole('button', { name: /Undo/ }))

    fireEvent(window, new Event('pagehide'))
    unmount()
    expect(onCommit).not.toHaveBeenCalled()
  })

  it('sends exactly once when several endings race', () => {
    const onCommit = vi.fn()
    const { unmount } = render(
      <Table
        columns={COLS}
        rows={ROWS}
        barPosition="inline"
        bar={{ actions: [{ id: 'drop', label: 'Stop watching' }] }}
        mutate={() => null}
        onCommit={onCommit}
      />,
    )

    selectAndAct('Stop watching')
    fireEvent(window, new Event('pagehide'))
    unmount()

    expect(onCommit).toHaveBeenCalledTimes(1)
  })
})
