import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Scan } from './Scan'

// The contracts worth pinning are structural: the tone attribute the
// stylesheet keys on (its specificity is what keeps the caller's own type
// class from hiding the effect), the merged className, and that an inactive
// scan is plain text with no gradient machinery at all.

describe('Scan', () => {
  it('marks the element with its tone and merges the caller class', () => {
    render(
      <Scan className="rr-act" as="div">
        Replaying 6 commits onto origin/main
      </Scan>,
    )
    const el = screen.getByText('Replaying 6 commits onto origin/main')
    expect(el.tagName).toBe('DIV')
    expect(el.className).toBe('scan rr-act')
    // The stylesheet matches .scan[data-tone] so the transparent-text rule
    // out-specifies a sibling class setting color.
    expect(el).toHaveAttribute('data-tone', 'ink')
  })

  it('puts the accent in the crest only when asked', () => {
    render(<Scan tone="cool">line</Scan>)
    expect(screen.getByText('line')).toHaveAttribute('data-tone', 'cool')
  })

  it('renders plainly when inactive — no scan class, no tone, nothing to relight', () => {
    render(
      <Scan className="rr-act" active={false}>
        Waiting for a slot
      </Scan>,
    )
    const el = screen.getByText('Waiting for a slot')
    expect(el.className).toBe('rr-act')
    expect(el).not.toHaveAttribute('data-tone')
  })
})
