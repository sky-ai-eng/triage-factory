// The Linear gate page. Pinned: Connect is offered only when an app resolves,
// the key paste always is, a refused Connect explains itself (including the
// admin-approval caveat on `denied`), a pasted key binds through the user's
// own route, and a connected user is sent on to where they were headed, but
// never off this origin.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router'
import ConnectLinear from './ConnectLinear'
import { jsonBody } from '../test/apiResponse'
import type { LinearIdentityStatus } from '../types'

const ORG = 'org-1'
const identityPath = `/api/orgs/${ORG}/linear/identity`

let identity: LinearIdentityStatus
let pasteAnswer: { ok: boolean; status: number; body: unknown }

function stubBackend() {
  const fetchMock = vi.fn((input: unknown, init?: { method?: string }) => {
    const path = String(input)
    if (path === identityPath && (init?.method ?? 'GET') === 'GET') {
      return Promise.resolve({ ok: true, status: 200, ...jsonBody(identity) })
    }
    if (path === identityPath + '/api-key' && init?.method === 'POST') {
      return Promise.resolve({
        ok: pasteAnswer.ok,
        status: pasteAnswer.status,
        ...jsonBody(pasteAnswer.body),
      })
    }
    return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function renderPage(search = '') {
  return render(
    <MemoryRouter initialEntries={[`/orgs/${ORG}/connect-linear${search}`]}>
      <Routes>
        <Route path="/orgs/:org_id/connect-linear" element={<ConnectLinear />} />
        <Route path="/orgs/:org_id/board" element={<p>the board</p>} />
        <Route path="/orgs/:org_id" element={<p>the org home</p>} />
      </Routes>
    </MemoryRouter>,
  )
}

beforeEach(() => {
  identity = { connected: false, workspace_url_key: 'acme', connect_available: true }
  pasteAnswer = { ok: true, status: 200, body: { account: 'Ada', workspace_url_key: 'acme' } }
})

describe('ConnectLinear', () => {
  it('offers Connect and the key paste when an app resolves, and says the credential is stored', async () => {
    stubBackend()
    renderPage()
    expect(await screen.findByRole('button', { name: 'Connect Linear' })).toBeInTheDocument()
    expect(screen.getByText('Or paste a personal API key')).toBeInTheDocument()
    expect(screen.getByText(/linear\.app\/acme/)).toBeInTheDocument()
    expect(document.body.textContent).toMatch(/stored in your workspace.s secret store/)
  })

  it('offers only the key paste with no app', async () => {
    identity = { ...identity, connect_available: false }
    stubBackend()
    renderPage()
    expect(await screen.findByText('Paste a personal API key')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Connect Linear' })).not.toBeInTheDocument()
  })

  it.each([
    ['denied', /admin to approve Triage Factory/],
    ['workspace_mismatch', /different Linear workspace/],
    ['no_app', /needs a Linear OAuth app/],
    ['not_configured', /hasn.t connected Linear yet/],
    ['state', /expired/],
    ['connect_failed', /Something went wrong/],
  ])('explains a refused Connect (%s)', async (code, text) => {
    stubBackend()
    renderPage(`?error=${code}`)
    expect(await screen.findByRole('alert')).toHaveTextContent(text)
  })

  it('binds a pasted key through the user route, then goes where the user was headed', async () => {
    const fetchMock = stubBackend()
    renderPage(`?return_to=${encodeURIComponent(`/orgs/${ORG}/board`)}`)
    const input = await screen.findByPlaceholderText('lin_api_…')
    fireEvent.change(input, { target: { value: ' lin_api_ada ' } })
    identity = {
      connected: true,
      account: 'Ada',
      workspace_url_key: 'acme',
      connect_available: true,
    }
    fireEvent.click(screen.getByRole('button', { name: 'Verify key' }))

    expect(await screen.findByText('the board')).toBeInTheDocument()
    const post = fetchMock.mock.calls.find(([, init]) => init?.method === 'POST')
    expect(post?.[0]).toBe(identityPath + '/api-key')
    expect(JSON.parse(String((post?.[1] as { body: string }).body))).toEqual({
      api_key: 'lin_api_ada',
    })
  })

  it('shows a refused key inline and stays put', async () => {
    pasteAnswer = {
      ok: false,
      status: 422,
      body: {
        errors: [
          {
            reason: 'UPSTREAM_REJECTED',
            message:
              'This key belongs to a different Linear workspace than the one connected to Triage Factory.',
            field: 'api_key',
          },
        ],
      },
    }
    stubBackend()
    renderPage()
    fireEvent.change(await screen.findByPlaceholderText('lin_api_…'), {
      target: { value: 'lin_api_other' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Verify key' }))
    expect(await screen.findByText(/different Linear workspace/)).toBeInTheDocument()
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Verify key' })).not.toBeDisabled(),
    )
  })

  it('sends an already-connected user on', async () => {
    identity = {
      connected: true,
      account: 'Ada',
      workspace_url_key: 'acme',
      connect_available: true,
    }
    stubBackend()
    renderPage()
    expect(await screen.findByText('the org home')).toBeInTheDocument()
  })

  it('never sends a connected user off this origin', async () => {
    identity = {
      connected: true,
      account: 'Ada',
      workspace_url_key: 'acme',
      connect_available: true,
    }
    stubBackend()
    // Decodes to /\evil.example, which a browser resolves to evil.example.
    renderPage('?return_to=/%5Cevil.example')
    expect(await screen.findByText('the org home')).toBeInTheDocument()
  })
})
