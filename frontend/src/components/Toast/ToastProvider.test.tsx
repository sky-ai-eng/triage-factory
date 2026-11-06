import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { MemoryRouter, useLocation } from 'react-router'
import { installFakeAnimations } from '../../ui/shared/fakeAnimation'
import ToastProvider from './ToastProvider'
import { toast, toastStore } from './toastStore'

vi.mock('../../hooks/useWebSocket', () => ({ useWebSocket: () => {} }))

function Where() {
  return <span data-testid="where">{useLocation().pathname}</span>
}

describe('ToastProvider', () => {
  beforeEach(() => {
    installFakeAnimations()
    vi.stubGlobal(
      'matchMedia',
      vi.fn().mockImplementation((query: string) => ({
        matches: false,
        media: query,
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
      })),
    )
  })
  afterEach(() => {
    for (const t of toastStore.getState()) toastStore.dismiss(t.id)
  })

  // The store sits outside React, so an action that carries only an in-app
  // href is routed here; a plain click must not reload the app.
  it('follows an in-app action through the router', () => {
    render(
      <MemoryRouter initialEntries={['/orgs/o1/board']}>
        <ToastProvider />
        <Where />
      </MemoryRouter>,
    )
    act(() => {
      toast.error('Taken.', undefined, {
        action: { label: 'Back to Acme knowledge', href: '/orgs/o1/knowledge' },
      })
    })
    fireEvent.click(screen.getByText('Back to Acme knowledge'), { button: 0 })
    expect(screen.getByTestId('where').textContent).toBe('/orgs/o1/knowledge')
  })
})
