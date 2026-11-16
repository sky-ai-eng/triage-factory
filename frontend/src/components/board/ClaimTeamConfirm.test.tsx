import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import ClaimTeamConfirm from './ClaimTeamConfirm'

// The dialog asks one question, which team takes the task, and what matters
// is what it hands back: the team that was chosen on confirm, and nothing at
// all on cancel. The viewer is on three teams; the task can land on two.

vi.mock('../../hooks/useTeams', () => ({
  useTeams: () => ({
    teams: [
      { id: 't-alpha', name: 'Alpha', slug: 'alpha' },
      { id: 't-beta', name: 'Beta', slug: 'beta' },
      { id: 't-gamma', name: 'Gamma', slug: 'gamma' },
    ],
    lastActingTeamId: '',
    multi: true,
    loaded: true,
  }),
}))

function renderDialog(preselected = 't-alpha') {
  const onConfirm = vi.fn()
  const onCancel = vi.fn()
  render(
    <ClaimTeamConfirm
      open
      candidates={['t-alpha', 't-beta']}
      preselected={preselected}
      onConfirm={onConfirm}
      onCancel={onCancel}
    />,
  )
  return { onConfirm, onCancel }
}

describe('ClaimTeamConfirm', () => {
  it('offers the candidates by name, with the preselected one chosen', () => {
    renderDialog('t-beta')

    expect(screen.getByText('Which team takes this task?')).toBeInTheDocument()
    const options = screen.getAllByRole('radio')
    expect(options.map((o) => o.textContent)).toEqual(['Alpha', 'Beta'])
    expect(screen.getByRole('radio', { name: 'Beta' })).toHaveAttribute('aria-checked', 'true')
    // A team the viewer is on that cannot take the task is not offered.
    expect(screen.queryByText('Gamma')).not.toBeInTheDocument()
  })

  it('confirms with the team that was chosen', () => {
    const { onConfirm, onCancel } = renderDialog('t-alpha')

    fireEvent.click(screen.getByRole('radio', { name: 'Beta' }))
    fireEvent.click(screen.getByRole('button', { name: 'Claim' }))

    expect(onConfirm).toHaveBeenCalledTimes(1)
    expect(onConfirm).toHaveBeenCalledWith('t-beta')
    expect(onCancel).not.toHaveBeenCalled()
  })

  it('confirms with the preselected team when nothing is changed', () => {
    const { onConfirm } = renderDialog('t-beta')

    fireEvent.click(screen.getByRole('button', { name: 'Claim' }))

    expect(onConfirm).toHaveBeenCalledWith('t-beta')
  })

  it('hands back no team on cancel', () => {
    const { onConfirm, onCancel } = renderDialog()

    fireEvent.click(screen.getByRole('radio', { name: 'Beta' }))
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))

    expect(onCancel).toHaveBeenCalledTimes(1)
    expect(onConfirm).not.toHaveBeenCalled()
  })

  it('hands back no team on Escape', () => {
    const { onConfirm, onCancel } = renderDialog()

    fireEvent.keyDown(window, { key: 'Escape' })

    expect(onCancel).toHaveBeenCalledTimes(1)
    expect(onConfirm).not.toHaveBeenCalled()
  })
})
