import { useState } from 'react'
import { FX_URL } from '../config'
import { ApiError, convert, getRate, type FxConvertResult, type FxRate } from '../lib/api'

const CURRENCIES = ['USD', 'EUR', 'GBP', 'JPY']

export function FxRatesView() {
  const [base, setBase] = useState('USD')
  const [quote, setQuote] = useState('EUR')
  const [amountMinor, setAmountMinor] = useState('10000')
  const [rate, setRate] = useState<FxRate | null>(null)
  const [conversion, setConversion] = useState<FxConvertResult | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  async function handleGetRate() {
    setBusy(true)
    setError(null)
    setRate(null)
    try {
      setRate(await getRate(FX_URL, base, quote))
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  async function handleConvert() {
    setBusy(true)
    setError(null)
    setConversion(null)
    try {
      setConversion(await convert(FX_URL, base, quote, Number(amountMinor)))
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="panel">
      <h2>FX Rates</h2>
      <p className="hint">Queries {FX_URL}</p>
      <div className="form-grid">
        <label>
          Base currency
          <select value={base} onChange={(e) => setBase(e.target.value)}>
            {CURRENCIES.map((c) => (
              <option key={c} value={c}>
                {c}
              </option>
            ))}
          </select>
        </label>
        <label>
          Quote currency
          <select value={quote} onChange={(e) => setQuote(e.target.value)}>
            {CURRENCIES.map((c) => (
              <option key={c} value={c}>
                {c}
              </option>
            ))}
          </select>
        </label>
        <label>
          Amount (minor units, for conversion)
          <input
            type="number"
            min="0"
            value={amountMinor}
            onChange={(e) => setAmountMinor(e.target.value)}
          />
        </label>
      </div>
      <div className="button-row">
        <button type="button" onClick={handleGetRate} disabled={busy}>
          Get rate
        </button>
        <button type="button" onClick={handleConvert} disabled={busy}>
          Convert
        </button>
      </div>
      {error && <div className="error-banner">Could not reach fx: {error}</div>}
      {rate && (
        <div className="result-banner">
          <p>
            1 {rate.base} = {rate.rate} {rate.quote}
          </p>
          <p className="hint">as of {rate.as_of}</p>
        </div>
      )}
      {conversion && (
        <div className="result-banner">
          <p>
            {conversion.amount_minor} {conversion.base} (minor) = {conversion.converted_amount_minor}{' '}
            {conversion.quote} (minor) at rate {conversion.rate}
          </p>
        </div>
      )}
    </div>
  )
}
