import { describe, it, expect, vi } from 'vitest'
import { act, renderHook, waitFor } from '@testing-library/react'
import { useWizard } from './useWizard'
import { initialWizardState } from './steps'
import type { WizardStep } from './types'

function step(id: string, persist: WizardStep['persist']): WizardStep {
  return {
    id,
    section: 'org',
    title: id,
    isComplete: () => false,
    persist,
    collapsedSummary: () => '',
    render: () => null,
  }
}

describe('useWizard — hold', () => {
  // A held in-step action (a disconnect) is changing what the step's state
  // says; Continue must not persist and advance on that state until it settles.
  it('refuses to advance while an in-step action holds the wizard', async () => {
    const persist = vi.fn().mockResolvedValue(undefined)
    const steps = [step('first', persist), step('second', vi.fn())]
    const identity = { orgId: 'org-1', teamId: 'team-1', isLocal: true }
    const { result } = renderHook(() => useWizard(steps, identity, initialWizardState, () => {}))
    await waitFor(() => expect(result.current.phase).toBe('ready'))

    let release!: () => void
    let held!: Promise<void>
    act(() => {
      held = result.current.hold(() => new Promise<void>((r) => (release = r)))
    })
    expect(result.current.busy).toBe(true)
    act(() => result.current.advance())
    expect(persist).not.toHaveBeenCalled()
    expect(result.current.activeIndex).toBe(0)

    await act(async () => {
      release()
      await held
    })
    expect(result.current.busy).toBe(false)
    act(() => result.current.advance())
    await waitFor(() => expect(result.current.activeIndex).toBe(1))
    expect(persist).toHaveBeenCalledTimes(1)
  })
})
