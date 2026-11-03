import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import TeamMemberPicker from './TeamMemberPicker'
import { jsonBody } from '../test/apiResponse'

describe('TeamMemberPicker', () => {
  it('asks the org roster for the people not on the team, and offers what it answers', async () => {
    // u1 is on the team as far as any client could know. The server's answer
    // is the candidate set, so it is offered as given rather than checked
    // against a roster the picker no longer reads.
    const fetchMock = vi.fn<(url: string, init?: RequestInit) => Promise<unknown>>(() =>
      Promise.resolve({
        ok: true,
        ...jsonBody({
          items: [
            { user_id: 'u1', display_name: 'Robin', github_username: null, role: 'member' },
            { user_id: 'u2', display_name: '', github_username: 'dana-gh', role: 'member' },
          ],
          next_page_token: '',
          total_count: 2,
        }),
      }),
    )
    vi.stubGlobal('fetch', fetchMock)
    render(<TeamMemberPicker orgId="org-1" teamId="t1" onAdded={vi.fn()} />)

    fireEvent.click(screen.getByRole('button', { name: /Add member/ }))

    expect(await screen.findByRole('option', { name: 'Robin' })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: '@dana-gh' })).toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledTimes(1)
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/orgs/org-1/members/list')
    expect(JSON.parse(String(init?.body))).toEqual({ page_size: 200, excluding_team_id: 't1' })
  })
})
