import { useCallback, useEffect, useState } from 'react'
import { BALANCE_READER_URL } from '../config'
import { ApiError, getBalances, type Balance } from '../lib/api'
import { formatMinorAmount } from '../lib/format'

const REFRESH_INTERVAL_MS = 3000

export function BalancesView() {
  const [balances, setBalances] = useState<Balance[]>([])
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  const load = useCallback(async () => {
    try {
      const data = await getBalances(BALANCE_READER_URL)
      setBalances(data)
      setError(null)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load()
    // Auto-refresh on an interval rather than a manual button, so the view
    // stays live while you submit transfers from the other tab.
    const id = setInterval(load, REFRESH_INTERVAL_MS)
    return () => clearInterval(id)
  }, [load])

  return (
    <div className="panel">
      <div className="panel-header">
        <h2>Balances</h2>
        <button type="button" onClick={load}>
          Refresh now
        </button>
      </div>
      <p className="hint">
        Source: {BALANCE_READER_URL}/v1/balances - auto-refreshes every{' '}
        {REFRESH_INTERVAL_MS / 1000}s. Amounts assume 2 decimal places for every currency (see
        README).
      </p>
      {error && <div className="error-banner">Could not reach balance-reader: {error}</div>}
      {!error && loading && balances.length === 0 && <p>Loading...</p>}
      {!error && balances.length === 0 && !loading && <p>No accounts yet.</p>}
      {balances.length > 0 && (
        <table>
          <thead>
            <tr>
              <th>Account ID</th>
              <th>Currency</th>
              <th>Balance (minor)</th>
              <th>Balance</th>
            </tr>
          </thead>
          <tbody>
            {balances.map((b) => (
              <tr key={b.account_id}>
                <td>{b.account_id}</td>
                <td>{b.currency}</td>
                <td>{b.balance_minor}</td>
                <td>{formatMinorAmount(b.balance_minor, b.currency)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}
