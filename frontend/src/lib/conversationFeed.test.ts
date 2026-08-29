import { describe, it, expect } from 'vitest'
import type { Message } from '../types'
import { appendToFeed, feedFromMessages, EMPTY_FEED } from './conversationFeed'

let nextID = 1
function msg(over: Partial<Message>): Message {
  return {
    id: nextID++,
    conversation_id: 'r1',
    role: 'assistant',
    content: '',
    subtype: '',
    tool_call_id: '',
    is_error: false,
    model: '',
    created_at: '2026-07-07T12:00:00Z',
    ...over,
  }
}

describe('appendToFeed', () => {
  it('returns the SAME reference for a display-no-op message (the WS handler skips the state write on identity)', () => {
    const feed = appendToFeed(undefined, msg({ content: 'hello', output_tokens: 5 }))
    // A tool-result row: no ticker line, no tokens.
    const after = appendToFeed(feed, msg({ role: 'tool', tool_call_id: 'tc-1', content: 'ok' }))
    expect(after).toBe(feed)
    // Same contract from an empty start: base EMPTY_FEED comes back untouched.
    expect(appendToFeed(undefined, msg({ role: 'tool', tool_call_id: 'tc-2' }))).toBe(EMPTY_FEED)
  })

  it('accumulates tokens and ticker lines', () => {
    let feed = appendToFeed(
      undefined,
      msg({ content: 'planning', input_tokens: 100, output_tokens: 10 }),
    )
    feed = appendToFeed(
      feed,
      msg({
        subtype: 'tool_use',
        tool_calls: [
          {
            id: 'tc-1',
            name: 'Bash',
            input: { command: 'triagefactory exec gh pr add-comment', description: '' },
          },
        ],
        input_tokens: 50,
      }),
    )
    expect(feed.tokens).toBe(160)
    expect(feed.lines.map((l) => l.text)).toEqual(['planning', 'Adding comment'])
  })

  it('reads the authored summary off a native bash row, whose tool name is lowercase', () => {
    // A native conversation calls the tool `bash`, and native is the only
    // runtime that authors both tenses — so a ticker matching the SDK's `Bash`
    // alone would miss the summary on exactly the rows that carry one.
    const feed = appendToFeed(
      undefined,
      msg({
        subtype: 'tool_use',
        tool_calls: [
          {
            id: 'tc-1',
            name: 'bash',
            input: {
              command: 'go test ./internal/sandbox -run TestSampler_Series -count=20',
              description: 'Reproducing the flake',
              description_past: 'Ran the sampler test 50x',
            },
          },
        ],
      }),
    )
    // The ticker is a live strip, so it stays in the present tense even though
    // this row carries a past one.
    expect(feed.lines.map((l) => l.text)).toEqual(['Reproducing the flake'])
  })

  it('falls back to the command itself when a bash call carries no summary', () => {
    const feed = appendToFeed(
      undefined,
      msg({
        subtype: 'tool_use',
        tool_calls: [{ id: 'tc-1', name: 'bash', input: { command: 'go build ./...' } }],
      }),
    )
    expect(feed.lines.map((l) => l.text)).toEqual(['go build ./...'])
  })

  // Every curated reading is a sentence or it is not a reading. A branch that
  // matches on a token's presence and then renders its value can produce a
  // label that stops mid-phrase — "Running: undefined", "Reading diff: " —
  // which is worse on a card than the command it was standing in for.
  it('never renders a curated reading that stops mid-phrase', () => {
    const line = (command: string) =>
      appendToFeed(
        undefined,
        msg({
          subtype: 'tool_use',
          tool_calls: [{ id: 'tc-1', name: 'bash', input: { command } }],
        }),
      ).lines[0].text

    // The applet name with no argv after it, on its own line, or quoted inside
    // another command: each contains "triagefactory exec" and none of them
    // splits to an argv, so each reads as the command itself.
    expect(line('triagefactory exec')).toBe('triagefactory exec')
    expect(line('triagefactory exec\n  gh pr list')).toBe('triagefactory exec …')
    expect(line('echo "triagefactory exec"')).toBe('echo "triagefactory exec"')

    // --file present but valueless (dangling, or the =value form extractFlag
    // does not read) falls back to the whole-diff reading, never a bare colon.
    expect(line('triagefactory exec gh pr diff --file')).toBe('Reading full diff')
    expect(line('triagefactory exec gh pr diff --file=a.go')).toBe('Reading full diff')

    // The readings that do have their value are untouched.
    expect(line('triagefactory exec gh pr diff --file a.go')).toBe('Reading diff: a.go')
    expect(line('triagefactory exec jira issue view SKY-1')).toBe('Running: jira issue view SKY-1')
  })

  it('skips thinking and the JSON completion blob on the ticker but still counts their tokens', () => {
    let feed = appendToFeed(
      undefined,
      msg({ content: 'inner voice', subtype: 'thinking', output_tokens: 40 }),
    )
    feed = appendToFeed(feed, msg({ content: '{"status":"done"}', output_tokens: 8 }))
    expect(feed.lines).toEqual([])
    expect(feed.tokens).toBe(48)
  })

  it('caps the ticker buffer without changing what the card shows (last 5 of at least 8 kept)', () => {
    let feed = EMPTY_FEED
    for (let i = 0; i < 20; i++) {
      feed = appendToFeed(feed, msg({ content: `line ${i}` }))
    }
    expect(feed.lines.length).toBe(8)
    expect(feed.lines[feed.lines.length - 1].text).toBe('line 19')
  })
})

describe('feedFromMessages', () => {
  it('matches the fold of appendToFeed over the same messages', () => {
    const messages = [
      msg({ content: 'a', input_tokens: 1, output_tokens: 2 }),
      msg({ role: 'tool', tool_call_id: 'tc-1', content: 'result' }),
      msg({ role: 'user', content: 'steer it' }),
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

// The ticker's "you:" prefix is an attribution, and a role=user row is not
// always a person: the backend writes stop notes and executor notices on the
// same role. Prefixing those tells the reader they said something they never
// said.
describe('system-authored rows on the ticker', () => {
  it('shows a stop note without the operator attribution', () => {
    const feed = feedFromMessages([
      msg({ role: 'user', subtype: '', content: 'fix the build' }),
      msg({
        role: 'user',
        subtype: 'stop-note',
        content: 'Run stopped by the user. It may be resumed later.',
      }),
    ])
    expect(feed.lines.map((l) => l.text)).toEqual([
      'you: fix the build',
      'Run stopped by the user. It may be resumed later.',
    ])
  })

  it('still attributes a mid-work steer to the operator', () => {
    const feed = feedFromMessages([
      msg({ role: 'user', subtype: 'injection:steer', content: 'also check the tests' }),
    ])
    expect(feed.lines.map((l) => l.text)).toEqual(['you: also check the tests'])
  })
})
