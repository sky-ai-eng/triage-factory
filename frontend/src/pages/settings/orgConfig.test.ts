import { describe, it, expect } from 'vitest'
import { dailyCapError, concurrentRunsError, MAX_CONCURRENT_RUNS_CEILING } from './orgConfig'

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

  it('accepts 0 — the explicit "unlimited" value (blank or 0)', () => {
    expect(concurrentRunsError('0')).toBeNull()
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
    expect(concurrentRunsError('0.5')).not.toBeNull()
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
