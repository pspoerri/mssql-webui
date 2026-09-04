import { useEffect, useState } from 'react'
import { api, type Selection } from './api'
import { Console } from './Console'
import { TableView } from './TableView'
import { Tree } from './Tree'

export default function App() {
  const [me, setMe] = useState<{ name: string; email: string } | null>(null)
  const [sel, setSel] = useState<Selection | null>(null)

  useEffect(() => {
    api<{ name: string; email: string }>('/api/me').then(setMe).catch(() => {})
  }, [])

  const logout = async () => {
    await fetch('/auth/logout', { method: 'POST' })
    window.location.href = '/auth/login'
  }

  return (
    <div className="app">
      <header>
        <b>mssql-webui</b>
        <span className="me">{me?.name}</span>
        <button onClick={logout}>Logout</button>
      </header>
      <Tree selected={sel} onSelect={setSel} />
      <main>
        {sel?.console ? (
          <Console key={`${sel.srv}/${sel.db}`} srv={sel.srv} db={sel.db} />
        ) : sel?.table ? (
          <TableView key={`${sel.srv}/${sel.db}/${sel.table.schema}/${sel.table.name}`} srv={sel.srv} db={sel.db} table={sel.table} />
        ) : (
          <p>Select a table.</p>
        )}
      </main>
    </div>
  )
}
