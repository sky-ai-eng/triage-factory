import { describe, it, expect, vi, beforeEach } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import PendingPROverlay from './PendingPROverlay'

// The draft PR overlay offers its verbs (edit, Open PR, Reject) to a caller
// the deliverable's routes accept, and reads the draft to one they refuse.

const api = vi.hoisted(() => ({ apiJSON: vi.fn(), apiFetch: vi.fn() }))
vi.mock('../lib/apiClient', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/apiClient')>()
  return { ...actual, ...api }
})

function draft(canWrite: boolean | undefined) {
  return {
    id: 'd1',
    kind: 'pull_request',
    url: 'https://github.com/octo/repo/pull/7',
    state: 'draft',
    ...(canWrite === undefined ? {} : { can_write: canWrite }),
    details: {
      owner: 'octo',
      repo: 'repo',
      number: 7,
      head_branch: 'feature/x',
      base_branch: 'main',
      title: 'Harden the sampler',
      body: 'Adds a retry.',
    },
  }
}

beforeEach(() => {
  api.apiJSON.mockReset()
  api.apiFetch.mockReset()
  api.apiFetch.mockResolvedValue({ text: async () => '', headers: { get: () => null } })
})

async function open(canWrite: boolean | undefined) {
  api.apiJSON.mockResolvedValue(draft(canWrite))
  render(<PendingPROverlay deliverableId="d1" open onClose={() => {}} />)
  expect(await screen.findByText('Harden the sampler')).toBeInTheDocument()
}

describe('PendingPROverlay', () => {
  it('reads the draft to a caller who may not act on it', async () => {
    await open(false)
    expect(screen.queryByRole('button', { name: 'Open PR' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /reject/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Edit' })).not.toBeInTheDocument()
    expect(screen.queryByText('click to edit')).not.toBeInTheDocument()
    expect(screen.getByText(/view only/i)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'View on GitHub' })).toHaveAttribute(
      'href',
      'https://github.com/octo/repo/pull/7',
    )
  })

  it('offers every verb to a writer, and to a response without the field', async () => {
    for (const canWrite of [true, undefined]) {
      cleanup()
      await open(canWrite)
      expect(screen.getByRole('button', { name: 'Open PR' })).toBeInTheDocument()
      expect(screen.getByRole('button', { name: /reject/i })).toBeInTheDocument()
      expect(screen.getByRole('button', { name: 'Edit' })).toBeInTheDocument()
      expect(screen.queryByText(/view only/i)).not.toBeInTheDocument()
    }
  })
})
