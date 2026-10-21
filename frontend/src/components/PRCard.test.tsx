import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import PRCard from './PRCard'
import type { PRSummary } from '../pages/PRDashboard'
import { jsonBody } from '../test/apiResponse'

const PR: PRSummary = {
  entity_id: '0b6f4a52-3f0a-4f7e-9a51-5d1c2e3f4a5b',
  number: 7,
  title: 'Mine',
  repo: 'acme/old',
  author: 'octocat',
  state: 'open',
  draft: false,
  labels: [],
  created_at: '2026-10-01T00:00:00Z',
  updated_at: '2026-10-01T00:00:00Z',
  html_url: 'https://github.com/acme/old/pull/7',
}

describe('PRCard', () => {
  // The status route takes the entity id the list row served; the repo and
  // number on the row are display values, and the repository may have been
  // renamed since its key was written.
  it('asks for live status by the pull request entity id', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      ...jsonBody({
        mergeable: true,
        auto_merge: false,
        mergeable_state: 'clean',
        reviews: [],
        checks_status: { total: 0, passing: 0, failing: 0, pending: 0, skipped: 0 },
        conflicts: false,
        review_decision: '',
      }),
    })
    vi.stubGlobal('fetch', fetchMock)
    render(<PRCard pr={PR} />)

    fireEvent.click(screen.getByRole('button', { name: /Mine/ }))

    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        `/api/dashboard/prs/${PR.entity_id}/status`,
        expect.anything(),
      ),
    )
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })
})
