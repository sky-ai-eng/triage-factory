import { useState } from 'react'
import { render, screen, fireEvent } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import StatusRules from './StatusRules'
import type { StatusMap } from './StatusRules'

// The board is a drag surface, and drag is pointer-only. It does not need an
// invented keyboard equivalent because it already had one — click a tray chip to
// stage it, then click a column to land it. That path simply was not reachable.
// These pin that it is, and that every gesture now reports the next map out.
//
// Item ids are deliberately NOT the labels: any site that keys on the label
// instead of the id — membership, ★, drag payload — fails these tests rather
// than passing by coincidence.

const item = (label: string) => ({ id: 'id:' + label, label })

const MAP: StatusMap = {
  // `ready` has no primary on purpose: TF reads work out of it and never
  // writes it, so it is the one column with no ★ to set.
  ready: { members: [item('To Do')], primary: null },
  inprogress: { members: [item('In Progress')], primary: 'id:In Progress' },
  review: { members: [], primary: null },
  done: { members: [item('Done')], primary: 'id:Done' },
}

const STATUSES = ['To Do', 'In Progress', 'Done', 'QA', 'Blocked'].map(item)

/** The controlled harness: what a real page mounts. */
function Harness({ onNext }: { onNext?: (next: StatusMap) => void }) {
  const [map, setMap] = useState<StatusMap>(MAP)
  return (
    <StatusRules
      value={map}
      onChange={(next) => {
        setMap(next)
        onNext?.(next)
      }}
      statuses={STATUSES}
    />
  )
}

describe('StatusRules', () => {
  it('leaves unmapped statuses in the tray rather than calling them an error', () => {
    render(<StatusRules value={MAP} statuses={STATUSES} />)

    // A status nobody mapped simply sits in the tray.
    expect(screen.getByRole('button', { name: /QA/ })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Blocked/ })).toBeInTheDocument()
  })

  it('stages a tray chip and lands it in a column from the keyboard', () => {
    render(<StatusRules value={MAP} statuses={STATUSES} />)

    const qa = screen.getByRole('button', { name: /QA/ })
    expect(qa).toHaveAttribute('aria-pressed', 'false')

    fireEvent.keyDown(qa, { key: 'Enter' })
    expect(qa).toHaveAttribute('aria-pressed', 'true')

    // Every column becomes a tab stop while something is staged — that is the
    // only moment any of them is a control — and each name says both halves of
    // the move, so the announcement is unambiguous about which one it lands in.
    expect(screen.getAllByRole('button', { name: /^Put QA in/ })).toHaveLength(4)
    fireEvent.keyDown(screen.getByRole('button', { name: 'Put QA in IN REVIEW' }), { key: 'Enter' })

    // Landed: it is no longer offered in the tray.
    expect(screen.queryByRole('button', { name: /^QA$/ })).not.toBeInTheDocument()
  })

  it('reports every gesture out as the next whole map', () => {
    const seen = vi.fn()
    render(<Harness onNext={seen} />)

    // Land QA in IN REVIEW: the next map carries the full item, and the empty
    // column mints its ★ from the first landing.
    fireEvent.keyDown(screen.getByRole('button', { name: /QA/ }), { key: 'Enter' })
    fireEvent.keyDown(screen.getByRole('button', { name: 'Put QA in IN REVIEW' }), { key: 'Enter' })
    let next = seen.mock.calls.at(-1)![0] as StatusMap
    expect(next.review.members).toEqual([item('QA')])
    expect(next.review.primary).toBe('id:QA')

    // Move the ★ by pressing the other chip: Blocked joins, then takes it.
    fireEvent.keyDown(screen.getByRole('button', { name: /Blocked/ }), { key: 'Enter' })
    fireEvent.keyDown(screen.getByRole('button', { name: 'Put Blocked in IN REVIEW' }), {
      key: 'Enter',
    })
    fireEvent.keyDown(screen.getByRole('button', { name: /Blocked — make this the write target/ }), {
      key: 'Enter',
    })
    next = seen.mock.calls.at(-1)![0] as StatusMap
    expect(next.review.primary).toBe('id:Blocked')
    expect(next.review.members).toHaveLength(2)
  })

  it('never mints a ★ in READY, whose chips are not controls at all', () => {
    const seen = vi.fn()
    render(<Harness onNext={seen} />)

    fireEvent.keyDown(screen.getByRole('button', { name: /QA/ }), { key: 'Enter' })
    fireEvent.keyDown(screen.getByRole('button', { name: 'Put QA in READY' }), { key: 'Enter' })

    const next = seen.mock.calls.at(-1)![0] as StatusMap
    expect(next.ready.members).toHaveLength(2)
    expect(next.ready.primary).toBeNull()
    // A READY chip has no write target to set, so it is not a button — an
    // action-less control would be noise in the tab order.
    expect(screen.queryByRole('button', { name: /To Do/ })).not.toBeInTheDocument()
  })

  it('escape cancels a staged chip', () => {
    render(<StatusRules value={MAP} statuses={STATUSES} />)

    const qa = screen.getByRole('button', { name: /QA/ })
    fireEvent.keyDown(qa, { key: 'Enter' })
    expect(qa).toHaveAttribute('aria-pressed', 'true')

    fireEvent.keyDown(window, { key: 'Escape' })
    expect(screen.getByRole('button', { name: /QA/ })).toHaveAttribute('aria-pressed', 'false')
  })

  it('a read-only board has nothing to reach, rather than things that are disabled', () => {
    render(<StatusRules value={MAP} statuses={STATUSES} interactive={false} />)

    // Nothing is greyed out: the mapping answers "where does our work come
    // from", which a member legitimately needs. Dimming it would hide
    // information to signal a permission.
    expect(screen.getByText('QA')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /QA/ })).not.toBeInTheDocument()
    expect(document.querySelector('[draggable="true"]')).not.toBeInTheDocument()
  })

  it('marks the write target for each column that has one', () => {
    const { container } = render(<StatusRules value={MAP} statuses={STATUSES} />)

    // ★ is the status TF writes back, and `ready` never has one — TF reads work
    // out of it and never writes it, so 'To Do' must not be marked.
    const canonical = Array.from(container.querySelectorAll('[data-canonical]'))
    expect(canonical.length).toBeGreaterThan(0)
    expect(canonical.map((el) => el.textContent)).not.toContain('To Do')
  })
})
