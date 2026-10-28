import { useState } from 'react'
import { GATEWAY_URL } from '../config'
import { ApiError, submitTransfer, type TransferRequest } from '../lib/api'

function randomDedupId(): string {
  if (typeof crypto !== 'undefined' && 'randomUUID' in crypto) {
    return `txn-${crypto.randomUUID()}`
  }
  return `txn-${Math.random().toString(36).slice(2)}${Date.now().toString(36)}`
}

interface FormState {
  dedup_id: string
  from_account: string
  to_account: string
  amount_minor: string
  fee_minor: string
  currency: string
  category: string
  country: string
}

function initialState(): FormState {
  return {
    dedup_id: randomDedupId(),
    from_account: '',
    to_account: '',
    amount_minor: '',
    fee_minor: '0',
    currency: 'USD',
    category: '',
    country: '',
  }
}

export function TransferForm() {
  const [form, setForm] = useState<FormState>(initialState)
  const [result, setResult] = useState<unknown>(null)
  const [resultIsError, setResultIsError] = useState(false)
  const [submitting, setSubmitting] = useState(false)

  function update<K extends keyof FormState>(key: K, value: FormState[K]) {
    setForm((f) => ({ ...f, [key]: value }))
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setSubmitting(true)
    setResult(null)
    setResultIsError(false)

    const req: TransferRequest = {
      dedup_id: form.dedup_id,
      from_account: form.from_account,
      to_account: form.to_account,
      amount_minor: Number(form.amount_minor),
      currency: form.currency,
    }
    if (form.fee_minor.trim() !== '') req.fee_minor = Number(form.fee_minor)
    if (form.category.trim() !== '') req.category = form.category
    if (form.country.trim() !== '') req.country = form.country

    try {
      const res = await submitTransfer(GATEWAY_URL, req)
      setResult(res)
      setResultIsError(false)
      // Generate a fresh dedup_id for the next submission so a double-click
      // doesn't accidentally resubmit the same transfer.
      setForm((f) => ({ ...f, dedup_id: randomDedupId() }))
    } catch (err) {
      setResultIsError(true)
      if (err instanceof ApiError) {
        setResult({ error: err.message, status: err.status, url: err.url })
      } else {
        setResult({ error: String(err) })
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="panel">
      <h2>Submit Transfer</h2>
      <p className="hint">POSTs to {GATEWAY_URL}/v1/transfers</p>
      <form onSubmit={handleSubmit} className="form-grid">
        <label>
          Dedup ID
          <input
            value={form.dedup_id}
            onChange={(e) => update('dedup_id', e.target.value)}
            required
          />
        </label>
        <label>
          From account
          <input
            value={form.from_account}
            onChange={(e) => update('from_account', e.target.value)}
            required
          />
        </label>
        <label>
          To account
          <input
            value={form.to_account}
            onChange={(e) => update('to_account', e.target.value)}
            required
          />
        </label>
        <label>
          Amount (minor units)
          <input
            type="number"
            min="0"
            value={form.amount_minor}
            onChange={(e) => update('amount_minor', e.target.value)}
            required
          />
        </label>
        <label>
          Fee (minor units, optional)
          <input
            type="number"
            min="0"
            value={form.fee_minor}
            onChange={(e) => update('fee_minor', e.target.value)}
          />
        </label>
        <label>
          Currency
          <input value={form.currency} onChange={(e) => update('currency', e.target.value)} />
        </label>
        <label>
          Category (optional)
          <input value={form.category} onChange={(e) => update('category', e.target.value)} />
        </label>
        <label>
          Country (optional)
          <input value={form.country} onChange={(e) => update('country', e.target.value)} />
        </label>
        <button type="submit" disabled={submitting}>
          {submitting ? 'Submitting...' : 'Submit transfer'}
        </button>
      </form>
      {result !== null && (
        <div className={resultIsError ? 'error-banner' : 'result-banner'}>
          <h3>{resultIsError ? 'Error' : 'Response'}</h3>
          <pre>{JSON.stringify(result, null, 2)}</pre>
        </div>
      )}
    </div>
  )
}
