// The Jira gate page's explanation of a lost connection. Pinned: each reason
// the status reports gets the sentence a Jira action's 409 uses, a user who
// never connected gets no banner, and a Connect callback error wins over the
// reason because it describes the attempt the user just made.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router'
import ConnectJira from './ConnectJira'
import { jsonBody } from '../test/apiResponse'
import type { JiraIdentityStatus } from '../types'

const ORG = 'org-1'
const identityPath = `/api/orgs/${ORG}/jira/identity`

let identity: JiraIdentityStatus

function stubBackend() {
  const fetchMock = vi.fn((input: unknown) => {
    if (String(input) === identityPath) {
      return Promise.resolve({ ok: true, status: 200, ...jsonBody(identity) })
    }
    return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
  })
  vi.stubGlobal('fetch', fetchMock)
}

function renderPage(search = '') {
  return render(
    <MemoryRouter initialEntries={[`/orgs/${ORG}/connect-jira${search}`]}>
      <Routes>
        <Route path="/orgs/:org_id/connect-jira" element={<ConnectJira />} />
      </Routes>
    </MemoryRouter>,
  )
}

beforeEach(() => {
  identity = {
    connected: false,
    host: 'https://acme.atlassian.net',
    connect_available: true,
    deployment: 'cloud',
  }
})

describe('ConnectJira', () => {
  it.each([
    [
      'app_removed',
      'The Jira app your connection was made through was removed from this workspace. Connect your Jira again to act on tickets.',
    ],
    [
      'refused',
      'Jira no longer accepts your connection: it was revoked or expired. Connect your Jira again to act on tickets.',
    ],
  ] as const)('says why the connection stopped working (%s)', async (reason, text) => {
    identity = { ...identity, disconnected_reason: reason }
    stubBackend()
    renderPage()
    expect(await screen.findByRole('alert')).toHaveTextContent(text)
  })

  it('shows no banner without a reason', async () => {
    stubBackend()
    renderPage()
    expect(await screen.findByRole('button', { name: 'Connect Jira' })).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('shows no banner for a reason it does not know', async () => {
    identity = {
      ...identity,
      disconnected_reason: 'cutover' as unknown as JiraIdentityStatus['disconnected_reason'],
    }
    stubBackend()
    renderPage()
    expect(await screen.findByRole('button', { name: 'Connect Jira' })).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('shows the callback error instead of the reason', async () => {
    identity = { ...identity, disconnected_reason: 'refused' }
    stubBackend()
    renderPage('?error=denied')
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(/authorization was cancelled/)
    expect(alert).not.toHaveTextContent(/revoked or expired/)
  })
})
