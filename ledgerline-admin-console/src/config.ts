// Backend base URLs. All overridable via Vite env vars (see README) so that
// a docker-compose setup (or any other deployment) can point this UI at
// different hostnames without touching the code.
export const GATEWAY_URL: string =
  import.meta.env.VITE_GATEWAY_URL ?? 'http://127.0.0.1:8080'

export const BALANCE_READER_URL: string =
  import.meta.env.VITE_BALANCE_READER_URL ?? 'http://127.0.0.1:8280'

export const FX_URL: string = import.meta.env.VITE_FX_URL ?? 'http://127.0.0.1:8590'
