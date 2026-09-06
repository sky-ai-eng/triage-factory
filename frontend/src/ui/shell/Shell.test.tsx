import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi, beforeEach } from 'vitest'
import Shell from './Shell'

// What is worth pinning here is the permission model and the layer discipline —
// the parts that are invisible in a screenshot and expensive when wrong. The
// rail's appearance is reviewed on /dev/ui; its behaviour across grants is
// reviewed here.

function rail() {
  return document.querySelector('.sh-rail') as HTMLElement
}

beforeEach(() => {
  localStorage.clear()
})

describe('Shell — the route table is the permission model', () => {
  it('gives a local install no multi-tenant destinations at all', () => {
    render(<Shell mode="local" org="Triage Factory" />)

    // Absent, never disabled: a greyed row is an advertisement for a feature
    // the reader cannot buy.
    expect(within(rail()).queryByText('Team')).not.toBeInTheDocument()
    expect(within(rail()).queryByText('Organization')).not.toBeInTheDocument()
    expect(within(rail()).queryByText('Marketplace')).not.toBeInTheDocument()
    expect(within(rail()).queryByText('Fleet')).not.toBeInTheDocument()
    // The work is still there.
    expect(within(rail()).getByText('Board')).toBeInTheDocument()
  })

  it('shows a multi member Usage but no administration', () => {
    render(<Shell mode="multi" grants={[]} />)

    // The section is named for what is actually in it — calling a section with
    // only Usage in it "Administration" would be a lie.
    expect(within(rail()).getByText('CAPACITY')).toBeInTheDocument()
    expect(within(rail()).queryByText('ADMINISTRATION')).not.toBeInTheDocument()
    expect(within(rail()).getByText('Usage')).toBeInTheDocument()
    expect(within(rail()).queryByText('Team')).not.toBeInTheDocument()
  })

  it('gives a team admin Team but not Organization', () => {
    render(<Shell mode="multi" grants={['team-admin']} />)

    expect(within(rail()).getByText('ADMINISTRATION')).toBeInTheDocument()
    expect(within(rail()).getByText('Team')).toBeInTheDocument()
    // An org admin is a separate grant; team-admin does not imply it.
    expect(within(rail()).queryByText('Organization')).not.toBeInTheDocument()
  })

  it('gates Fleet on its own grant, independent of org admin', () => {
    const { rerender } = render(<Shell mode="multi" grants={['org-admin']} />)
    expect(within(rail()).queryByText('Fleet')).not.toBeInTheDocument()

    rerender(<Shell mode="multi" grants={['org-admin', 'fleet']} />)
    expect(within(rail()).getByText('Fleet')).toBeInTheDocument()
  })

  it('hides Governance until the flag says the page exists', () => {
    const { rerender } = render(<Shell mode="multi" grants={['org-admin']} counts={{ gov: 2 }} />)
    expect(within(rail()).queryByText('Governance')).not.toBeInTheDocument()

    rerender(
      <Shell
        mode="multi"
        grants={['org-admin']}
        flags={{ governance: true }}
        counts={{ gov: 2 }}
      />,
    )
    expect(within(rail()).getByText('Governance')).toBeInTheDocument()
  })
})

describe('Shell — the palette reads the same array as the rail', () => {
  it('never offers a destination the rail is hiding', async () => {
    const user = userEvent.setup()
    render(<Shell mode="local" />)

    await user.keyboard('{Meta>}k{/Meta}')
    const q = screen.getByLabelText('Go to')
    await user.type(q, 'team')

    // The whole point of routes() living beside the palette: two copies of a
    // permission rule eventually disagree, and this is the failure mode.
    expect(screen.getByText('nothing by that name')).toBeInTheDocument()
  })

  it('finds a destination the viewer does hold', async () => {
    const user = userEvent.setup()
    render(<Shell mode="multi" grants={['org-admin', 'team-admin']} />)

    await user.keyboard('{Meta>}k{/Meta}')
    await user.type(screen.getByLabelText('Go to'), 'organi')
    expect(document.querySelector('.sh-palrow')).toHaveTextContent('Organization')
  })
})

describe('Shell — offline is inert, not stale', () => {
  it('takes every readout to -- together and states the reason once', () => {
    render(<Shell mode="multi" offline needs={7} running={3} queued={18} counts={{ repos: 8 }} />)

    // A rail with two live numbers and six stale ones is harder to read than
    // one with none.
    expect(within(rail()).queryByText('7')).not.toBeInTheDocument()
    expect(within(rail()).queryByText('18')).not.toBeInTheDocument()
    expect(within(rail()).queryByText('8')).not.toBeInTheDocument()
    // Stated once, in words, at the foot — not six copies of one fact.
    expect(within(rail()).getByRole('status')).toHaveTextContent('Connection lost')
  })

  it('treats a count that has not loaded the same as one that cannot', () => {
    render(<Shell mode="multi" needs={null} queued={null} />)

    // null is NOT 0. "Nothing needs you" is a claim the rail has no business
    // making before it knows.
    expect(within(rail()).queryByText('0')).not.toBeInTheDocument()
    expect(document.querySelector('.sh-qtext')).toHaveTextContent('--')
  })
})

describe('Shell — Escape belongs to the topmost layer', () => {
  it('consumes the press only when it has a layer to dismiss', async () => {
    const user = userEvent.setup()
    const onPageEscape = vi.fn()
    // A page inside the shell whose Escape means something of its own. The
    // contract is that it bails on defaultPrevented, so one press never does
    // two things.
    window.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && !e.defaultPrevented) onPageEscape()
    })

    render(<Shell mode="multi" grants={['org-admin']} />)

    await user.keyboard('{Meta>}k{/Meta}')
    expect(screen.getByLabelText('Go to')).toBeInTheDocument()

    // Shell has a layer open: it consumes the press, the page must not see it.
    await user.keyboard('{Escape}')
    expect(screen.queryByLabelText('Go to')).not.toBeInTheDocument()
    expect(onPageEscape).not.toHaveBeenCalled()

    // Nothing left open: the press falls through to the page.
    await user.keyboard('{Escape}')
    expect(onPageEscape).toHaveBeenCalledTimes(1)
  })
})

describe('Shell — the rail pin', () => {
  it('persists an explicit toggle but never the transient expansion', async () => {
    const user = userEvent.setup()
    render(
      <Shell mode="multi" org="sky" team={{ id: 't1', name: 'platform' }} grants={['org-admin']} />,
    )

    expect(rail()).not.toHaveClass('open')

    await user.keyboard('{Meta>}\\{/Meta}')
    expect(rail()).toHaveClass('open')
    expect(localStorage.getItem('tf-rail')).toBe('1')

    await user.keyboard('{Meta>}\\{/Meta}')
    expect(localStorage.getItem('tf-rail')).toBe('0')

    // Opening a panel from a collapsed rail widens it — a panel is a region of
    // the rail, never a float — and that widening is in service of the panel,
    // so it must not be mistaken for a preference.
    await user.click(document.querySelector('.sh-mark') as HTMLElement)
    expect(rail()).toHaveClass('open')
    expect(localStorage.getItem('tf-rail')).toBe('0')
  })
})

describe('Shell — drilling narrows, it does not navigate', () => {
  it('opens a child column without reporting a route change', async () => {
    const user = userEvent.setup()
    const onRoute = vi.fn()
    render(<Shell mode="multi" grants={[]} onRoute={onRoute} />)

    await user.click(within(rail()).getByText('Prompts'))

    // The parent slid its column aside; nothing navigated.
    expect(onRoute).not.toHaveBeenCalled()
    expect(within(rail()).getByText('Library')).toBeInTheDocument()

    await user.click(within(rail()).getByText('Library'))
    expect(onRoute).toHaveBeenCalledWith('prompts.library')
  })
})

describe('Shell — a page can ask for the whole screen', () => {
  it('fades the chrome and takes it out of the tab order, without resizing it', () => {
    const { rerender } = render(<Shell mode="multi" grants={['org-admin']} />)

    expect(rail()).not.toHaveAttribute('inert')
    const widthBefore = rail().className

    rerender(<Shell mode="multi" grants={['org-admin']} immersive />)

    // Invisible controls that still take focus are worse than visible ones —
    // the reader tabs into a rail they cannot see.
    expect(rail()).toHaveAttribute('inert')
    expect(document.querySelector('.sh-head')).toHaveAttribute('inert')
    expect(document.querySelector('.sh')).toHaveAttribute('data-immersive')

    // Nothing changed size. A collapse would reflow the page, which is the
    // thing an immersive page is usually escaping.
    expect(rail().className).toBe(widthBefore)
  })
})

describe('Shell — the identity panel is a readout with one verb', () => {
  it('carries no appearance control — that setting lives on the settings page', async () => {
    const user = userEvent.setup()
    render(
      <Shell
        mode="multi"
        org="sky"
        user={{ name: 'Aidan Allchin', email: 'aidan@allchin.com' }}
        grants={['org-admin']}
        onSignOut={() => {}}
      />,
    )
    await user.click(document.querySelector('.sh-me') as HTMLElement)
    // The panel keeps name, email, grants and Sign out; the theme control was
    // a second copy of a personal setting, and a personal setting has one home.
    expect(screen.queryByRole('radiogroup', { name: 'Appearance' })).not.toBeInTheDocument()
    expect(screen.getByText('Sign out')).toBeInTheDocument()
  })
})

describe('Shell — the scope switcher trades in team ids', () => {
  it('reports the clicked row by id, so two same-named teams stay two teams', async () => {
    // Nothing makes display names unique. The id is the selection's currency;
    // the name is only its face — resolving by name here silently picked
    // whichever team with that name sorted first.
    const user = userEvent.setup()
    const onTeamChange = vi.fn()
    render(
      <Shell
        mode="multi"
        org="sky"
        team={{ id: 't1', name: 'Platform' }}
        teams={[
          { id: 't1', name: 'Platform' },
          { id: 't2', name: 'Platform' },
        ]}
        onTeamChange={onTeamChange}
      />,
    )

    await user.click(screen.getByText('sky'))
    const rows = screen.getAllByRole('button', { name: 'Platform' })
    expect(rows).toHaveLength(2)
    // Only the CURRENT team's row is marked current, keyed by id — a name
    // match would mark both.
    expect(rows[0]).toHaveAttribute('aria-current', 'true')
    expect(rows[1]).not.toHaveAttribute('aria-current')

    await user.click(rows[1])
    expect(onTeamChange).toHaveBeenCalledWith('t2')
  })
})
