// The org's API-token age cap, as the admin meets it. Two things are pinned:
// the copy states the consequence that surprises people — the limit applies to
// tokens that already exist, so lowering it shortens them — and the input's
// own validation refuses a value the handler would 422, so Save blocks before
// the round-trip rather than bouncing off the API.
import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'

import ApiTokenPolicyGroup from './ApiTokenPolicyGroup'
import {
  apiTokenMaxAgeError,
  API_TOKEN_MAX_AGE_DAYS_MAX,
  API_TOKEN_MAX_AGE_DAYS_MIN,
} from './orgConfig'

describe('ApiTokenPolicyGroup', () => {
  it('renders the stored cap and says the limit reaches existing tokens', () => {
    render(<ApiTokenPolicyGroup value="30" onChange={vi.fn()} error={null} />)

    const input = screen.getByLabelText(/maximum age/i) as HTMLInputElement
    expect(input.value).toBe('30')
    expect(input.placeholder).toBe('No maximum')
    // The consequence an admin has to know before typing: this is not a rule
    // about tokens minted from now on.
    expect(screen.getByText(/tokens that already exist/i)).toBeTruthy()
  })

  it('reports what the user typed rather than swallowing it', () => {
    const onChange = vi.fn()
    render(<ApiTokenPolicyGroup value="" onChange={onChange} error={null} />)

    fireEvent.change(screen.getByLabelText(/maximum age/i), { target: { value: '45' } })
    expect(onChange).toHaveBeenCalledWith('45')
  })

  it('shows the validation message and marks the input invalid', () => {
    const message = apiTokenMaxAgeError(String(API_TOKEN_MAX_AGE_DAYS_MAX + 1))
    expect(message).not.toBeNull()

    render(<ApiTokenPolicyGroup value="366" onChange={vi.fn()} error={message} />)
    const input = screen.getByLabelText(/maximum age/i)
    expect(input.getAttribute('aria-invalid')).toBe('true')
    expect(screen.getByText(message as string)).toBeTruthy()
  })
})

// The rules the input mirrors. The handler is the enforcement — this is the
// fast feedback, and it has to agree with the band the API accepts or Save
// blocks on a value the API would have taken (or lets through one it won't).
describe('apiTokenMaxAgeError', () => {
  it('treats blank (and whitespace) as valid — that is how "no maximum" is expressed', () => {
    expect(apiTokenMaxAgeError('')).toBeNull()
    expect(apiTokenMaxAgeError('   ')).toBeNull()
  })

  it('accepts both ends of the band', () => {
    expect(apiTokenMaxAgeError(String(API_TOKEN_MAX_AGE_DAYS_MIN))).toBeNull()
    expect(apiTokenMaxAgeError(String(API_TOKEN_MAX_AGE_DAYS_MAX))).toBeNull()
    expect(apiTokenMaxAgeError('30')).toBeNull()
  })

  // 0 is refused rather than read as "no maximum": that has one spelling, the
  // blank field, and a zero-day cap would expire every token at mint.
  it('rejects 0, negatives and anything past the upper bound', () => {
    expect(apiTokenMaxAgeError('0')).not.toBeNull()
    expect(apiTokenMaxAgeError('-1')).not.toBeNull()
    expect(apiTokenMaxAgeError(String(API_TOKEN_MAX_AGE_DAYS_MAX + 1))).not.toBeNull()
  })

  it('rejects fractional and non-numeric input (the column is an integer)', () => {
    expect(apiTokenMaxAgeError('1.5')).not.toBeNull()
    expect(apiTokenMaxAgeError('thirty')).not.toBeNull()
  })
})
