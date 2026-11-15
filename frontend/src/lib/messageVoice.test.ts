import { describe, expect, it } from 'vitest'
import type { Message } from '../types'
import { isSystemNotice } from './messageVoice'

function row(role: string, subtype = ''): Message {
  return { id: 1, conversation_id: 'c', role, content: 'x', subtype } as Message
}

describe('isSystemNotice', () => {
  it('reads what a person typed as theirs', () => {
    expect(isSystemNotice(row('user'))).toBe(false)
    expect(isSystemNotice(row('user', 'injection:steer'))).toBe(false)
  })

  it('reads a user row the system wrote as the system', () => {
    expect(isSystemNotice(row('user', 'injection:executor-changed'))).toBe(true)
  })

  it('reads an update to the instructions as the system', () => {
    expect(isSystemNotice(row('system', 'injection:system-update'))).toBe(true)
  })

  it('leaves the agent its own rows', () => {
    expect(isSystemNotice(row('assistant'))).toBe(false)
  })
})
