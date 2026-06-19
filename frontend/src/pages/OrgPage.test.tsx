import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { HttpError } from '../lib/apiClient'
import type { PendingInvite } from '../types'

// Mock the apiClient I/O but keep HttpError + httpErrorMessage real (the page's
// error path uses them).
const apiMocks = vi.hoisted(() => ({ apiJSON: vi.fn(), apiFetch: vi.fn() }))
vi.mock('../lib/apiClient', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/apiClient')>()
  return { ...actual, apiJSON: apiMocks.apiJSON, apiFetch: apiMocks.apiFetch }
})

// Admin viewer in a concrete active org.
vi.mock('../hooks/useOrgRole', () => ({
  useOrgRole: () => ({ role: 'admin', isAdmin: true, loading: false }),
}))
vi.mock('../contexts/OrgContext', () => ({ useActiveOrgId: () => 'org-1' }))
vi.mock('../hooks/useTeams', () => ({
  useTeams: () => ({
    teams: [],
    lastActingTeamId: '',
    multi: false,
    loading: false,
    loaded: true,
    error: null,
    refresh: vi.fn(),
    createTeam: vi.fn(),
  }),
}))

import OrgPage from './OrgPage'

// OrgPage reads the active tab from the URL (useSearchParams), so it needs a
// router context. MemoryRouter at its default entry leaves ?tab unset, so the
// People tab — these tests' subject — is the default.
function renderPage() {
  return render(
    <MemoryRouter>
      <OrgPage />
    </MemoryRouter>,
  )
}

const OWNER_ROW = {
  user_id: 'u-owner',
  display_name: 'Alice',
  github_username: 'alice',
  jira_account_id: null,
  role: 'owner',
  is_current_user: true,
}

function makeInvite(over: Partial<PendingInvite> = {}): PendingInvite {
  return {
    id: 'inv-1',
    email: 'bob@example.com',
    role: 'member',
    created_at: new Date(Date.now() - 60 * 60 * 1000).toISOString(),
    expires_at: new Date(Date.now() + 6 * 24 * 60 * 60 * 1000).toISOString(),
    ...over,
  }
}

let invitesState: PendingInvite[]

beforeEach(() => {
  invitesState = [makeInvite()]
  apiMocks.apiJSON.mockReset()
  apiMocks.apiFetch.mockReset()
  apiMocks.apiJSON.mockImplementation(async (path: string, opts?: { method?: string }) => {
    if (path === '/members') return { members: [OWNER_ROW] }
    if (path === '/api/invites') {
      if (opts?.method === 'POST') {
        return {
          id: 'inv-new',
          accept_url: 'https://tf.example.com/invite/accept?token=fresh',
          expires_at: makeInvite().expires_at,
        }
      }
      return invitesState
    }
    throw new Error('unexpected apiJSON path: ' + path)
  })
  apiMocks.apiFetch.mockResolvedValue(undefined as unknown as Response)
})

describe('OrgPage — pending invites', () => {
  it('renders pending invites as muted ghost rows', async () => {
    renderPage()
    expect(await screen.findByText('bob@example.com')).toBeInTheDocument()
    expect(screen.getByText(/pending · invited/i)).toBeInTheDocument()
  })

  it('hides and shows the ghost rows via the Show invited toggle', async () => {
    renderPage()
    await screen.findByText('bob@example.com')

    const toggle = screen.getByRole('switch', { name: /show invited/i })
    fireEvent.click(toggle)
    await waitFor(() => expect(screen.queryByText('bob@example.com')).not.toBeInTheDocument())

    fireEvent.click(toggle)
    expect(await screen.findByText('bob@example.com')).toBeInTheDocument()
  })

  it('Revoke hits POST /api/invites/{id}/revoke', async () => {
    renderPage()
    await screen.findByText('bob@example.com')

    fireEvent.click(screen.getByRole('button', { name: /revoke/i }))
    await waitFor(() =>
      expect(apiMocks.apiFetch).toHaveBeenCalledWith('/api/invites/inv-1/revoke', {
        method: 'POST',
      }),
    )
  })

  it('Resend revokes the old token then re-creates the invite', async () => {
    renderPage()
    await screen.findByText('bob@example.com')

    fireEvent.click(screen.getByRole('button', { name: /resend/i }))

    await waitFor(() =>
      expect(apiMocks.apiFetch).toHaveBeenCalledWith('/api/invites/inv-1/revoke', {
        method: 'POST',
      }),
    )
    expect(apiMocks.apiJSON).toHaveBeenCalledWith(
      '/api/invites',
      expect.objectContaining({ method: 'POST' }),
    )
  })

  it('reconciles the list when resend revokes but the re-create then fails', async () => {
    // Partial failure: the revoke lands (the invite is gone server-side), but
    // the follow-up create throws. The list must still reconcile (load() runs in
    // `finally`) so the stale ghost row doesn't linger as falsely "pending".
    apiMocks.apiFetch.mockImplementation(async (path: string) => {
      if (path === '/api/invites/inv-1/revoke') {
        invitesState = [] // revoked server-side
      }
      return undefined as unknown as Response
    })
    apiMocks.apiJSON.mockImplementation(async (path: string, opts?: { method?: string }) => {
      if (path === '/members') return { members: [OWNER_ROW] }
      if (path === '/api/invites') {
        if (opts?.method === 'POST') throw new HttpError(500, 'boom') // re-create fails
        return invitesState // GET reflects the now-empty (revoked) state
      }
      throw new Error('unexpected apiJSON path: ' + path)
    })

    renderPage()
    await screen.findByText('bob@example.com')

    fireEvent.click(screen.getByRole('button', { name: /resend/i }))

    await waitFor(() => expect(screen.queryByText('bob@example.com')).not.toBeInTheDocument())
    expect(apiMocks.apiFetch).toHaveBeenCalledWith('/api/invites/inv-1/revoke', { method: 'POST' })
  })

  it('opens the invite modal from the + Invite affordance', async () => {
    renderPage()
    await screen.findByText('bob@example.com')

    fireEvent.click(screen.getByRole('button', { name: /^invite$/i }))
    expect(await screen.findByRole('dialog')).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: /invite to organization/i })).toBeInTheDocument()
  })
})
