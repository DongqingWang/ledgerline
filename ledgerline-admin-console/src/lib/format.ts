/**
 * Formats an integer "minor units" amount (e.g. cents) as a decimal currency
 * string.
 *
 * Known simplification: this demo assumes every currency uses 2 decimal
 * places (minor unit = 1/100 of the major unit). That is true for USD, EUR,
 * GBP, etc. but is NOT true for all real-world currencies (e.g. JPY has 0
 * decimal places, some currencies have 3). A production tool would look up
 * the decimal precision per currency code.
 */
export function formatMinorAmount(amountMinor: number, currency: string): string {
  const major = amountMinor / 100
  try {
    return new Intl.NumberFormat('en-US', {
      style: 'currency',
      currency,
      currencyDisplay: 'narrowSymbol',
    }).format(major)
  } catch {
    // Unknown/invalid currency code for Intl - fall back to a plain decimal
    // with the currency code suffixed.
    return `${major.toFixed(2)} ${currency}`
  }
}
