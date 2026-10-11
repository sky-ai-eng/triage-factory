// RequireLinearIdentity: the per-user Linear gate. What's pinned is the
// ticket's contract — no gate in an org without Linear, a redirect to the
// Connect page (carrying where the user was headed) when they have no
// credential of their own, and fail-closed on a read error.
import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router'
import { RequireLinearIdentity } from './AuthGate'
import { LOCAL_DEFAULT_ORG_ID } from './lib/githubApp'
import { jsonBody } from './test/apiResponse'
import type { LinearIdentityStatus } from './types'

const identityPath = `/api/orgs/${LOCAL_DEFAULT_ORG_ID}/linear/identity`

function stubIdentity(answers: Array<LinearIdentityStatus | 'error'>) {
  const fetchMock = vi.fn((input: unknown) => {
    if (String(input) !== identityPath) {
      return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
    }
    const next = answers.length > 1 ? answers.shift()! : answers[0]
    if (next === 'error') {
      return Promise.resolve({ ok: false, status: 500, ...jsonBody({ errors: [] }) })
    }
    return Promise.resolve({ ok: true, status: 200, ...jsonBody(next) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function Landed() {
  const location = useLocation()
  return <p>connect page {location.pathname + location.search}</p>
}

function renderGate(at = '/board?filter=mine') {
  return render(
    <MemoryRouter initialEntries={[at]}>
      <Routes>
        <Route path="/orgs/:org_id/connect-linear" element={<Landed />} />
        <Route
          path="*"
          element={
            <RequireLinearIdentity isLocal>
              <p>the app</p>
            </RequireLinearIdentity>
          }
        />
      </Routes>
    </MemoryRouter>,
  )
}

describe('RequireLinearIdentity', () => {
  it('lets everyone through in an org without Linear', async () => {
    stubIdentity([{ connected: false, workspace_url_key: '', connect_available: true }])
    renderGate()
    expect(await screen.findByText('the app')).toBeInTheDocument()
  })

  it('sends a user without their own credential to the Connect page, keeping where they were', async () => {
    stubIdentity([{ connected: false, workspace_url_key: 'acme', connect_available: false }])
    renderGate()
    expect(
      await screen.findByText(
        `connect page /orgs/${LOCAL_DEFAULT_ORG_ID}/connect-linear?return_to=${encodeURIComponent('/board?filter=mine')}`,
      ),
    ).toBeInTheDocument()
    expect(screen.queryByText('the app')).not.toBeInTheDocument()
  })

  it('admits a connected user', async () => {
    stubIdentity([
      { connected: true, account: 'Ada', workspace_url_key: 'acme', connect_available: true },
    ])
    renderGate()
    expect(await screen.findByText('the app')).toBeInTheDocument()
  })

  it('holds a read error rather than rendering through, and retries', async () => {
    const fetchMock = stubIdentity([
      'error',
      { connected: true, account: 'Ada', workspace_url_key: 'acme', connect_available: true },
    ])
    renderGate()
    expect(await screen.findByText(/Couldn.t check your Linear connection/)).toBeInTheDocument()
    expect(screen.queryByText('the app')).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(await screen.findByText('the app')).toBeInTheDocument()
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))
  })
})
