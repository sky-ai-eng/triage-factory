import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import Checkbox from './Checkbox'

// The keyboard contract is the whole reason this component was extracted, so it
// is what gets pinned. An earlier version declared role="checkbox" with no tab
// stop and no key handler — a control announced as operable that could never be
// reached, which is worse than announcing nothing.

describe('Checkbox', () => {
  it('is reachable and toggles on Space', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(<Checkbox checked={false} onChange={onChange} title="Select row" />)

    await user.tab()
    expect(screen.getByRole('checkbox')).toHaveFocus()

    await user.keyboard(' ')
    expect(onChange).toHaveBeenCalledWith(true, expect.anything())
  })

  it('leaves Enter alone for the row it sits in', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(<Checkbox checked={false} onChange={onChange} title="Select row" />)

    await user.tab()
    await user.keyboard('{Enter}')

    // A row checkbox lives inside a row with its own Enter meaning; swallowing
    // it would take the row's primary action away.
    expect(onChange).not.toHaveBeenCalled()
  })

  it('stays focusable while disabled, and inert', async () => {
    const user = userEvent.setup()
    const onChange = vi.fn()
    render(<Checkbox checked={false} disabled onChange={onChange} title="Owned by another team" />)

    // aria-disabled rather than tabIndex={-1}: a row is usually disabled because
    // something else owns it, and a keyboard reader should be able to find out
    // which rows those are.
    await user.tab()
    const box = screen.getByRole('checkbox')
    expect(box).toHaveFocus()
    expect(box).toHaveAttribute('aria-disabled', 'true')

    await user.keyboard(' ')
    await user.click(box)
    expect(onChange).not.toHaveBeenCalled()
  })

  it('announces the mixed state rather than a guess at it', () => {
    render(<Checkbox indeterminate title="Some selected" />)
    expect(screen.getByRole('checkbox')).toHaveAttribute('aria-checked', 'mixed')
  })

  it('takes its accessible name from the label, falling back to the title', () => {
    const { rerender } = render(<Checkbox title="Select row" />)
    expect(screen.getByRole('checkbox', { name: 'Select row' })).toBeInTheDocument()

    rerender(<Checkbox title="Select row" label="Owned by another team" />)
    expect(screen.getByRole('checkbox', { name: 'Owned by another team' })).toBeInTheDocument()
  })
})
