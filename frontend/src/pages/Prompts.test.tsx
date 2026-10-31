import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { MemoryRouter, useLocation } from 'react-router'
import Prompts from './Prompts'

// The page under test is the deep-link handling, so the active-team hook is a
// plain state the test seeds, and the workspace a stand-in that shows what it
// was handed and reports the trigger's team the test names.
const teamsMock = vi.hoisted(() => ({
  initial: 'team-a',
  teams: [{ id: 'team-a' }, { id: 'team-b' }],
}))
vi.mock('../hooks/useTeams', async () => {
  const { useState } = await import('react')
  return {
    useActiveTeam: () => {
      const [teamId, setTeamId] = useState(teamsMock.initial)
      return { teamId, setTeamId, multi: true, ready: true, teams: teamsMock.teams }
    },
  }
})
vi.mock('../components/TeamSwitch', () => ({ default: () => null }))
vi.mock('../components/PromptsWorkspace', () => ({
  default: (props: {
    teamId: string
    openTriggerId?: string | null
    onTriggerOpened?: (team: string | null) => void
  }) => (
    <div>
      <p>team {props.teamId}</p>
      <p>open {props.openTriggerId ?? 'none'}</p>
      <button onClick={() => props.onTriggerOpened?.('team-b')}>opened on team-b</button>
      <button onClick={() => props.onTriggerOpened?.('team-x')}>opened on team-x</button>
      <button onClick={() => props.onTriggerOpened?.(null)}>not opened</button>
    </div>
  ),
}))

function Search() {
  return <p>search {useLocation().search || 'empty'}</p>
}

function renderAt(url: string) {
  render(
    <MemoryRouter initialEntries={[url]}>
      <Prompts />
      <Search />
    </MemoryRouter>,
  )
}

describe('Prompts deep link', () => {
  beforeEach(() => {
    teamsMock.initial = 'team-a'
  })

  it('opens the linked trigger, then switches to its team and drops the link', async () => {
    renderAt('/prompts?trigger=trig-1')
    expect(screen.getByText('open trig-1')).toBeTruthy()
    expect(screen.getByText('team team-a')).toBeTruthy()

    fireEvent.click(screen.getByText('opened on team-b'))
    await waitFor(() => expect(screen.getByText('search empty')).toBeTruthy())
    expect(screen.getByText('open none')).toBeTruthy()
    expect(screen.getByText('team team-b')).toBeTruthy()
  })

  it("stays on the current team when the viewer isn't on the trigger's", async () => {
    renderAt('/prompts?trigger=trig-1')
    fireEvent.click(screen.getByText('opened on team-x'))
    await waitFor(() => expect(screen.getByText('search empty')).toBeTruthy())
    expect(screen.getByText('team team-a')).toBeTruthy()
  })

  it('drops a link that opened nothing', async () => {
    renderAt('/prompts?trigger=gone')
    fireEvent.click(screen.getByText('not opened'))
    await waitFor(() => expect(screen.getByText('search empty')).toBeTruthy())
    expect(screen.getByText('team team-a')).toBeTruthy()
  })

  it('opens nothing without a link', () => {
    renderAt('/prompts')
    expect(screen.getByText('open none')).toBeTruthy()
  })
})
