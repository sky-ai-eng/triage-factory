import { render, screen, fireEvent } from '@testing-library/react'
import { describe, expect, it, vi, beforeEach } from 'vitest'
import SourceCard from './SourceCard'

// One card, three surfaces, and the surface is the whole message. What matters
// is that the inert states are genuinely inert — a card that lights up under the
// pointer and then does nothing is worse than one that stays still.

beforeEach(() => localStorage.clear())

describe('SourceCard', () => {
  it('only a configured card is a control', () => {
    const onClick = vi.fn()
    const { rerender } = render(
      <SourceCard
        name="GitHub"
        source="github"
        state="configured"
        scope="33 of 48 repositories"
        onClick={onClick}
      />,
    )
    const live = screen.getByRole('link', { name: 'GitHub' })
    fireEvent.click(live)
    expect(onClick).toHaveBeenCalledTimes(1)

    // There is nothing behind the other two, so neither takes a role or a tab
    // stop — giving one a focus ring would promise otherwise.
    rerender(
      <SourceCard
        name="Slack"
        source="slack"
        state="unavailable"
        scope="not connected"
        onClick={onClick}
      />,
    )
    expect(screen.queryByRole('link')).not.toBeInTheDocument()

    rerender(<SourceCard name="Schedule" state="soon" scope="coming soon" onClick={onClick} />)
    expect(screen.queryByRole('link')).not.toBeInTheDocument()
    expect(onClick).toHaveBeenCalledTimes(1)
  })

  it('opens on enter but not on space', () => {
    const onClick = vi.fn()
    render(<SourceCard name="Jira" source="jira" state="configured" onClick={onClick} />)
    const card = screen.getByRole('link', { name: 'Jira' })

    fireEvent.keyDown(card, { key: 'Enter' })
    expect(onClick).toHaveBeenCalledTimes(1)

    // Space scrolls a page, and a link that swallowed it would be the only one
    // on the page that did.
    fireEvent.keyDown(card, { key: ' ' })
    expect(onClick).toHaveBeenCalledTimes(1)
  })

  it('gives numbers only to a source that has measured something', () => {
    const stats: Array<[string, number]> = [
      ['events · 7d', 164],
      ['became tasks', 22],
    ]
    const { rerender } = render(
      <SourceCard name="GitHub" source="github" state="configured" stats={stats} />,
    )
    expect(screen.getByText('164')).toBeInTheDocument()

    // A zero on an unconnected source reads as a measurement, when the truth is
    // that nothing has been measured.
    rerender(
      <SourceCard
        name="Slack"
        source="slack"
        state="unavailable"
        stats={stats}
        note="An org admin connects Slack once for the whole organization."
      />,
    )
    expect(screen.queryByText('164')).not.toBeInTheDocument()
    expect(screen.getByText(/An org admin connects Slack/)).toBeInTheDocument()
  })

  it('asks once, says so in place, and never offers again', () => {
    const onAction = vi.fn()
    const { unmount } = render(
      <SourceCard
        name="Slack"
        source="slack"
        state="unavailable"
        actionLabel="Ask an org admin"
        requestKey="sky-ai-eng:slack"
        onAction={onAction}
      />,
    )

    fireEvent.click(screen.getByRole('button', { name: 'Ask an org admin' }))
    expect(onAction).toHaveBeenCalledTimes(1)
    // No banner, no toast, nothing moves — the verb becomes the receipt.
    expect(screen.queryByRole('button', { name: 'Ask an org admin' })).not.toBeInTheDocument()

    // A second request tells an admin nothing the first one didn't, so it does
    // not survive a remount.
    unmount()
    render(
      <SourceCard
        name="Slack"
        source="slack"
        state="unavailable"
        actionLabel="Ask an org admin"
        requestKey="sky-ai-eng:slack"
        onAction={onAction}
      />,
    )
    expect(screen.queryByRole('button', { name: 'Ask an org admin' })).not.toBeInTheDocument()
  })
})
