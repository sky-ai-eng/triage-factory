import { useState } from 'react'
import { render, screen, fireEvent } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import Segmented from './Segmented'

// What is worth pinning is the contract a screenshot cannot show: that the
// keyboard moves the choice and skips a struck option, that a struck option is
// still there to be read, and that the group is one tab stop.

const EXPIRY = [
  { value: '7', label: '7 days' },
  { value: '30', label: '30 days' },
  { value: 'never', label: 'never', disabled: true, note: 'sky-ai-eng caps tokens at 90 days' },
]

describe('Segmented', () => {
  it('is a radiogroup with one tab stop, on the chosen option', () => {
    render(<Segmented options={['light', 'dark', 'system']} value="dark" label="Appearance" />)
    expect(screen.getByRole('radiogroup', { name: 'Appearance' })).toBeInTheDocument()
    const radios = screen.getAllByRole('radio')
    expect(radios.map((r) => r.getAttribute('tabindex'))).toEqual(['-1', '0', '-1'])
    expect(radios[1]).toHaveAttribute('aria-checked', 'true')
  })

  it('picks on click and reports nothing for the option already chosen', () => {
    const onChange = vi.fn()
    render(<Segmented options={['light', 'dark', 'system']} value="dark" onChange={onChange} />)
    fireEvent.click(screen.getByRole('radio', { name: 'system' }))
    expect(onChange).toHaveBeenCalledWith('system')
    fireEvent.click(screen.getByRole('radio', { name: 'dark' }))
    expect(onChange).toHaveBeenCalledTimes(1)
  })

  it('moves with the arrows, wraps, and skips a struck option', () => {
    // Controlled, like every caller: the choice lives outside.
    function Host() {
      const [v, setV] = useState('30')
      return <Segmented options={EXPIRY} value={v} onChange={setV} label="Expiry" />
    }
    render(<Host />)
    const group = screen.getByRole('radiogroup')
    const checked = () =>
      screen.getAllByRole('radio').find((r) => r.getAttribute('aria-checked') === 'true')
        ?.textContent

    // Right from the last live option wraps to the first — never lands on the
    // struck one.
    fireEvent.keyDown(group, { key: 'ArrowRight' })
    expect(checked()).toBe('7 days')
    fireEvent.keyDown(group, { key: 'ArrowLeft' })
    expect(checked()).toBe('30 days')
    fireEvent.keyDown(group, { key: 'Home' })
    expect(checked()).toBe('7 days')
    fireEvent.keyDown(group, { key: 'End' })
    expect(checked()).toBe('30 days')
    // Focus travels with the choice, so the one tab stop is where the mark is.
    expect(screen.getByRole('radio', { name: '30 days' })).toHaveFocus()
  })

  it('keeps a struck option in place, unpickable, with its reason on hover', () => {
    const onChange = vi.fn()
    render(<Segmented options={EXPIRY} value="30" onChange={onChange} />)
    const never = screen.getByRole('radio', { name: 'never' })
    expect(never).toHaveAttribute('aria-disabled', 'true')
    expect(never).toHaveAttribute('data-struck')
    expect(never).toHaveAttribute('title', 'sky-ai-eng caps tokens at 90 days')
    fireEvent.click(never)
    expect(onChange).not.toHaveBeenCalled()
  })

  it('re-measures the mark when a label changes under the same option count', () => {
    // jsdom has no layout, so the mark's numbers are all zero; what is worth
    // pinning is that a relabel reaches the measurement at all — the
    // bounding-box read runs again rather than only when the count changes.
    const spy = vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect')
    const { rerender } = render(<Segmented options={['light', 'dark']} value="dark" />)
    const before = spy.mock.calls.length
    rerender(<Segmented options={['light', { value: 'dark', label: 'darker' }]} value="dark" />)
    expect(spy.mock.calls.length).toBeGreaterThan(before)
    spy.mockRestore()
  })

  it('takes every click and every key when the whole control is disabled, and is no tab stop', () => {
    const onChange = vi.fn()
    render(<Segmented options={['light', 'dark']} value="light" onChange={onChange} disabled />)
    const group = screen.getByRole('radiogroup')
    fireEvent.click(screen.getByRole('radio', { name: 'dark' }))
    fireEvent.keyDown(group, { key: 'ArrowRight' })
    fireEvent.keyDown(group, { key: 'End' })
    expect(onChange).not.toHaveBeenCalled()
    expect(group).toHaveAttribute('aria-disabled', 'true')
    // Focus never moved either: a key that cannot pick must not walk the
    // ring to an option it cannot choose.
    expect(screen.getByRole('radio', { name: 'dark' })).not.toHaveFocus()
    expect(screen.getAllByRole('radio').map((r) => r.getAttribute('tabindex'))).toEqual([
      '-1',
      '-1',
    ])
  })
})
