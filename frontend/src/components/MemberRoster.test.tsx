import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import MemberRoster from './MemberRoster'
import type { MemberRosterAdapter, RosterMember } from '../hooks/useMemberRoster'

// TFAC-560: a member with no display_name (a GitHub account with no profile
// "Name" set) should render "@<login>" rather than the literal "Unnamed
// user" fallback, so long as a GitHub handle was actually captured.
function adapterFor(members: RosterMember[]): MemberRosterAdapter {
  return {
    roles: ['owner', 'admin', 'member'],
    protectedRole: 'owner',
    fetchMembers: () => Promise.resolve(members),
    changeRole: vi.fn(),
    remove: vi.fn(),
  }
}

describe('MemberRoster — display name fallback', () => {
  it('renders the GitHub handle when display name is empty but a login is captured', async () => {
    render(
      <MemberRoster
        adapter={adapterFor([
          {
            userId: 'u-1',
            displayName: '',
            githubUsername: 'octocat',
            jiraAccountId: null,
            role: 'member',
            isCurrentUser: false,
          },
        ])}
        canManage={false}
      />,
    )

    expect(await screen.findByText('@octocat')).toBeInTheDocument()
    expect(screen.queryByText('Unnamed user')).not.toBeInTheDocument()
    // The avatar initial should match the visible "@octocat" handle, not '?'.
    expect(screen.getByText('O')).toBeInTheDocument()
  })

  it('still falls back to "Unnamed user" with no display name and no GitHub login', async () => {
    render(
      <MemberRoster
        adapter={adapterFor([
          {
            userId: 'u-2',
            displayName: '',
            githubUsername: null,
            jiraAccountId: null,
            role: 'member',
            isCurrentUser: false,
          },
        ])}
        canManage={false}
      />,
    )

    expect(await screen.findByText('Unnamed user')).toBeInTheDocument()
    expect(screen.getByText('U')).toBeInTheDocument()
  })
})
