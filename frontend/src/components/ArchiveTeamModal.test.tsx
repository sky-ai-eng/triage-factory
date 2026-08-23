import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import ArchiveTeamModal from './ArchiveTeamModal'
import * as lifecycle from '../lib/teamLifecycle'

vi.mock('../lib/teamLifecycle', () => ({
  archiveTeam: vi.fn(),
}))

const archive = lifecycle.archiveTeam as unknown as ReturnType<typeof vi.fn>

// The modal opens already holding its consequences preview — the opener
// fetches it first and withdraws on failure (that arm is pinned in the
// openers' own tests). What is left to pin here is that the counts it was
// handed are the counts it states, and what confirm does.
const PREVIEW = {
  team_id: 't1',
  name: 'Platform',
  archived: false,
  active_runs: 3,
}

describe('ArchiveTeamModal', () => {
  beforeEach(() => {
    archive.mockReset()
  })

  it('states the active-work counts it was handed, whole from the first frame', () => {
    render(
      <ArchiveTeamModal
        teamId="t1"
        teamName="Platform"
        preview={PREVIEW}
        onDone={vi.fn()}
        onClose={vi.fn()}
      />,
    )

    expect(screen.getByText(/3 active delegations/)).toBeInTheDocument()
    // Nothing to wait for: the confirm is live immediately.
    expect(screen.getByRole('button', { name: /Archive team/ })).toBeEnabled()
  })

  it('archives on confirm and reports the cancelled count', async () => {
    archive.mockResolvedValue({ cancelled_runs: 2 })
    const onDone = vi.fn()
    render(
      <ArchiveTeamModal
        teamId="t1"
        teamName="Platform"
        preview={{ ...PREVIEW, active_runs: 2 }}
        onDone={onDone}
        onClose={vi.fn()}
      />,
    )

    fireEvent.click(screen.getByRole('button', { name: /Archive team/ }))

    await waitFor(() => {
      expect(archive).toHaveBeenCalledWith('t1')
      expect(onDone).toHaveBeenCalledWith(2)
    })
  })

  it('surfaces the refusal inline when the archive itself fails', async () => {
    archive.mockRejectedValue(new Error('still winding down'))
    const onDone = vi.fn()
    render(
      <ArchiveTeamModal
        teamId="t1"
        teamName="Platform"
        preview={PREVIEW}
        onDone={onDone}
        onClose={vi.fn()}
      />,
    )

    fireEvent.click(screen.getByRole('button', { name: /Archive team/ }))

    expect(await screen.findByText('still winding down')).toBeInTheDocument()
    expect(onDone).not.toHaveBeenCalled()
    // The confirm recovers for a retry rather than wedging disabled.
    expect(screen.getByRole('button', { name: /Archive team/ })).toBeEnabled()
  })
})
