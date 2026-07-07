import { describe, it, expect } from 'vitest'
import type { AgentMessage } from '../types'
import { appendToFeed, feedFromMessages, EMPTY_FEED } from './runFeed'

let nextID = 1
function msg(over: Partial<AgentMessage>): AgentMessage {
  return {
    ID: nextID++,
    RunID: 'r1',
    Role: 'assistant',
    Content: '',
    Subtype: 'text',
    ToolCallID: '',
    IsError: false,
    Model: '',
    CreatedAt: '2026-07-07T12:00:00Z',
    ...over,
  }
}

describe('appendToFeed', () => {
  it('returns the SAME reference for a display-no-op message (the WS handler skips the state write on identity)', () => {
    const feed = appendToFeed(undefined, msg({ Content: 'hello', OutputTokens: 5 }))
    // A tool-result row: no ticker line, no tokens, no comment delta.
    const after = appendToFeed(feed, msg({ Role: 'tool', ToolCallID: 'tc-1', Content: 'ok' }))
    expect(after).toBe(feed)
    // Same contract from an empty start: base EMPTY_FEED comes back untouched.
    expect(appendToFeed(undefined, msg({ Role: 'tool', ToolCallID: 'tc-2' }))).toBe(EMPTY_FEED)
  })

  it('accumulates tokens, comments, and ticker lines', () => {
    let feed = appendToFeed(
      undefined,
      msg({ Content: 'planning', InputTokens: 100, OutputTokens: 10 }),
    )
    feed = appendToFeed(
      feed,
      msg({
        Subtype: 'tool_use',
        ToolCalls: [
          {
            id: 'tc-1',
            name: 'Bash',
            input: { command: 'triagefactory exec gh pr add-comment', description: '' },
          },
        ],
        InputTokens: 50,
      }),
    )
    expect(feed.tokens).toBe(160)
    expect(feed.comments).toBe(1)
    expect(feed.lines.map((l) => l.text)).toEqual(['planning', 'Adding comment'])
  })

  it('skips thinking and the JSON completion blob on the ticker but still counts their tokens', () => {
    let feed = appendToFeed(
      undefined,
      msg({ Content: 'inner voice', Subtype: 'thinking', OutputTokens: 40 }),
    )
    feed = appendToFeed(feed, msg({ Content: '{"status":"done"}', OutputTokens: 8 }))
    expect(feed.lines).toEqual([])
    expect(feed.tokens).toBe(48)
  })

  it('caps the ticker buffer without changing what the card shows (last 5 of at least 8 kept)', () => {
    let feed = EMPTY_FEED
    for (let i = 0; i < 20; i++) {
      feed = appendToFeed(feed, msg({ Content: `line ${i}` }))
    }
    expect(feed.lines.length).toBe(8)
    expect(feed.lines[feed.lines.length - 1].text).toBe('line 19')
  })
})

describe('feedFromMessages', () => {
  it('matches the fold of appendToFeed over the same messages', () => {
    const messages = [
      msg({ Content: 'a', InputTokens: 1, OutputTokens: 2 }),
      msg({ Role: 'tool', ToolCallID: 'tc-1', Content: 'result' }),
      msg({ Role: 'user', Content: 'steer it' }),
    ]
    const folded = messages.reduce(
      (f, m) => appendToFeed(f, m),
      undefined as ReturnType<typeof feedFromMessages> | undefined,
    )
    expect(feedFromMessages(messages)).toEqual(folded)
    expect(feedFromMessages(messages).tokens).toBe(3)
    expect(feedFromMessages(messages).lines.map((l) => l.text)).toEqual(['a', 'you: steer it'])
  })
})
