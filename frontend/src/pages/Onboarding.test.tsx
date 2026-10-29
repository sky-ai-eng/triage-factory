import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import Onboarding from './Onboarding'
import type { MeResponse } from '../types'
import { jsonBody } from '../test/apiResponse'

const navigateMock = vi.hoisted(() => vi.fn())
vi.mock('react-router', async (importOriginal) => {
  const actual = await importOriginal<typeof import('react-router')>()
  return { ...actual, useNavigate: () => navigateMock }
})

const authState = vi.hoisted(() => ({ value: {} as Record<string, unknown> }))
vi.mock('../contexts/AuthContext', () => ({ useAuth: () => authState.value }))

function me(overrides: Partial<MeResponse>): MeResponse {
  return {
    id: 'u1',
    email: 'pat@corp.example',
    orgs: [],
    teams: [],
    org_creation_enabled: true,
    can_create_org: true,
    is_operator: false,
    ...overrides,
  }
}

function renderWith(m: MeResponse) {
  authState.value = {
    status: 'authed',
    me: m,
    orgs: [],
    error: null,
    refresh: vi.fn().mockResolvedValue(undefined),
    logout: vi.fn().mockResolvedValue(undefined),
  }
  return render(
    <MemoryRouter>
      <Onboarding />
    </MemoryRouter>,
  )
}

beforeEach(() => {
  navigateMock.mockReset()
})

describe('Onboarding', () => {
  it('offers the create form when the caller can create an org', () => {
    renderWith(me({}))
    expect(screen.getByLabelText(/organization name/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /start your factory/i })).toBeInTheDocument()
  })

  it('shows the invite-only state when creation is disabled on the deployment', () => {
    renderWith(me({ org_creation_enabled: false, can_create_org: false }))
    expect(screen.getByRole('heading', { name: /invitation required/i })).toBeInTheDocument()
    expect(screen.queryByLabelText(/organization name/i)).not.toBeInTheDocument()
  })

  it('tells an SSO session that organizations are created by people signed in with GitHub', () => {
    renderWith(me({ can_create_org: false }))
    expect(screen.getByRole('heading', { name: /welcome to triage factory/i })).toBeInTheDocument()
    expect(
      screen.getByText(/organizations are created by people signed in with github/i),
    ).toBeInTheDocument()
    expect(screen.queryByLabelText(/organization name/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/invitation required/i)).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /log out/i })).toBeInTheDocument()
  })

  it("shows the server's message when create is refused", async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: false,
        status: 403,
        ...jsonBody({
          errors: [
            {
              reason: 'GITHUB_SIGN_IN_REQUIRED',
              message: 'Sign in with GitHub to create an organization.',
            },
          ],
        }),
      }),
    )
    renderWith(me({}))

    fireEvent.change(screen.getByLabelText(/organization name/i), { target: { value: 'Acme' } })
    fireEvent.click(screen.getByRole('button', { name: /start your factory/i }))

    expect(
      await screen.findByText('Sign in with GitHub to create an organization.'),
    ).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /start your factory/i })).toBeEnabled()
  })
})
