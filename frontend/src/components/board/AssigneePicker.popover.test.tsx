import { describe, it, expect, vi, beforeEach, afterAll } from 'vitest'
import { render, screen, fireEvent, act } from '@testing-library/react'
import type { Task, TeamBot, TeamMember } from '../../types'

// The lifted picker lives in the browser's top layer, and a popover the
// browser hides — which it does, silently, whenever the element leaves the
// document, as a card does when its lane reorders around it — is
// display:none until something shows it again. These pin that opening the
// menu is that something. jsdom has no popover API, so the top-layer route
// is stubbed onto the prototype before the module resolves its feature check.

let shown = false
const showPopover = vi.fn(function (this: HTMLElement) {
  shown = true
})
const hidePopover = vi.fn(function (this: HTMLElement) {
  shown = false
})
Object.defineProperty(HTMLElement.prototype, 'showPopover', {
  value: showPopover,
  configurable: true,
  writable: true,
})
Object.defineProperty(HTMLElement.prototype, 'hidePopover', {
  value: hidePopover,
  configurable: true,
  writable: true,
})
const realMatches = Element.prototype.matches
Element.prototype.matches = function (this: Element, sel: string) {
  if (sel === ':popover-open') return shown
  return realMatches.call(this, sel)
}
afterAll(() => {
  Element.prototype.matches = realMatches
  delete (HTMLElement.prototype as Partial<HTMLElement>).showPopover
  delete (HTMLElement.prototype as Partial<HTMLElement>).hidePopover
})

// The feature check is a module-level constant, so the component has to be
// evaluated AFTER the stubs land. Resetting first means a copy another file in
// this worker already evaluated (with no popover API in sight) is never reused.
vi.resetModules()
const { default: AssigneePicker } = await import('./AssigneePicker')

const ME = {
  user_id: 'u1',
  display_name: 'Aidan Allchin',
  github_username: null,
  jira_account_id: null,
  role: 'member',
  is_current_user: true,
} as TeamMember
const BOT = { agent_id: 'a1', display_name: 'machinist' } as TeamBot

const task = { id: 't1', title: 'x', status: 'queued', memory_pending: false } as Task

const handlers = {
  onClaim: vi.fn(async () => {}),
  onUnclaim: vi.fn(async () => {}),
  onDelegate: vi.fn(),
  onReassign: vi.fn(async () => {}),
}

beforeEach(() => {
  shown = false
  showPopover.mockClear()
  hidePopover.mockClear()
  document.getElementById('tf-assign-scrim')?.remove()
})

describe('AssigneePicker in the top layer', () => {
  it('lifts the picker into the top layer at mount and takes it back down at unmount', () => {
    const { unmount } = render(
      <AssigneePicker task={task} currentUserID="u1" members={[ME]} bot={BOT} {...handlers} />,
    )
    expect(screen.getByRole('button', { name: 'Unassigned' })).toBeInTheDocument()
    expect(showPopover).toHaveBeenCalledTimes(1)
    expect(shown).toBe(true)
    unmount()
    expect(hidePopover).toHaveBeenCalled()
  })

  it('re-shows a popover the browser hid before the menu opens', async () => {
    render(<AssigneePicker task={task} currentUserID="u1" members={[ME]} bot={BOT} {...handlers} />)
    expect(shown).toBe(true)
    // The browser hides a popover whose element left the document — a card
    // moved by its lane's reorder — without any event.
    shown = false
    showPopover.mockClear()
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Unassigned' }))
    })
    expect(showPopover).toHaveBeenCalled()
    expect(shown).toBe(true)
    // jsdom's UA sheet hides a [popover] it cannot open itself, whatever the
    // stubbed state says, so the row is asked for past the accessibility gate.
    expect(screen.getByRole('option', { name: 'Aidan Allchin', hidden: true })).toBeInTheDocument()
  })

  it('leaves an already-shown popover alone when opening', async () => {
    render(<AssigneePicker task={task} currentUserID="u1" members={[ME]} bot={BOT} {...handlers} />)
    showPopover.mockClear()
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Unassigned' }))
    })
    expect(showPopover).not.toHaveBeenCalled()
    expect(shown).toBe(true)
  })
})
