import { useEffect, useState } from 'react'
import { api, fromPath, toPath, type Selection } from './api'
import { Console } from './Console'
import { TableView } from './TableView'
import { Tree } from './Tree'
import { Icon } from './icons'

type Me = { name: string; email: string; version: string }

export default function App() {
  const [me, setMe] = useState<Me | null>(null)
  const [path, setPath] = useState(location.pathname)
  const [dirty, setDirty] = useState(false)
  const sel = fromPath(path)
  // Sidebar: drag the gutter to resize, header button to hide; both remembered.
  const [navW, setNavW] = useState(() => +(localStorage.getItem('navW') ?? 280))
  const [navOpen, setNavOpen] = useState(() => localStorage.getItem('navOpen') !== '0')
  const toggleNav = () => {
    localStorage.setItem('navOpen', navOpen ? '0' : '1')
    setNavOpen(!navOpen)
  }

  useEffect(() => {
    api<Me>('/api/me').then(setMe).catch(() => {})
  }, [])

  // Header breadcrumb and tab title follow the selection.
  const crumbs = path === '/help' ? ['Help']
    : sel ? [sel.srv, sel.db, sel.console ? 'SQL console' : sel.table ? `${sel.table.schema}.${sel.table.name}` : ''].filter(Boolean)
    : []
  useEffect(() => {
    document.title = crumbs.length > 1 ? `${crumbs[crumbs.length - 1]} – ${crumbs[1]}` : 'mssql-webui'
  }, [path]) // eslint-disable-line react-hooks/exhaustive-deps

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
    <div className={navOpen ? 'app' : 'app nonav'} style={{ '--nav-w': navOpen ? `${navW}px` : '0px' } as React.CSSProperties}>
      <header>
        <button className="quiet" onClick={toggleNav} aria-expanded={navOpen} aria-controls="side"
          title={navOpen ? 'Hide sidebar' : 'Show sidebar'}><Icon name="sidebar" size={16} /></button>
        <span className="brand"><Icon name="database" />mssql-webui</span>
        <div className="crumbs">
          {crumbs.map((c, i) => (
            <span key={i}>{i > 0 && <span className="sep">/ </span>}{i === crumbs.length - 1 ? <b>{c}</b> : c}</span>
          ))}
        </div>
        <span className="me">{me?.name}</span>
        <button className="quiet" onClick={logout}>Log out</button>
      </header>
      <div className="side" id="side">
        <Tree selected={sel} onSelect={(s: Selection) => nav(toPath(s))} />
        <div className="gutter" title="Drag to resize; double-click to reset"
          onPointerDown={(e) => { e.preventDefault(); e.currentTarget.setPointerCapture(e.pointerId) }}
          onPointerMove={(e) => e.currentTarget.hasPointerCapture(e.pointerId) && setNavW(Math.min(600, Math.max(160, e.clientX)))}
          onPointerUp={() => localStorage.setItem('navW', String(navW))}
          onDoubleClick={() => { setNavW(280); localStorage.removeItem('navW') }} />
      </div>
      <main>
        {path === '/help' ? (
          <Help />
        ) : sel?.console ? (
          <Console key={`${sel.srv}/${sel.db}`} srv={sel.srv} db={sel.db} />
        ) : sel?.table ? (
          <TableView key={path} srv={sel.srv} db={sel.db} table={sel.table} onDirty={setDirty} />
        ) : (
          <div className="empty">
            <Icon name="table" size={28} />
            <p>Pick a table from the list on the left to browse its rows, or open a SQL console.</p>
          </div>
        )}
      </main>
      <footer>
        <span className={dirty ? 'status unsaved' : 'status'}>{dirty ? 'Unsaved changes' : 'All changes saved'}</span>
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
      <h3>Browsing</h3>
      <ul>
        <li>Expand a database in the tree and pick a table; more rows load as you scroll.</li>
        <li>Drag the sidebar's right edge to resize it; the button at the top left hides and shows it.</li>
        <li>Drag a column header's right edge to resize it; double-click the edge to fit the content, double-click again to fit the label.
          Key columns stay in place when scrolling sideways; the pin icon in a column header pins or unpins any column.</li>
        <li>The selected table, search, sort and scroll position are part of the URL, so a link can be bookmarked or shared.</li>
        <li><b>Download CSV</b> exports the whole table.</li>
      </ul>
      <h3>Searching</h3>
      <ul>
        <li>Bare words must all occur in one column (<code>User 1001</code> finds that name);
          <code>col=value</code> matches exactly, <code>col^value</code> a prefix, <code>col~value</code> a substring;
          quotes keep spaces together (<code>name='User 1002'</code>); all terms must match.</li>
        <li>Hover a column header for its sort and filter icons. The filter popover writes a term for that column into the
          search box, so several columns can be filtered at once; clicking a lit filter icon removes that filter.</li>
      </ul>
      <h3>Editing</h3>
      <ul>
        <li>Tables with a primary key are editable: change cells, tick rows to delete, or add rows. Edited cells are highlighted.
          Nothing is written until you press <b>Save</b> (or <kbd>Enter</kbd> in a cell; <kbd>Ctrl</kbd>+<kbd>Enter</kbd> in a multi-line editor),
          which applies all pending changes in one transaction.
          Leaving the table with unsaved changes asks for confirmation.</li>
        <li>Tables without a primary key are append-only; views are read-only.</li>
        <li>The focused cell is also shown in an editor above the table, handy for long values. <kbd>Esc</kbd> (or Revert there)
          undoes that cell's change and leaves it; Discard undoes everything.</li>
        <li>The SQL console runs ad-hoc statements against the selected database.</li>
      </ul>
      <p>Source code, issues and documentation: <a href="https://github.com/pspoerri/mssql-webui" target="_blank" rel="noreferrer">github.com/pspoerri/mssql-webui</a></p>
    </div>
  )
}
