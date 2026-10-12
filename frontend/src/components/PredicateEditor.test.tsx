import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import PredicateEditor from './PredicateEditor'
import { jsonBody } from '../test/apiResponse'
import type { MeResponse, TeamMember } from '../types'

// The viewer has an identity on every source, each a different value, so a
// test can tell which column an allowlist was filled from.
const me: MeResponse = {
  id: 'user-1',
  github_username: 'octocat',
  jira_account_id: '557058:abc-me',
  jira_display_name: 'Me on Jira',
  linear_user_id: 'lin-me',
  linear_display_name: 'Me on Linear',
  orgs: [],
  teams: [],
  org_creation_enabled: true,
  is_operator: false,
}

const roster = vi.hoisted(() => ({ members: [] as TeamMember[] }))
vi.mock('../hooks/useDeploymentConfig', () => ({
  useMe: () => ({ me, loading: false, error: null }),
  useTeamMembers: () => ({ members: roster.members, bot: null, loading: false, error: null }),
}))

function member(over: Partial<TeamMember>): TeamMember {
  return {
    user_id: 'user-x',
    display_name: '',
    github_username: null,
    jira_account_id: null,
    linear_user_id: null,
    role: 'member',
    is_current_user: false,
    ...over,
  }
}

function stubSchema(name: string) {
  vi.stubGlobal(
    'fetch',
    vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      ...jsonBody({ fields: [{ name, type: 'string_list' }] }),
    }),
  )
}

describe('PredicateEditor identity allowlists', () => {
  beforeEach(() => {
    roster.members = []
  })

  it.each([
    ['linear:issue:assigned', 'assignee_in', 'lin-me'],
    ['linear:issue:available', 'creator_in', 'lin-me'],
    ['jira:issue:assigned', 'reporter_in', '557058:abc-me'],
    ['github:pr:opened', 'author_in', 'octocat'],
  ])(
    'the solo toggle on %s %s writes the viewer’s identity for that source',
    async (eventType, field, want) => {
      stubSchema(field)
      const onChange = vi.fn()
      render(<PredicateEditor eventType={eventType} value={{}} onChange={onChange} />)

      fireEvent.click(await screen.findByRole('button', { pressed: false }))

      expect(onChange).toHaveBeenCalledWith({ [field]: [want] })
    },
  )

  it('lists team members by their Linear user id on a Linear event', async () => {
    stubSchema('assignee_in')
    roster.members = [
      member({
        user_id: 'user-1',
        display_name: 'Me',
        github_username: 'octocat',
        linear_user_id: 'lin-me',
        is_current_user: true,
      }),
      member({
        user_id: 'user-2',
        display_name: 'Ada',
        github_username: 'ada-gh',
        linear_user_id: 'lin-ada',
      }),
      member({ user_id: 'user-3', display_name: 'Bob', github_username: 'bob-gh' }),
    ]
    const onChange = vi.fn()
    render(<PredicateEditor eventType="linear:issue:assigned" value={{}} onChange={onChange} />)

    fireEvent.focus(await screen.findByRole('combobox'))

    expect(screen.getByText('lin-ada')).toBeInTheDocument()
    expect(screen.queryByText('ada-gh')).not.toBeInTheDocument()
    expect(screen.getByText('— no Linear identity')).toBeInTheDocument()

    fireEvent.mouseDown(screen.getByText('lin-ada'))
    expect(onChange).toHaveBeenCalledWith({ assignee_in: ['lin-ada'] })
  })
})
