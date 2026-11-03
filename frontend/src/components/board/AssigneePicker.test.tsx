import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, act, waitFor } from '@testing-library/react'
import AssigneePicker from './AssigneePicker'
import { teamRosterSearch } from '../../lib/teamRoster'
import { jsonBody } from '../../test/apiResponse'
import type { Task, TeamBot, TeamMember } from '../../types'

// The picker's interaction is settled; these pin what a row DOES, the two
// rules about the scrim — it takes the pointer while up, and a click outside
// closes the menu and does nothing else — and that a query is the roster
// route's search, answered in the route's order.

const ME = {
  user_id: 'u1',
  display_name: 'Aidan Allchin',
  github_username: null,
  jira_account_id: null,
  role: 'member',
  is_current_user: true,
} as TeamMember
const PRIYA = {
  user_id: 'u2',
  display_name: 'Priya Raman',
  github_username: null,
  jira_account_id: null,
  role: 'member',
  is_current_user: false,
} as TeamMember
const BOT = { agent_id: 'a1', display_name: 'machinist' } as TeamBot

function task(over: Partial<Task> = {}): Task {
  return { id: 't1', title: 'x', status: 'queued', memory_pending: false, ...over } as Task
}

const handlers = {
  onClaim: vi.fn(async () => {}),
  onUnclaim: vi.fn(async () => {}),
  onDelegate: vi.fn(),
  onReassign: vi.fn(async () => {}),
}
// The board's own search, against the stubbed fetch below.
const searchMembers = teamRosterSearch('t1')

function renderPicker(t: Task, members: TeamMember[] = [ME, PRIYA], bot: TeamBot | null = BOT) {
  return render(
    <>
      <button type="button">outside</button>
      <AssigneePicker
        task={t}
        currentUserID="u1"
        members={members}
        searchMembers={searchMembers}
        bot={bot}
        {...handlers}
      />
    </>,
  )
}

beforeEach(() => {
  for (const h of Object.values(handlers)) h.mockClear()
  document.getElementById('tf-assign-scrim')?.remove()
})

async function flush() {
  // The scrim's class lands on a microtask, after the commit.
  await act(async () => {
    await Promise.resolve()
  })
}

describe('AssigneePicker', () => {
  it('shows the holder as the mark, and the roster as options in the ranking order', () => {
    renderPicker(task({ claimed_by_user_id: 'u1' }))
    const mark = screen.getByRole('button', { name: 'Assigned to Aidan Allchin' })
    expect(mark).toHaveTextContent('AA')
    fireEvent.click(mark)
    const names = screen.getAllByRole('option').map((o) => o.textContent)
    expect(names).toEqual(['Agent · machinist', 'Aidan Allchin', 'Priya Raman', 'Unassign'])
    expect(screen.getByRole('option', { name: 'Aidan Allchin' })).toHaveAttribute(
      'aria-selected',
      'true',
    )
    // Six or fewer: everyone, no filter.
    expect(screen.queryByRole('combobox')).toBeNull()
  })

  it('draws the agent square and nobody as a dash', () => {
    const { rerender } = renderPicker(task({ claimed_by_agent_id: 'a1' }))
    expect(screen.getByRole('button', { name: 'Assigned to Agent · machinist' })).toHaveClass('bot')
    rerender(
      <AssigneePicker
        task={task()}
        currentUserID="u1"
        members={[ME]}
        searchMembers={searchMembers}
        bot={BOT}
        {...handlers}
      />,
    )
    expect(screen.getByRole('button', { name: 'Unassigned' })).toHaveTextContent('—')
  })

  it('routes each row to its verb', async () => {
    renderPicker(task())
    const open = () => fireEvent.click(screen.getByRole('button', { name: 'Unassigned' }))
    open()
    fireEvent.click(screen.getByRole('option', { name: 'Agent · machinist' }))
    expect(handlers.onDelegate).toHaveBeenCalledTimes(1)
    open()
    fireEvent.click(screen.getByRole('option', { name: 'Aidan Allchin' }))
    expect(handlers.onClaim).toHaveBeenCalledTimes(1)
    open()
    fireEvent.click(screen.getByRole('option', { name: 'Priya Raman' }))
    expect(handlers.onReassign).toHaveBeenCalledWith(expect.objectContaining({ id: 't1' }), 'u2')
    // Unassign on an unclaimed task moves nothing.
    open()
    fireEvent.click(screen.getByRole('option', { name: 'Unassign' }))
    expect(handlers.onUnclaim).not.toHaveBeenCalled()
  })

  it('returns a held task to the queue from Unassign, and leaves the holder’s own row inert', () => {
    renderPicker(task({ claimed_by_user_id: 'u1' }))
    fireEvent.click(screen.getByRole('button', { name: 'Assigned to Aidan Allchin' }))
    fireEvent.click(screen.getByRole('option', { name: 'Aidan Allchin' }))
    expect(handlers.onClaim).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: 'Assigned to Aidan Allchin' }))
    fireEvent.click(screen.getByRole('option', { name: 'Unassign' }))
    expect(handlers.onUnclaim).toHaveBeenCalledTimes(1)
  })

  it('searches the roster route rather than scrolls past six, five rows always', async () => {
    const many = Array.from({ length: 12 }, (_, i) => ({
      ...PRIYA,
      user_id: `u${i + 10}`,
      display_name: `Person ${String.fromCharCode(65 + i)}`,
    }))
    // The route's answer, in the route's order: two rows a client-side match
    // on the query would drop, and an order no client ranking would produce.
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      ...jsonBody({ items: [many[7], many[2], ME], total_count: 9, bot: BOT }),
    })
    vi.stubGlobal('fetch', fetchMock)
    renderPicker(task(), [ME, ...many])
    fireEvent.click(screen.getByRole('button', { name: 'Unassigned' }))
    // The empty query is the five you actually pick: the agent, yourself, the
    // first of the rest, and the escape hatch — with a count for what is not
    // shown, never a scrollbar. Nothing is asked of the route for it.
    expect(screen.getAllByRole('option')).toHaveLength(5)
    expect(screen.getAllByRole('option').at(-1)).toHaveTextContent('Unassign')
    expect(screen.getByText(/^\+\d+ more$/)).toBeInTheDocument()
    expect(fetchMock).not.toHaveBeenCalled()

    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'person c' } })
    await waitFor(() =>
      expect(screen.getAllByRole('option').map((o) => o.textContent)).toEqual([
        'Person H',
        'Person C',
        'Aidan Allchin',
      ]),
    )
    expect(fetchMock).toHaveBeenCalledTimes(1)
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/teams/t1/members/list')
    expect(JSON.parse(String((init as RequestInit).body))).toEqual({
      page_size: 5,
      search: 'person c',
    })
    // The count is the route's total less what is shown.
    expect(screen.getByText('+6 more')).toBeInTheDocument()

    // Enter takes the route's first answer.
    fireEvent.keyDown(document.querySelector('.assign')!, { key: 'Enter' })
    expect(handlers.onReassign).toHaveBeenCalledWith(expect.objectContaining({ id: 't1' }), 'u17')
  })

  it('recedes the page while open, and a click outside closes and does nothing else', async () => {
    const outside = vi.fn()
    renderPicker(task())
    screen.getByRole('button', { name: 'outside' }).addEventListener('click', outside)
    fireEvent.click(screen.getByRole('button', { name: 'Unassigned' }))
    await flush()
    const scrim = document.getElementById('tf-assign-scrim')
    expect(scrim).not.toBeNull()
    expect(scrim).toHaveClass('on')
    expect(document.querySelector('.assign')).toHaveClass('open')

    fireEvent.click(screen.getByRole('button', { name: 'outside' }))
    await flush()
    expect(outside).not.toHaveBeenCalled()
    expect(document.querySelector('.assign')).not.toHaveClass('open')
    expect(scrim).not.toHaveClass('on')
  })

  it('closes on Escape and hands focus back to the mark', () => {
    renderPicker(task())
    const mark = screen.getByRole('button', { name: 'Unassigned' })
    fireEvent.click(mark)
    fireEvent.keyDown(document.querySelector('.assign')!, { key: 'Escape' })
    expect(document.querySelector('.assign')).not.toHaveClass('open')
    expect(document.activeElement).toBe(mark)
  })

  it('keeps the mark and drops the picker for a reader who may not reassign', () => {
    render(
      <AssigneePicker
        task={task({ claimed_by_user_id: 'u2' })}
        currentUserID="u1"
        members={[ME, PRIYA]}
        searchMembers={searchMembers}
        bot={BOT}
        {...handlers}
        readOnly
      />,
    )
    const mark = screen.getByTitle('Priya Raman')
    expect(mark).toHaveClass('static')
    expect(mark).toHaveTextContent('PR')
    expect(screen.queryByRole('listbox')).toBeNull()
  })

  it('marks an agent holder even when this team has no bot configured', () => {
    // Claimed by the agent, but this team has none configured — the fallback
    // mark still needs a seat in the option list to be shown as held.
    renderPicker(task({ claimed_by_agent_id: 'a1' }), [ME, PRIYA], null)
    fireEvent.click(screen.getByRole('button', { name: 'Assigned to Agent' }))
    expect(screen.getByRole('option', { name: 'Agent' })).toHaveAttribute('aria-selected', 'true')
  })

  it('marks a user holder who has since left the team roster', () => {
    renderPicker(task({ claimed_by_user_id: 'ugone' }), [ME, PRIYA], null)
    fireEvent.click(screen.getByRole('button', { name: 'Assigned to User ugone' }))
    expect(screen.getByRole('option', { name: 'User ugone' })).toHaveAttribute(
      'aria-selected',
      'true',
    )
  })

  it('only wires remeasure listeners while the menu is open', () => {
    const addSpy = vi.spyOn(window, 'addEventListener')
    renderPicker(task())
    expect(addSpy).not.toHaveBeenCalledWith('resize', expect.any(Function))

    fireEvent.click(screen.getByRole('button', { name: 'Unassigned' }))
    expect(addSpy).toHaveBeenCalledWith('resize', expect.any(Function))

    const removeSpy = vi.spyOn(window, 'removeEventListener')
    fireEvent.keyDown(document.querySelector('.assign')!, { key: 'Escape' })
    expect(removeSpy).toHaveBeenCalledWith('resize', expect.any(Function))
    addSpy.mockRestore()
    removeSpy.mockRestore()
  })
})
