import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'

// The control renders only in multi mode, where useOptionalAuth is non-null.
vi.mock('../contexts/AuthContext', () => ({ useOptionalAuth: () => ({}) as unknown }))

import MarketplacePublishControl from './MarketplacePublishControl'
import { jsonBody } from '../test/apiResponse'

const listing = (status: 'published' | 'delisted') => ({
  id: 'l1',
  kind: 'prompt',
  status,
  name: 'Fix CI',
  description: '',
  current_version: 1,
  event_types: [],
})

// stubListing answers the by-source lookup with a listing in status, and the
// status write with the listing in the status the body asked for.
function stubListing(status: 'published' | 'delisted') {
  const fetchMock = vi.fn(async (path: string, init?: RequestInit) => {
    if (path === '/api/marketplace/listings/by-source/p1') {
      return { ok: true, status: 200, ...jsonBody(listing(status)) }
    }
    if (path === '/api/marketplace/listings/l1' && init?.method === 'PATCH') {
      const body = JSON.parse(String(init.body)) as { status: 'published' | 'delisted' }
      return { ok: true, status: 200, ...jsonBody(listing(body.status)) }
    }
    return { ok: false, status: 404, ...jsonBody({ errors: [] }) }
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('MarketplacePublishControl', () => {
  it('delists a published listing by setting its status', async () => {
    const fetchMock = stubListing('published')
    render(<MarketplacePublishControl kind="prompt" sourceId="p1" defaultName="Fix CI" />)

    fireEvent.click(await screen.findByRole('button', { name: 'Delist' }))

    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/marketplace/listings/l1',
        expect.objectContaining({ method: 'PATCH', body: JSON.stringify({ status: 'delisted' }) }),
      ),
    )
  })

  it('relists a delisted listing by setting its status to published', async () => {
    const fetchMock = stubListing('delisted')
    render(<MarketplacePublishControl kind="prompt" sourceId="p1" defaultName="Fix CI" />)

    fireEvent.click(await screen.findByRole('button', { name: 'Relist' }))

    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/marketplace/listings/l1',
        expect.objectContaining({ method: 'PATCH', body: JSON.stringify({ status: 'published' }) }),
      ),
    )
  })
})
