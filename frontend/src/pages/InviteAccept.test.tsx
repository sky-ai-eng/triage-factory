import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import InviteAccept from './InviteAccept'
import { HttpError } from '../lib/apiClient'

// Capture navigate; keep the rest of react-router (MemoryRouter, useLocation)
// real so the ?token= query resolves normally.
const navigateMock = vi.hoisted(() => vi.fn())
vi.mock('react-router', async (importOriginal) => {
  const actual = await importOriginal<typeof import('react-router')>()
  return { ...actual, useNavigate: () => navigateMock }
})

// Drive auth status per test.
const authState = vi.hoisted(() => ({
  value: {
    status: 'unauth' as string,
    refresh: vi.fn(),
    logout: vi.fn(),
  },
}))
vi.mock('../contexts/AuthContext', () => ({ useAuth: () => authState.value }))

// Mock the preview/accept I/O; keep HttpError + httpErrorMessage real.
const api = vi.hoisted(() => ({ apiJSON: vi.fn() }))
vi.mock('../lib/apiClient', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/apiClient')>()
  return { ...actual, apiJSON: api.apiJSON }
})

function renderAt(token = 'tok-123') {
  return render(
    <MemoryRouter initialEntries={[`/invite/accept?token=${token}`]}>
      <InviteAccept />
    </MemoryRouter>,
  )
}

beforeEach(() => {
  navigateMock.mockReset()
  api.apiJSON.mockReset()
  authState.value = {
    status: 'unauth',
    refresh: vi.fn().mockResolvedValue(undefined),
    logout: vi.fn().mockResolvedValue(undefined),
  }
  vi.stubGlobal('location', { href: '' })
})

const TERMINAL_STATUSES = ['expired', 'revoked', 'accepted', 'not_found'] as const

describe('InviteAccept', () => {
  it.each(TERMINAL_STATUSES)('renders the terminal dead-end for a %s invite', async (status) => {
    api.apiJSON.mockResolvedValue({ status })
    renderAt()
    expect(await screen.findByText(/this invitation isn.t available/i)).toBeInTheDocument()
  })

  it('shows an invalid-link state when the token is missing', async () => {
    renderAt('')
    expect(await screen.findByText(/invalid invite link/i)).toBeInTheDocument()
    // No preview is fetched without a token.
    expect(api.apiJSON).not.toHaveBeenCalled()
  })

  it('valid + logged out → Sign in to accept bounces with the token in return_to', async () => {
    authState.value.status = 'unauth'
    api.apiJSON.mockResolvedValue({ status: 'valid', org_name: 'Acme', role: 'member' })
    renderAt('tok-xyz')

    expect(await screen.findByRole('heading', { name: /you.ve been invited/i })).toBeInTheDocument()
    expect(screen.getByText('Acme')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: /sign in to accept/i }))
    expect(window.location.href).toContain('/api/auth/oauth/github?return_to=')
    expect(window.location.href).toContain(encodeURIComponent('/invite/accept?token=tok-xyz'))
  })

  it('valid + logged in → Accept posts the token and routes into the org', async () => {
    authState.value.status = 'authed'
    const refresh = authState.value.refresh
    api.apiJSON.mockImplementation(async (path: string) => {
      if (path.startsWith('/api/invites/preview'))
        return { status: 'valid', org_name: 'Acme', role: 'admin' }
      if (path === '/api/invites/accept') return { org_id: 'org-9' }
      throw new Error('unexpected path: ' + path)
    })
    renderAt('tok-9')

    fireEvent.click(await screen.findByRole('button', { name: /accept invitation/i }))

    await waitFor(() =>
      expect(api.apiJSON).toHaveBeenCalledWith(
        '/api/invites/accept',
        expect.objectContaining({ method: 'POST', body: JSON.stringify({ token: 'tok-9' }) }),
      ),
    )
    await waitFor(() => expect(refresh).toHaveBeenCalled())
    expect(navigateMock).toHaveBeenCalledWith('/orgs/org-9', { replace: true })
  })

  it('409 wrong-identity → shows the actionable message and a switch-account action', async () => {
    authState.value.status = 'authed'
    const logout = authState.value.logout
    api.apiJSON.mockImplementation(async (path: string) => {
      if (path.startsWith('/api/invites/preview'))
        return { status: 'valid', org_name: 'Acme', role: 'member' }
      if (path === '/api/invites/accept')
        throw new HttpError(
          409,
          JSON.stringify({
            errors: [
              {
                reason: 'INVITE_EMAIL_MISMATCH',
                message:
                  'This invitation was sent to bob@example.com, but you are signed in as me@example.com.',
              },
            ],
          }),
        )
      throw new Error('unexpected path: ' + path)
    })
    renderAt()

    fireEvent.click(await screen.findByRole('button', { name: /accept invitation/i }))

    expect(
      await screen.findByText(/this invitation was sent to bob@example.com/i),
    ).toBeInTheDocument()
    fireEvent.click(
      screen.getByRole('button', { name: /log out and sign in with the invited account/i }),
    )

    await waitFor(() => expect(logout).toHaveBeenCalled())
    expect(window.location.href).toContain('/api/auth/oauth/github')
  })
})
