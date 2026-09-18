// Amounts cross the API as a BigInt scalar: a string of minor units (US
// cents). They stay strings or bigints here, never floats, so no amount is
// rounded on its way through the browser.

const DOLLARS = /^(\d+)(?:\.(\d{1,2}))?$/

// A dollar amount as typed ("1,250.5", "$40") to cents, or null when it is not
// a positive amount with at most two decimal places.
export function dollarsToCents(input: string): string | null {
  const match = DOLLARS.exec(input.trim().replace(/^\$/, '').replaceAll(',', ''))
  if (!match) return null

  const [, whole = '0', fraction = ''] = match
  const cents = BigInt(whole) * 100n + BigInt(fraction.padEnd(2, '0'))
  return cents > 0n ? cents.toString() : null
}

// Negative for an Account that has overdrawn.
export function formatMinorUnits(minorUnits: string, currency: string): string {
  const cents = BigInt(minorUnits)
  const magnitude = cents < 0n ? -cents : cents
  const fraction = (magnitude % 100n).toString().padStart(2, '0')

  // Intl formats the bigint whole part exactly; only the cents are spliced in.
  const formatter = new Intl.NumberFormat('en-US', { style: 'currency', currency, maximumFractionDigits: 0 })
  return `${cents < 0n ? '-' : ''}${formatter.format(magnitude / 100n)}.${fraction}`
}
