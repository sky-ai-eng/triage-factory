import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import Login from './Login'

// Keep MemoryRouter/useLocation real (so ?return_to= resolves), capture navigate.
const navigateMock = vi.hoisted(() => vi.fn())
vi.mock('react-router', async (importOriginal) => {
  const actual = await importOriginal<typeof import('react-router')>()
  return { ...actual, useNavigate: () => navigateMock }
})

// Drive auth status per test.
const authState = vi.hoisted(() => ({
  value: { status: 'unauth' as string, error: null as unknown },
}))
vi.mock('../contexts/AuthContext', () => ({ useAuth: () => authState.value }))

// Mock the discovery I/O; keep the rest of apiClient real.
const api = vi.hoisted(() => ({ apiJSON: vi.fn() }))
vi.mock('../lib/apiClient', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/apiClient')>()
  return { ...actual, apiJSON: api.apiJSON }
})

function renderAt(path = '/login') {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Login />
    </MemoryRouter>,
  )
}

const emailField = () => screen.getByLabelText(/work email/i)
const continueBtn = () => screen.getByRole('button', { name: /continue/i })
const githubBtn = () => screen.getByRole('button', { name: /sign in with github/i })

beforeEach(() => {
  navigateMock.mockReset()
  api.apiJSON.mockReset()
  authState.value = { status: 'unauth', error: null }
  vi.stubGlobal('location', { href: '' })
})

describe('Login (identifier-first, TFAC-427)', () => {
  it('renders the email field and the always-present GitHub button', () => {
    renderAt()
    expect(emailField()).toBeInTheDocument()
    expect(githubBtn()).toBeInTheDocument()
    // Continue is gated until an email is typed.
    expect(continueBtn()).toBeDisabled()
    fireEvent.change(emailField(), { target: { value: 'a@b.com' } })
    expect(continueBtn()).toBeEnabled()
  })

  it('verified domain → discovery returns sso:true → redirects into the SAML start_url', async () => {
    const startURL = '/api/auth/oauth/saml?provider_id=prov-1&return_to=%2F'
    api.apiJSON.mockResolvedValue({ sso: true, start_url: startURL })
    renderAt()

    fireEvent.change(emailField(), { target: { value: 'alice@corp.com' } })
    fireEvent.click(continueBtn())

    await waitFor(() => expect(window.location.href).toBe(startURL))
    expect(api.apiJSON).toHaveBeenCalledWith(
      '/api/sso/discover',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({ email: 'alice@corp.com', return_to: '/' }),
      }),
    )
  })

  it('unknown domain → discovery returns sso:false → GitHub fallback shown, no redirect', async () => {
    api.apiJSON.mockResolvedValue({ sso: false })
    renderAt()

    fireEvent.change(emailField(), { target: { value: 'bob@unknown-co.com' } })
    fireEvent.click(continueBtn())

    expect(await screen.findByText(/no single sign-on for that email/i)).toBeInTheDocument()
    // Stayed put — no SSO redirect — and GitHub is still right there.
    expect(window.location.href).toBe('')
    expect(githubBtn()).toBeInTheDocument()
  })

  it('passes the return_to from the query string into discovery', async () => {
    api.apiJSON.mockResolvedValue({ sso: false })
    renderAt('/login?return_to=%2Fteams')

    fireEvent.change(emailField(), { target: { value: 'x@corp.com' } })
    fireEvent.click(continueBtn())

    await waitFor(() =>
      expect(api.apiJSON).toHaveBeenCalledWith(
        '/api/sso/discover',
        expect.objectContaining({
          body: JSON.stringify({ email: 'x@corp.com', return_to: '/teams' }),
        }),
      ),
    )
  })

  it('GitHub button → redirects to the GitHub OAuth start with return_to', () => {
    renderAt('/login?return_to=%2Fdashboard')
    fireEvent.click(githubBtn())
    expect(window.location.href).toContain('/api/auth/oauth/github?return_to=')
    expect(window.location.href).toContain(encodeURIComponent('/dashboard'))
  })

  it('discovery failure → shows a retry hint (not a false "no SSO"), never traps the user', async () => {
    api.apiJSON.mockRejectedValue(new Error('network down'))
    renderAt()

    fireEvent.change(emailField(), { target: { value: 'x@corp.com' } })
    fireEvent.click(continueBtn())

    // A transient failure must NOT claim the domain has no SSO — it shows the
    // "couldn't check" copy instead, and GitHub stays available.
    expect(await screen.findByText(/couldn.t check single sign-on/i)).toBeInTheDocument()
    expect(screen.queryByText(/no single sign-on for that email/i)).not.toBeInTheDocument()
    expect(window.location.href).toBe('')
    expect(githubBtn()).toBeInTheDocument()
  })

  it('already authed → bounces to /', () => {
    authState.value = { status: 'authed', error: null }
    renderAt()
    expect(navigateMock).toHaveBeenCalledWith('/', { replace: true })
  })

  it('?error=login_failed → shows a recoverable "try again" banner, GitHub still available', () => {
    renderAt('/login?error=login_failed')
    expect(screen.getByText(/sign-in couldn.t be completed/i)).toBeInTheDocument()
    expect(githubBtn()).toBeInTheDocument()
  })

  it('?error=login_cancelled → shows the gentler "cancelled" banner', () => {
    renderAt('/login?error=login_cancelled')
    expect(screen.getByText(/sign-in was cancelled/i)).toBeInTheDocument()
    expect(githubBtn()).toBeInTheDocument()
  })
})
