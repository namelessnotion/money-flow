import { describe, expect, it } from 'vitest'
import { dollarsToCents, formatMinorUnits } from './money'

describe('dollarsToCents', () => {
  it.each([
    ['125', '12500'],
    ['125.5', '12550'],
    ['125.05', '12505'],
    ['0.01', '1'],
    [' 1,250.00 ', '125000'],
    ['$40', '4000'],
    ['90071992547409.93', '9007199254740993'],
  ])('reads %j as %s cents', (input, cents) => {
    expect(dollarsToCents(input)).toBe(cents)
  })

  it.each(['', '0', '0.00', '-5', '1.234', 'abc', '1.2.3', '.'])('refuses %j', (input) => {
    expect(dollarsToCents(input)).toBeNull()
  })
})

describe('formatMinorUnits', () => {
  it('formats cents as currency', () => {
    expect(formatMinorUnits('12505', 'USD')).toBe('$125.05')
  })

  it('keeps precision beyond Number.MAX_SAFE_INTEGER', () => {
    expect(formatMinorUnits('9007199254740993', 'USD')).toBe('$90,071,992,547,409.93')
  })

  it('formats a negative amount, as an overdrawn Account has', () => {
    expect(formatMinorUnits('-12505', 'USD')).toBe('-$125.05')
    expect(formatMinorUnits('-50', 'USD')).toBe('-$0.50')
  })

  it('formats zero', () => {
    expect(formatMinorUnits('0', 'USD')).toBe('$0.00')
  })
})
