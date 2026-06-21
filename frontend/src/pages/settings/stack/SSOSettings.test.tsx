import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { HttpError } from '../../../lib/apiClient'
import type { SSOConnection, SSOConnectionResponse, SSODomain } from '../../../types'

// Mock the apiClient I/O but keep HttpError + httpErrorMessage real — the
// verify path's 409 handling runs through httpErrorMessage to surface the
// server's friendly message.
const apiMocks = vi.hoisted(() => ({ apiJSON: vi.fn(), apiFetch: vi.fn() }))
vi.mock('../../../lib/apiClient', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../../lib/apiClient')>()
  return { ...actual, apiJSON: apiMocks.apiJSON, apiFetch: apiMocks.apiFetch }
})

import SSOSettings from './SSOSettings'

const SP = {
  entity_id: 'https://tf.example.com/auth/v1/sso/saml/metadata',
  acs_url: 'https://tf.example.com/auth/v1/sso/saml/acs',
}

function makeConnection(over: Partial<SSOConnection> = {}): SSOConnection {
  return {
    id: 'conn-1',
    kind: 'saml',
    provider_id: 'prov-uuid-123',
    default_role: 'member',
    enabled: true,
    created_at: '2026-06-01T00:00:00Z',
    updated_at: '2026-06-01T00:00:00Z',
    ...over,
  }
}

function makeDomain(over: Partial<SSODomain> = {}): SSODomain {
  return {
    id: 'dom-1',
    domain: 'example.com',
    txt_host: '_triagefactory-verification.example.com',
    txt_value: 'tok-abc123',
    status: 'pending',
    ...over,
  }
}

// Mutable server state the mock reads/writes so a POST/PATCH/DELETE is reflected
// by the next GET — the same stateful-mock shape OrgPage.test uses.
let connState: SSOConnectionResponse
let domainList: SSODomain[]
// When set, the verify endpoint throws it instead of stamping verified — drives
// the two 409 states (DNS-not-found-yet / already-claimed).
let verifyError: HttpError | null

// seedConnected primes the load with an existing connection (+ optional domains)
// so a test starts past the register step. entityID is overridable to exercise
// Sign-on URL derivation (e.g. a sub-pathed deployment public URL).
function seedConnected(domains: SSODomain[] = [], entityID: string = SP.entity_id) {
  connState = { connection: makeConnection(), entity_id: entityID, acs_url: SP.acs_url }
  domainList = domains
}

function installApi() {
  apiMocks.apiJSON.mockImplementation(
    async (path: string, opts?: { method?: string; body?: string }) => {
      const method = opts?.method ?? 'GET'

      if (path === '/api/sso/connection') {
        if (method === 'GET') return connState
        if (method === 'POST') {
          // Register → a freshly created connection is enabled.
          connState = { ...connState, connection: makeConnection() }
          return connState
        }
        if (method === 'PATCH') {
          const { enabled } = JSON.parse(opts!.body!) as { enabled: boolean }
          connState = {
            ...connState,
            connection: connState.connection ? { ...connState.connection, enabled } : null,
          }
          return connState
        }
      }

      if (path === '/api/sso/domains') {
        if (method === 'GET') return domainList
        if (method === 'POST') {
          const { domain } = JSON.parse(opts!.body!) as { domain: string }
          const created = makeDomain({ id: `dom-${domainList.length + 1}`, domain })
          domainList = [...domainList, created]
          return created
        }
      }

      const verifyMatch = path.match(/^\/api\/sso\/domains\/(.+)\/verify$/)
      if (verifyMatch && method === 'POST') {
        if (verifyError) throw verifyError
        const id = verifyMatch[1]
        const verified: SSODomain = {
          ...domainList.find((d) => d.id === id)!,
          status: 'verified',
          verified_at: '2026-06-02T00:00:00Z',
        }
        domainList = domainList.map((d) => (d.id === id ? verified : d))
        return verified
      }

      throw new Error(`unexpected apiJSON ${method} ${path}`)
    },
  )

  apiMocks.apiFetch.mockImplementation(async (path: string, opts?: { method?: string }) => {
    const delMatch = path.match(/^\/api\/sso\/domains\/(.+)$/)
    if (delMatch && opts?.method === 'DELETE') {
      domainList = domainList.filter((d) => d.id !== delMatch[1])
      return undefined as unknown as Response
    }
    throw new Error(`unexpected apiFetch ${opts?.method} ${path}`)
  })
}

beforeEach(() => {
  apiMocks.apiJSON.mockReset()
  apiMocks.apiFetch.mockReset()
  // Default: no connection yet (the cold-start register flow).
  connState = { connection: null, entity_id: SP.entity_id, acs_url: SP.acs_url }
  domainList = []
  verifyError = null
  window.confirm = vi.fn(() => true)
  // jsdom's window.open is a noisy no-op; stub it so the Test SSO popup launch is
  // assertable and doesn't log "Not implemented".
  window.open = vi.fn()
  Object.defineProperty(navigator, 'clipboard', {
    configurable: true,
    value: { writeText: vi.fn().mockResolvedValue(undefined) },
  })
  installApi()
})

describe('SSOSettings — SP details + connection', () => {
  it('renders the SP Entity ID and ACS URL with copy buttons', async () => {
    render(<SSOSettings />)

    const entity = (await screen.findByLabelText('Identifier (Entity ID)')) as HTMLInputElement
    expect(entity.value).toBe(SP.entity_id)
    const acs = screen.getByLabelText('Reply URL (ACS)') as HTMLInputElement
    expect(acs.value).toBe(SP.acs_url)

    // Copy the Entity ID writes it to the clipboard.
    fireEvent.click(screen.getByRole('button', { name: /copy identifier/i }))
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith(SP.entity_id)

    // The Sign-on URL needs a provider_id, so it's absent until connected.
    expect(screen.queryByLabelText('Sign-on URL')).not.toBeInTheDocument()
  })

  it('derives the Sign-on URL from the Entity ID once connected', async () => {
    seedConnected()
    render(<SSOSettings />)

    const signOn = (await screen.findByLabelText('Sign-on URL')) as HTMLInputElement
    expect(signOn.value).toBe(
      'https://tf.example.com/api/auth/oauth/saml?provider_id=prov-uuid-123&return_to=%2F',
    )
  })

  it('preserves a sub-path in the public URL when deriving the Sign-on URL', async () => {
    // A deployment hosted under a sub-path (TF_PUBLIC_URL = https://example.com/tf)
    // carries it in entity_id; the Sign-on URL must keep it. Deriving from
    // URL().origin would drop /tf and break the My Apps tile — this guards that.
    seedConnected([], 'https://example.com/tf/auth/v1/sso/saml/metadata')
    render(<SSOSettings />)

    const signOn = (await screen.findByLabelText('Sign-on URL')) as HTMLInputElement
    expect(signOn.value).toBe(
      'https://example.com/tf/api/auth/oauth/saml?provider_id=prov-uuid-123&return_to=%2F',
    )
  })

  it('registers a connection and reflects the enabled state', async () => {
    render(<SSOSettings />)

    const input = (await screen.findByLabelText(/app federation metadata url/i)) as HTMLInputElement
    fireEvent.change(input, {
      target: { value: 'https://login.microsoftonline.com/x/federationmetadata.xml' },
    })
    fireEvent.click(screen.getByRole('button', { name: /^connect$/i }))

    await waitFor(() =>
      expect(apiMocks.apiJSON).toHaveBeenCalledWith(
        '/api/sso/connection',
        expect.objectContaining({
          method: 'POST',
          body: expect.stringContaining('federationmetadata.xml'),
        }),
      ),
    )

    // The connected view reflects the enabled connection (toggle on).
    expect(await screen.findByText(/connected · enabled/i)).toBeInTheDocument()
    expect(screen.getByRole('switch', { name: /enable sso/i })).toBeChecked()
    // The domains surface opens once a connection exists.
    expect(screen.getByLabelText(/domain to claim/i)).toBeInTheDocument()
  })

  it('opens the verify-before-enforce test in a popup, even when disabled', async () => {
    // The Test SSO button is offered on a not-yet-enabled connection — testing
    // before enabling is the whole point — and launches the round-trip in a new
    // window (a GET the server 303s through the IdP, untouched session).
    connState = {
      connection: makeConnection({ enabled: false }),
      entity_id: SP.entity_id,
      acs_url: SP.acs_url,
    }
    domainList = []
    render(<SSOSettings />)

    expect(await screen.findByText(/connected · disabled/i)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /test sso/i }))

    // noopener/noreferrer must be present — the popup navigates to the external
    // IdP, so it must not retain a window.opener handle on this tab.
    expect(window.open).toHaveBeenCalledWith(
      '/api/sso/connection/test',
      'tf-sso-test',
      expect.stringMatching(/(?=.*popup)(?=.*noopener)(?=.*noreferrer)/),
    )
  })

  it('toggles the connection disabled via PATCH', async () => {
    seedConnected()
    render(<SSOSettings />)

    const toggle = await screen.findByRole('switch', { name: /enable sso/i })
    expect(toggle).toBeChecked()
    fireEvent.click(toggle)

    await waitFor(() =>
      expect(apiMocks.apiJSON).toHaveBeenCalledWith(
        '/api/sso/connection',
        expect.objectContaining({ method: 'PATCH', body: JSON.stringify({ enabled: false }) }),
      ),
    )
    expect(await screen.findByText(/connected · disabled/i)).toBeInTheDocument()
    expect(screen.getByRole('switch', { name: /enable sso/i })).not.toBeChecked()
  })
})

describe('SSOSettings — domains', () => {
  it('claims a domain and shows the DNS TXT record with copy buttons', async () => {
    seedConnected()
    render(<SSOSettings />)

    const input = (await screen.findByLabelText(/domain to claim/i)) as HTMLInputElement
    fireEvent.change(input, { target: { value: 'example.com' } })
    fireEvent.click(screen.getByRole('button', { name: /add domain/i }))

    // The exact DNS record renders: host + TXT type + value.
    const host = (await screen.findByLabelText('Host / name')) as HTMLInputElement
    expect(host.value).toBe('_triagefactory-verification.example.com')
    expect(screen.getByText('TXT')).toBeInTheDocument()
    const value = screen.getByLabelText('Value') as HTMLInputElement
    expect(value.value).toBe('tok-abc123')

    // Copy the host writes the record name to the clipboard.
    fireEvent.click(screen.getByRole('button', { name: /copy host \/ name/i }))
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith(
      '_triagefactory-verification.example.com',
    )
  })

  it('verifies a pending domain and flips the badge to Verified', async () => {
    seedConnected([makeDomain()])
    render(<SSOSettings />)

    expect(await screen.findByText('Pending')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /^verify$/i }))

    expect(await screen.findByText('Verified')).toBeInTheDocument()
    expect(screen.queryByText('Pending')).not.toBeInTheDocument()
    // The TXT record collapses once verified (nothing left to publish).
    expect(screen.queryByLabelText('Host / name')).not.toBeInTheDocument()
  })

  it('surfaces the "DNS not found yet" 409 as a friendly inline message', async () => {
    seedConnected([makeDomain()])
    verifyError = new HttpError(
      409,
      JSON.stringify({ error: 'DNS record not found yet — propagation can take time, retry.' }),
    )
    render(<SSOSettings />)

    fireEvent.click(await screen.findByRole('button', { name: /^verify$/i }))

    expect(await screen.findByText(/not found yet/i)).toBeInTheDocument()
    // Still pending — no false flip to Verified.
    expect(screen.getByText('Pending')).toBeInTheDocument()
  })

  it('surfaces the "already claimed by another organization" 409 as a friendly message', async () => {
    seedConnected([makeDomain()])
    verifyError = new HttpError(
      409,
      JSON.stringify({ error: 'this domain is already verified by another organization' }),
    )
    render(<SSOSettings />)

    fireEvent.click(await screen.findByRole('button', { name: /^verify$/i }))

    expect(await screen.findByText(/already verified by another organization/i)).toBeInTheDocument()
  })

  it('removes a domain via DELETE', async () => {
    seedConnected([makeDomain()])
    render(<SSOSettings />)

    await screen.findByText('example.com')
    fireEvent.click(screen.getByRole('button', { name: /remove example\.com/i }))

    await waitFor(() =>
      expect(apiMocks.apiFetch).toHaveBeenCalledWith('/api/sso/domains/dom-1', {
        method: 'DELETE',
      }),
    )
    await waitFor(() => expect(screen.queryByText('example.com')).not.toBeInTheDocument())
  })
})
