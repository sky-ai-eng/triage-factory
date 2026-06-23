import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import ArchiveTeamModal from './ArchiveTeamModal'
import * as lifecycle from '../lib/teamLifecycle'

vi.mock('../lib/teamLifecycle', () => ({
  fetchArchivePreview: vi.fn(),
  archiveTeam: vi.fn(),
}))

const preview = lifecycle.fetchArchivePreview as unknown as ReturnType<typeof vi.fn>
const archive = lifecycle.archiveTeam as unknown as ReturnType<typeof vi.fn>

describe('ArchiveTeamModal', () => {
  beforeEach(() => {
    preview.mockReset()
    archive.mockReset()
  })

  it('surfaces the active-work counts from the preview', async () => {
    preview.mockResolvedValue({
      team_id: 't1',
      name: 'Platform',
      archived: false,
      active_runs: 3,
      active_curator_sessions: 1,
    })
    render(<ArchiveTeamModal teamId="t1" teamName="Platform" onDone={vi.fn()} onClose={vi.fn()} />)

    expect(await screen.findByText(/3 active delegations/)).toBeInTheDocument()
    // Singular curator session, pluralization respected.
    expect(screen.getByText(/1 curator session\b/)).toBeInTheDocument()
  })

  it('archives on confirm and reports the cancelled counts', async () => {
    preview.mockResolvedValue({
      team_id: 't1',
      name: 'Platform',
      archived: false,
      active_runs: 2,
      active_curator_sessions: 0,
    })
    archive.mockResolvedValue({ cancelled_runs: 2, cancelled_curator_sessions: 0 })
    const onDone = vi.fn()
    render(<ArchiveTeamModal teamId="t1" teamName="Platform" onDone={onDone} onClose={vi.fn()} />)

    await screen.findByText(/2 active delegations/)
    fireEvent.click(screen.getByRole('button', { name: /Archive team/ }))

    await waitFor(() => {
      expect(archive).toHaveBeenCalledWith('t1')
      expect(onDone).toHaveBeenCalledWith(2, 0)
    })
  })

  it('disables the confirm and shows the error when the preview fails to load', async () => {
    preview.mockRejectedValue(new Error('boom'))
    render(<ArchiveTeamModal teamId="t1" teamName="Platform" onDone={vi.fn()} onClose={vi.fn()} />)

    expect(await screen.findByText('boom')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /Archive team/ })).toBeDisabled()
    expect(archive).not.toHaveBeenCalled()
  })
})
