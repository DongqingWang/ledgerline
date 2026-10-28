import { useState } from 'react'
import './App.css'
import { BalancesView } from './components/BalancesView'
import { FxRatesView } from './components/FxRatesView'
import { TransferForm } from './components/TransferForm'

type Tab = 'balances' | 'transfer' | 'fx'

const TABS: { id: Tab; label: string }[] = [
  { id: 'balances', label: 'Balances' },
  { id: 'transfer', label: 'Submit Transfer' },
  { id: 'fx', label: 'FX Rates' },
]

function App() {
  const [tab, setTab] = useState<Tab>('balances')

  return (
    <div className="app">
      <header className="app-header">
        <h1>Ledgerline Admin Console</h1>
        <p className="hint">Local-only demo UI. No authentication - do not expose publicly.</p>
      </header>
      <nav className="tab-bar">
        {TABS.map((t) => (
          <button
            key={t.id}
            type="button"
            className={t.id === tab ? 'tab active' : 'tab'}
            onClick={() => setTab(t.id)}
          >
            {t.label}
          </button>
        ))}
      </nav>
      <main>
        {tab === 'balances' && <BalancesView />}
        {tab === 'transfer' && <TransferForm />}
        {tab === 'fx' && <FxRatesView />}
      </main>
    </div>
  )
}

export default App
