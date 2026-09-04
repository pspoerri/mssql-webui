import { useEffect, useState } from 'react'
import { api, fromPath, toPath, type Selection } from './api'
import { Console } from './Console'
import { TableView } from './TableView'
import { Tree } from './Tree'

type Me = { name: string; email: string; version: string }

export default function App() {
  const [me, setMe] = useState<Me | null>(null)
  const [path, setPath] = useState(location.pathname)
  const [dirty, setDirty] = useState(false)
  const sel = fromPath(path)

  useEffect(() => {
    api<Me>('/api/me').then(setMe).catch(() => {})
  }, [])

  // Unsaved table edits: warn before leaving the page, and confirm before
  // navigating within the app or logging out.
  useEffect(() => {
    if (!dirty) return
    const warn = (e: BeforeUnloadEvent) => { e.preventDefault(); e.returnValue = '' }
    window.addEventListener('beforeunload', warn)
    return () => window.removeEventListener('beforeunload', warn)
  }, [dirty])
  const guard = () => !dirty || confirm('You have unsaved changes. Discard them?')

  const nav = (p: string) => {
    if (p === path || !guard()) return
    history.pushState(null, '', p)
    setPath(p)
  }
  useEffect(() => {
    // ponytail: a declined back/forward re-pushes the current path instead of tracking history depth
    const onPop = () => (guard() ? setPath(location.pathname) : history.pushState(null, '', path))
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [dirty, path]) // eslint-disable-line react-hooks/exhaustive-deps

  const logout = async () => {
    if (!guard()) return
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
      <Tree selected={sel} onSelect={(s: Selection) => nav(toPath(s))} />
      <main>
        {path === '/help' ? (
          <Help />
        ) : sel?.console ? (
          <Console key={`${sel.srv}/${sel.db}`} srv={sel.srv} db={sel.db} />
        ) : sel?.table ? (
          <TableView key={path} srv={sel.srv} db={sel.db} table={sel.table} onDirty={setDirty} />
        ) : (
          <p>Select a table.</p>
        )}
      </main>
      <footer>
        <span className={dirty ? 'unsaved' : ''}>{dirty ? 'Unsaved changes' : 'All changes saved'}</span>
        <a href="/help" onClick={(e) => { e.preventDefault(); nav('/help') }}>Help</a>
        <span className="version">{me?.version}</span>
      </footer>
    </div>
  )
}

function Help() {
  return (
    <div className="help">
      <h2>Help</h2>
      <ul>
        <li>Expand a database in the tree and pick a table; more rows load as you scroll.</li>
        <li>Search filters rows: bare words must all occur in one column (<code>User 1001</code> finds that name);
          <code>col=value</code> matches exactly, <code>col^value</code> a prefix, <code>col~value</code> a substring;
          quotes keep spaces together (<code>name='User 1002'</code>); all terms must match.</li>
        <li>Hover a column header for its sort and filter icons. The filter popover writes a term for that column into the
          search box, so several columns can be filtered at once; clicking a lit filter icon removes that filter.</li>
        <li>Tables with a primary key are editable: change cells, tick rows to delete, or add rows.
          Nothing is written until you press <b>Save</b> (or <b>Enter</b> in a cell; Ctrl+Enter in a multi-line editor),
          which applies all pending changes in one transaction.
          Leaving the table with unsaved changes asks for confirmation.</li>
        <li>Tables without a primary key are append-only; views are read-only.</li>
        <li><b>Download CSV</b> exports the whole table.</li>
        <li>The SQL console runs ad-hoc statements against the selected database.</li>
        <li>Drag a column header's right edge to resize it; double-click the edge to fit the content, double-click again to fit the label.
          Key columns stay in place when scrolling sideways; edited cells are highlighted.</li>
        <li>The focused cell is also shown in an editor above the table, handy for long values. <b>Esc</b> (or Revert there)
          undoes that cell's change and leaves it; Discard undoes everything.</li>
        <li>The selected table, search, sort and scroll position are part of the URL, so a link can be bookmarked or shared.</li>
      </ul>
      <p>Source code, issues and documentation: <a href="https://github.com/pspoerri/mssql-webui" target="_blank" rel="noreferrer">github.com/pspoerri/mssql-webui</a></p>
    </div>
  )
}
