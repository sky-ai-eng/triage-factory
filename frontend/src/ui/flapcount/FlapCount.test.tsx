import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { FlapCount } from './FlapCount'

// The contracts worth pinning: only the digits that changed get windows, the
// roll carries the sign, a width change flaps everything, the diff runs on the
// formatted string, and stillness writes the end state.

function setReducedMotion(reduced: boolean) {
  vi.stubGlobal(
    'matchMedia',
    vi.fn().mockImplementation((query: string) => ({
      matches: reduced && query.includes('prefers-reduced-motion'),
      media: query,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })),
  )
}

const windows = () => Array.from(document.querySelectorAll('.flap-win'))
const strips = () => Array.from(document.querySelectorAll('.flap-strip')) as HTMLElement[]
const statics = () =>
  Array.from(document.querySelectorAll('.flap > .flap-v')).map((n) => n.textContent)

describe('FlapCount', () => {
  beforeEach(() => setReducedMotion(false))

  it('renders plain text on first paint — the first frame a reader sees is not news', () => {
    render(<FlapCount value={312} />)
    expect(windows()).toHaveLength(0)
    expect(statics()).toEqual(['3', '1', '2'])
    expect(screen.getByRole('img', { name: '312' })).toBeInTheDocument()
  })

  it('flaps only the digits whose face turned', () => {
    const { rerender } = render(<FlapCount value={312} />)
    rerender(<FlapCount value={335} />)

    // The leading 3 never moved: static text, no window, no frame.
    expect(statics()).toEqual(['3'])
    expect(windows()).toHaveLength(2)
    expect(document.querySelectorAll('.flap-frame')).toHaveLength(2)
    // Each strip ends on the incoming glyph: old face above, new face below.
    expect(strips().map((s) => s.textContent)).toEqual(['13', '25'])
    expect(strips().every((s) => s.dataset.dir === 'up')).toBe(true)
  })

  it('rolls down on a decrement — the motion carries the sign', () => {
    const { rerender } = render(<FlapCount value={24} />)
    rerender(<FlapCount value={23} />)

    const s = strips()
    expect(s).toHaveLength(1)
    expect(s[0].dataset.dir).toBe('down')
    // Down swaps the child order so the rest position (child 0) is the new
    // value, arriving from above.
    expect(s[0].textContent).toBe('34')
  })

  it('flaps every column when the width changes, rolling the new lead in from blank', () => {
    const { rerender } = render(<FlapCount value={99} />)
    rerender(<FlapCount value={103} />)

    expect(statics()).toEqual([])
    expect(windows()).toHaveLength(3)
    // No zero that was never on the board: shifted columns roll from blank.
    expect(strips().map((s) => s.textContent)).toEqual(['1', '0', '3'])
  })

  it('diffs the formatted string, so an abbreviated figure flaps only its moving glyph', () => {
    const compact = (n: number) =>
      n < 1000 ? String(n) : (Math.round(n / 100) / 10).toFixed(1) + 'k'
    const { rerender } = render(<FlapCount value={1249} format={compact} />)
    rerender(<FlapCount value={1349} format={compact} />)

    // "1.2k" → "1.3k": one flap, three statics.
    expect(statics()).toEqual(['1', '.', 'k'])
    expect(strips().map((s) => s.textContent)).toEqual(['23'])
  })

  it('draws an em dash for null and claims no role', () => {
    render(<FlapCount value={null} />)
    expect(screen.getByText('—')).toBeInTheDocument()
    expect(screen.queryByRole('img')).not.toBeInTheDocument()
  })

  it('hides the strips from assistive technology behind the label', () => {
    const { rerender } = render(<FlapCount value={9} label="9 open pull requests" />)
    rerender(<FlapCount value={12} label="12 open pull requests" />)

    expect(screen.getByRole('img', { name: '12 open pull requests' })).toBeInTheDocument()
    expect(document.querySelector('.flap-win')).toHaveAttribute('aria-hidden', 'true')
  })

  it('writes the end state itself under reduced motion', () => {
    setReducedMotion(true)
    const { rerender } = render(<FlapCount value={312} />)
    rerender(<FlapCount value={335} />)

    // The blanket kills the animation; a `both` fill that never ran would rest
    // on the OLD digit, so [data-still] states the resting transform directly.
    expect(document.querySelector('.flap')).toHaveAttribute('data-still')
  })
})
