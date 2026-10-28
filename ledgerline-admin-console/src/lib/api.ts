// Thin fetch wrappers for the three Ledgerline backend services. No
// authentication - this is a local-only demo tool (see README).

export class ApiError extends Error {
  readonly url: string
  readonly status?: number

  constructor(message: string, url: string, status?: number) {
    super(message)
    this.name = 'ApiError'
    this.url = url
    this.status = status
  }
}

/** Best-effort extraction of a human-readable message from an error body. */
function extractErrorMessage(body: unknown, fallback: string): string {
  if (body == null) return fallback
  if (typeof body === 'string') return body || fallback
  if (typeof body === 'object') {
    const obj = body as Record<string, unknown>
    if (typeof obj.error === 'string') return obj.error
    if (obj.error && typeof obj.error === 'object') {
      const nested = obj.error as Record<string, unknown>
      if (typeof nested.message === 'string') return nested.message
    }
    if (typeof obj.message === 'string') return obj.message
  }
  return fallback
}

/**
 * Fetches `url` and returns the parsed JSON body. Throws an ApiError with a
 * best-effort human-readable message on network failure or non-2xx status -
 * callers should catch this and show it inline rather than letting it crash
 * the view.
 */
export async function apiFetch<T>(url: string, init?: RequestInit): Promise<T> {
  let response: Response
  try {
    response = await fetch(url, {
      headers: { 'Content-Type': 'application/json', ...init?.headers },
      ...init,
    })
  } catch (err) {
    const reason = err instanceof Error ? err.message : String(err)
    throw new ApiError(`Could not reach ${url}: ${reason}`, url)
  }

  const text = await response.text()
  let body: unknown = undefined
  if (text) {
    try {
      body = JSON.parse(text)
    } catch {
      body = text
    }
  }

  if (!response.ok) {
    const message = extractErrorMessage(body, `${response.status} ${response.statusText}`)
    throw new ApiError(message, url, response.status)
  }

  return body as T
}

// --- balance-reader ---

export interface Balance {
  account_id: string
  currency: string
  balance_minor: number
}

export function getBalances(baseUrl: string): Promise<Balance[]> {
  return apiFetch<Balance[]>(`${baseUrl}/v1/balances`)
}

// --- gateway ---

export interface TransferRequest {
  dedup_id: string
  from_account: string
  to_account: string
  amount_minor: number
  fee_minor?: number
  currency: string
  category?: string
  country?: string
}

export function submitTransfer(baseUrl: string, req: TransferRequest): Promise<unknown> {
  return apiFetch<unknown>(`${baseUrl}/v1/transfers`, {
    method: 'POST',
    body: JSON.stringify(req),
  })
}

export interface Account {
  account_id: string
  currency: string
  balance_minor: number
  unlimited: boolean
}

export function getAccount(baseUrl: string, id: string): Promise<Account> {
  return apiFetch<Account>(`${baseUrl}/v1/accounts/${encodeURIComponent(id)}`)
}

// --- fx ---

export interface FxRate {
  base: string
  quote: string
  rate: number
  as_of: string
}

export function getRate(baseUrl: string, base: string, quote: string): Promise<FxRate> {
  return apiFetch<FxRate>(`${baseUrl}/v1/rates/${base}/${quote}`)
}

export interface FxConvertResult {
  base: string
  quote: string
  amount_minor: number
  converted_amount_minor: number
  rate: number
}

export function convert(
  baseUrl: string,
  base: string,
  quote: string,
  amountMinor: number,
): Promise<FxConvertResult> {
  return apiFetch<FxConvertResult>(`${baseUrl}/v1/convert`, {
    method: 'POST',
    body: JSON.stringify({ base, quote, amount_minor: amountMinor }),
  })
}
