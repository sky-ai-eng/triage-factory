import { describe, expect, it } from 'vitest'
import {
  expiry,
  expiresAtFor,
  presetOff,
  PRESETS,
  shortDate,
  validCidr,
  daysUntil,
  DAY,
} from './tokenMath'

const NOW = Date.UTC(2026, 8, 5, 12, 0, 0) // 5 Sep 2026, noon UTC

describe('tokenMath — the day-math the ticket specifies', () => {
  it('rounds days up, and reads expired at or past the instant', () => {
    expect(daysUntil(new Date(NOW + 25 * 3_600_000).toISOString(), NOW)).toBe(2)
    expect(daysUntil(new Date(NOW + 30 * 60_000).toISOString(), NOW)).toBe(1)
    expect(daysUntil(new Date(NOW - 1).toISOString(), NOW)).toBe(0)
  })

  it('reads the effective date and says when the cap is what decided', () => {
    const stored = new Date(NOW + 365 * DAY).toISOString()
    const capped = new Date(NOW + 50 * DAY).toISOString()
    expect(expiry({ expires_at: stored, effective_expires_at: capped }, NOW)).toEqual({
      text: 'in 50d',
      days: 50,
      orgLimit: true,
    })
    // No expiry of the minter's own, but a cap: the cap decides.
    expect(expiry({ expires_at: null, effective_expires_at: capped }, NOW).orgLimit).toBe(true)
    // The minter's own date, within the cap.
    expect(expiry({ expires_at: capped, effective_expires_at: capped }, NOW).orgLimit).toBe(false)
    expect(expiry({ expires_at: null, effective_expires_at: null }, NOW)).toEqual({
      text: 'never',
      days: Infinity,
      orgLimit: false,
    })
    expect(
      expiry({ expires_at: null, effective_expires_at: new Date(NOW - DAY).toISOString() }, NOW)
        .text,
    ).toBe('expired')
  })

  it('carries the year only when it is not this year', () => {
    expect(shortDate(NOW + 2 * DAY, NOW)).toBe('7 Sept')
    expect(shortDate(NOW + 400 * DAY, NOW)).toBe('10 Oct 2027')
  })

  it('validates v4 and v6 ranges, as fast feedback', () => {
    expect(validCidr('10.4.0.0/16')).toBe(true)
    expect(validCidr('52.14.9.20/32')).toBe(true)
    expect(validCidr('2600:1f18::/32')).toBe(true)
    expect(validCidr('10.4.0.0')).toBe(false)
    expect(validCidr('10.4.0.256/16')).toBe(false)
    expect(validCidr('10.4.0.0/33')).toBe(false)
    expect(validCidr('2600:1f18::/129')).toBe(false)
  })

  it('strikes the presets past the cap and keeps the rest', () => {
    const off = PRESETS.filter((p) => presetOff(p, 90)).map((p) => p.id)
    expect(off).toEqual(['never'])
    expect(PRESETS.filter((p) => presetOff(p, 30)).map((p) => p.id)).toEqual(['60', '90', 'never'])
    expect(PRESETS.some((p) => presetOff(p, null))).toBe(false)
  })

  it('resolves a pick to an absolute instant, never a duration', () => {
    expect(expiresAtFor('30', '', NOW)).toBe(new Date(NOW + 30 * DAY).toISOString())
    expect(expiresAtFor('never', '', NOW)).toBeNull()
    expect(expiresAtFor('custom', '', NOW)).toBeUndefined()
    expect(expiresAtFor('custom', '2027-06-09', NOW)).toBe(
      new Date('2027-06-09T00:00:00').toISOString(),
    )
  })
})
