import { describe, it, expect } from 'vitest'
import { dailyCapError } from './orgConfig'

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
