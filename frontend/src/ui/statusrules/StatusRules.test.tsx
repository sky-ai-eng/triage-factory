import { render, screen, fireEvent } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import StatusRules from './StatusRules'

// The board is a drag surface, and drag is pointer-only. It does not need an
// invented keyboard equivalent because it already had one — click a tray chip to
// stage it, then click a column to land it. That path simply was not reachable.
// These pin that it is.

const MAP = {
  // `ready` has no primary on purpose: TF reads work out of it and never
  // writes it, so it is the one column with no ★ to set.
  ready: { members: ['To Do'], primary: null },
  inprogress: { members: ['In Progress'], primary: 'In Progress' },
  review: { members: [], primary: null },
  done: { members: ['Done'], primary: 'Done' },
}

const STATUSES = ['To Do', 'In Progress', 'Done', 'QA', 'Blocked']

describe('StatusRules', () => {
  it('leaves unmapped statuses in the tray rather than calling them an error', () => {
    render(<StatusRules map={MAP} statuses={STATUSES} />)

    // A status nobody mapped simply sits in the tray.
    expect(screen.getByRole('button', { name: /QA/ })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Blocked/ })).toBeInTheDocument()
  })

  it('stages a tray chip and lands it in a column from the keyboard', () => {
    render(<StatusRules map={MAP} statuses={STATUSES} />)

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

  it('escape cancels a staged chip', () => {
    render(<StatusRules map={MAP} statuses={STATUSES} />)

    const qa = screen.getByRole('button', { name: /QA/ })
    fireEvent.keyDown(qa, { key: 'Enter' })
    expect(qa).toHaveAttribute('aria-pressed', 'true')

    fireEvent.keyDown(window, { key: 'Escape' })
    expect(screen.getByRole('button', { name: /QA/ })).toHaveAttribute('aria-pressed', 'false')
  })

  it('a read-only board has nothing to reach, rather than things that are disabled', () => {
    render(<StatusRules map={MAP} statuses={STATUSES} interactive={false} />)

    // Nothing is greyed out: the mapping answers "where does our work come
    // from", which a member legitimately needs. Dimming it would hide
    // information to signal a permission.
    expect(screen.getByText('QA')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /QA/ })).not.toBeInTheDocument()
    expect(document.querySelector('[draggable="true"]')).not.toBeInTheDocument()
  })

  it('marks the write target for each column that has one', () => {
    const { container } = render(<StatusRules map={MAP} statuses={STATUSES} />)

    // ★ is the status TF writes back, and `ready` never has one — TF reads work
    // out of it and never writes it, so 'To Do' must not be marked.
    const canonical = Array.from(container.querySelectorAll('[data-canonical]'))
    expect(canonical.length).toBeGreaterThan(0)
    expect(canonical.map((el) => el.textContent)).not.toContain('To Do')
  })
})
