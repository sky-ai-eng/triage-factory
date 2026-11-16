import { describe, it, expect, vi } from 'vitest'
import {
  dailyCapError,
  concurrentRunsError,
  MAX_CONCURRENT_RUNS_CEILING,
  patchOrgSettings,
} from './orgConfig'
import { jsonBody } from '../../test/apiResponse'

describe('dailyCapError', () => {
  it('treats blank (and whitespace) as valid — that is how "no cap" is expressed', () => {
    expect(dailyCapError('')).toBeNull()
    expect(dailyCapError('   ')).toBeNull()
  })

  it('accepts a positive number, fractions included', () => {
    expect(dailyCapError('25')).toBeNull()
    expect(dailyCapError('0.01')).toBeNull()
    expect(dailyCapError('1000.5')).toBeNull()
  })

  it('rejects zero (a $0/day cap is meaningless — clear the field instead)', () => {
    expect(dailyCapError('0')).not.toBeNull()
    expect(dailyCapError('0.00')).not.toBeNull()
  })

  it('rejects negative values', () => {
    expect(dailyCapError('-1')).not.toBeNull()
    expect(dailyCapError('-0.5')).not.toBeNull()
  })

  it('rejects non-numeric input', () => {
    expect(dailyCapError('abc')).not.toBeNull()
    expect(dailyCapError('$25')).not.toBeNull()
  })
})

describe('concurrentRunsError', () => {
  it('treats blank (and whitespace) as valid — one way to express "unlimited"', () => {
    expect(concurrentRunsError('')).toBeNull()
    expect(concurrentRunsError('   ')).toBeNull()
  })

  // "Unlimited" has exactly one spelling now, and it is the blank field. An
  // explicit 0 is refused rather than quietly read as unlimited: capping at 0
  // and having no cap are different intents, and the API 422s the former.
  it('rejects an explicit 0 — blank is the one way to say "unlimited"', () => {
    expect(concurrentRunsError('0')).not.toBeNull()
  })

  it('accepts a positive whole number', () => {
    expect(concurrentRunsError('1')).toBeNull()
    expect(concurrentRunsError('8')).toBeNull()
    expect(concurrentRunsError('256')).toBeNull()
  })

  it('rejects negative values', () => {
    expect(concurrentRunsError('-1')).not.toBeNull()
  })

  it('rejects fractional values (the column is an integer)', () => {
    expect(concurrentRunsError('2.5')).not.toBeNull()
  })

  it('accepts the ceiling but rejects anything above it', () => {
    expect(concurrentRunsError(String(MAX_CONCURRENT_RUNS_CEILING))).toBeNull()
    expect(concurrentRunsError(String(MAX_CONCURRENT_RUNS_CEILING + 1))).not.toBeNull()
  })

  it('rejects non-numeric input', () => {
    expect(concurrentRunsError('abc')).not.toBeNull()
    expect(concurrentRunsError('8 runs')).not.toBeNull()
  })
})

// `conflict` tells a caller to re-read the row and drop what the person typed.
// Only a stale version asks for that. A 409 about a value sent, such as a model
// whose provider is not connected, keeps the draft and shows its message.
describe('patchOrgSettings', () => {
  const refuse = (reason: string, message: string) =>
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: false,
        status: 409,
        ...jsonBody({ errors: [{ reason, message }] }),
      }),
    )

  it('reports a 409 VERSION_CONFLICT as a conflict', async () => {
    refuse('VERSION_CONFLICT', 'The settings changed since you loaded them.')
    expect(
      await patchOrgSettings('org-1', 3, { background_jobs_model: 'claude-haiku-5-5' }),
    ).toEqual({ ok: false, error: 'The settings changed since you loaded them.', conflict: true })
  })

  it('does not report a 409 NOT_CONFIGURED as a conflict', async () => {
    refuse('NOT_CONFIGURED', 'The provider is not connected.')
    expect(
      await patchOrgSettings('org-1', 3, {
        background_jobs_model: 'us.anthropic.claude-sonnet-5-5',
      }),
    ).toEqual({ ok: false, error: 'The provider is not connected.', conflict: false })
  })
})
