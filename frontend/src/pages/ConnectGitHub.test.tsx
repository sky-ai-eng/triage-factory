// The GitHub gate page. Pinned: multi mode offers Connect and never a token
// field — not even when Connect is not advertised — local mode keeps the token
// paste beside Connect, a failed Connect explains itself through the callback's
// error code, and a connected user is sent on to where they were headed.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router'
import ConnectGitHub from './ConnectGitHub'
import { jsonBody } from '../test/apiResponse'
import type { GitHubIdentityStatus } from '../types'

const ORG = 'org-1'
const identityPath = `/api/orgs/${ORG}/github/identity`

let identity: GitHubIdentityStatus

function stubBackend() {
  const fetchMock = vi.fn((input: unknown, init?: { method?: string }) => {
    const path = String(input)
    if (path === identityPath && (init?.method ?? 'GET') === 'GET') {
      return Promise.resolve({ ok: true, status: 200, ...jsonBody(identity) })
    }
    if (path === identityPath + '/pat' && init?.method === 'POST') {
      return Promise.resolve({
        ok: true,
        status: 200,
        ...jsonBody({ login: 'octocat', host: 'https://github.com' }),
      })
    }
    return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function renderPage({ isLocal = false, search = '' } = {}) {
  return render(
    <MemoryRouter initialEntries={[`/orgs/${ORG}/connect-github${search}`]}>
      <Routes>
        <Route path="/orgs/:org_id/connect-github" element={<ConnectGitHub isLocal={isLocal} />} />
        <Route path="/orgs/:org_id/board" element={<p>the board</p>} />
        <Route path="/orgs/:org_id" element={<p>the org home</p>} />
      </Routes>
    </MemoryRouter>,
  )
}

beforeEach(() => {
  identity = { connected: false, host: 'https://github.com', connect_available: true }
})

describe('ConnectGitHub — multi mode', () => {
  it('offers Connect and no token field', async () => {
    stubBackend()
    renderPage()
    expect(await screen.findByRole('button', { name: 'Connect GitHub' })).toBeInTheDocument()
    expect(screen.queryByPlaceholderText('ghp_…')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Verify token' })).not.toBeInTheDocument()
  })

  it('offers no token field when Connect is not advertised, and says why', async () => {
    identity = { ...identity, connect_available: false }
    stubBackend()
    renderPage()
    expect(await screen.findByText(/isn.t connected for this organization yet/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Connect GitHub' })).not.toBeInTheDocument()
    expect(screen.queryByPlaceholderText('ghp_…')).not.toBeInTheDocument()
  })

  it.each([
    ['deployment_app_unavailable', /deployment.s GitHub App can.t connect accounts/],
    ['sso_required', /requires signing in through its single sign-on/],
    ['no_app', /doesn.t have a GitHub App that can connect your account/],
    ['denied', /cancelled before it finished/],
  ])('explains a failed Connect (%s)', async (code, text) => {
    stubBackend()
    renderPage({ search: `?error=${code}` })
    expect(await screen.findByText(text)).toBeInTheDocument()
  })

  it('sends a connected user on to where they were headed', async () => {
    identity = {
      connected: true,
      login: 'octocat',
      host: 'https://github.com',
      connect_available: true,
    }
    stubBackend()
    renderPage({ search: `?return_to=${encodeURIComponent(`/orgs/${ORG}/board`)}` })
    expect(await screen.findByText('the board')).toBeInTheDocument()
  })
})

describe('ConnectGitHub — local mode', () => {
  it('keeps the token paste beside Connect, and binds through it', async () => {
    const fetchMock = stubBackend()
    renderPage({ isLocal: true })
    expect(await screen.findByRole('button', { name: 'Connect GitHub' })).toBeInTheDocument()
    fireEvent.change(screen.getByPlaceholderText('ghp_…'), { target: { value: 'ghp_local' } })
    fireEvent.click(screen.getByRole('button', { name: 'Verify token' }))
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        identityPath + '/pat',
        expect.objectContaining({ method: 'POST' }),
      ),
    )
  })

  it('offers the token paste alone when no App is registered', async () => {
    identity = { ...identity, connect_available: false }
    stubBackend()
    renderPage({ isLocal: true })
    expect(await screen.findByPlaceholderText('ghp_…')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Connect GitHub' })).not.toBeInTheDocument()
  })
})
