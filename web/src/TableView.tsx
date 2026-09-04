import { useCallback, useEffect, useRef, useState } from 'react'
import { api, cellToString, enc, type Cell, type RowsPage, type TableInfo, type TableMeta } from './api'

type Props = { srv: string; db: string; table: TableInfo; onDirty: (d: boolean) => void }
type Values = Record<string, string | null>

const LIMIT = 100

export function TableView({ srv, db, table, onDirty }: Props) {
  const base = `/api/s/${enc(srv)}/d/${enc(db)}/t/${enc(table.schema)}/${enc(table.name)}`
  const [meta, setMeta] = useState<TableMeta | null>(null)
  const [page, setPage] = useState<RowsPage | null>(null)
  const [loading, setLoading] = useState(false)
  const [q, setQ] = useState(() => new URLSearchParams(location.search).get('q') ?? '')
  const [draft, setDraft] = useState(q)
  const [edits, setEdits] = useState<Record<number, Values>>({})
  const [deleted, setDeleted] = useState<Set<number>>(new Set())
  const [added, setAdded] = useState<Values[]>([])
  const [err, setErr] = useState('')
  const gen = useRef(0) // bumps when q changes so a late page from the previous search is dropped
  const sentinel = useRef<HTMLDivElement>(null)

  const reset = () => {
    setEdits({})
    setDeleted(new Set())
    setAdded([])
  }

  // Loads rows from offset; offset 0 replaces the page, anything else appends.
  const load = useCallback((offset: number) => {
    const g = gen.current
    setLoading(true)
    api<RowsPage>(`${base}/rows?offset=${offset}&limit=${LIMIT}&q=${enc(q)}`)
      .then((p) => {
        if (g !== gen.current) return
        setPage((prev) => (offset && prev ? { ...p, rows: [...prev.rows, ...p.rows] } : p))
        setErr('')
      })
      .catch((e) => g === gen.current && setErr(e.message))
      .finally(() => g === gen.current && setLoading(false))
  }, [base, q])

  useEffect(() => {
    api<TableMeta>(base).then(setMeta).catch((e) => setErr(e.message))
  }, [base])
  useEffect(() => {
    gen.current++
    history.replaceState(null, '', location.pathname + (q ? `?q=${enc(q)}` : ''))
    reset()
    setPage(null)
    load(0)
  }, [load, q])

  // Infinite scroll: when the sentinel under the table becomes visible, fetch the next page.
  // ponytail: rows stay in the DOM; add virtualization if scrolling thousands of rows lags.
  useEffect(() => {
    const el = sentinel.current
    if (!el || !page?.hasMore || loading) return
    const io = new IntersectionObserver(([e]) => e.isIntersecting && load(page.rows.length))
    io.observe(el)
    return () => io.disconnect()
  }, [page, loading, load])

  const isTable = table.kind === 'table'
  const editable = isTable && !!meta && meta.pk.length > 0 // no PK: append-only
  const dirty = Object.keys(edits).length > 0 || deleted.size > 0 || added.length > 0
  useEffect(() => {
    onDirty(dirty)
    return () => onDirty(false)
  }, [dirty, onDirty])
  const colMeta = (name: string) => meta?.columns.find((c) => c.name === name)
  const isReadonly = (name: string) => !editable || (colMeta(name)?.readonly ?? true)
  const canInsert = (name: string) => isTable && !(colMeta(name)?.readonly ?? true)
  // ponytail: clearing a nullable cell means NULL, a non-nullable one means "".
  // Add an explicit NULL toggle if someone needs an empty string in a nullable column.
  const normalize = (name: string, v: string) => (v === '' && colMeta(name)?.nullable ? null : v)

  const edit = (row: number, col: string, v: string) =>
    setEdits({ ...edits, [row]: { ...edits[row], [col]: normalize(col, v) } })
  const toggleDelete = (row: number) => {
    const next = new Set(deleted)
    if (next.has(row)) next.delete(row)
    else next.add(row)
    setDeleted(next)
  }
  const search = (e: React.FormEvent) => {
    e.preventDefault()
    if (draft.trim() === q) return
    if (dirty && !confirm('Discard unsaved changes?')) return
    setQ(draft.trim())
  }

  const save = async () => {
    if (!page || !meta) return
    const idx = (c: string) => page.columns.indexOf(c)
    const keyOf = (row: Cell[]) => Object.fromEntries(meta.pk.map((k) => [k, cellToString(row[idx(k)])]))
    const body = {
      inserts: added,
      updates: Object.entries(edits)
        .filter(([i]) => !deleted.has(+i))
        .map(([i, set]) => ({ key: keyOf(page.rows[+i]), set })),
      deletes: [...deleted].map((i) => keyOf(page.rows[i])),
    }
    try {
      await api(`${base}/rows`, body)
      reset()
      load(0) // ponytail: reloads the first page only; scroll position is lost after a save
    } catch (e) {
      setErr((e as Error).message)
    }
  }

  if (!page) return err ? <div className="error">{err}</div> : <p>Loading…</p>

  return (
    <div>
      <div className="toolbar">
        <b>{table.schema}.{table.name}</b>
        <form onSubmit={search}>
          <input type="search" value={draft} placeholder="Search: word or col=value" aria-label="Search"
            title="Words match any column; col=value matches one column exactly; all terms must match"
            onChange={(e) => setDraft(e.target.value)} />
        </form>
        <span>{page.rows.length}{page.hasMore ? '+' : ''} rows</span>
        <a href={`${base}/csv`} download={`${table.schema}.${table.name}.csv`}>Download CSV</a>
        {isTable ? (
          <>
            <button onClick={() => setAdded([...added, {}])}>Add row</button>
            <button disabled={!dirty} className={dirty ? 'unsaved' : ''} title={dirty ? 'Unsaved changes' : ''} onClick={save}>Save</button>
            <button disabled={!dirty} onClick={reset}>Discard</button>
            {meta && !editable && <span className="note">No primary key: append-only</span>}
          </>
        ) : (
          <span className="note">View: read-only</span>
        )}
      </div>
      {err && <div className="error">{err}</div>}
      <table>
        <thead>
          <tr>
            {isTable && <th />}
            {page.columns.map((c) => <th key={c} title={colMeta(c)?.type}>{c}</th>)}
          </tr>
        </thead>
        <tbody>
          {page.rows.map((row, i) => (
            <tr key={i} className={deleted.has(i) ? 'deleted' : ''}>
              {isTable && (
                <td>{editable && <input type="checkbox" title="Delete" aria-label="Delete row" checked={deleted.has(i)} onChange={() => toggleDelete(i)} />}</td>
              )}
              {row.map((v, j) => {
                const col = page.columns[j]
                const val = edits[i] && col in edits[i] ? edits[i][col] : cellToString(v)
                return (
                  <td key={col}>
                    {isReadonly(col) ? (
                      v === null ? <span className="null">NULL</span> : String(v)
                    ) : (
                      <input type="text" value={val ?? ''} placeholder={val === null ? 'NULL' : ''}
                        aria-label={col}
                        onChange={(e) => edit(i, col, e.target.value)} />
                    )}
                  </td>
                )
              })}
            </tr>
          ))}
          {added.map((row, i) => (
            <tr key={`new${i}`} className="new">
              <td><button title="Remove" aria-label="Remove new row" onClick={() => setAdded(added.filter((_, k) => k !== i))}>×</button></td>
              {page.columns.map((col) => (
                <td key={col}>
                  {canInsert(col) && (
                    <input type="text" value={row[col] ?? ''} placeholder="NULL"
                      aria-label={col}
                      onChange={(e) => setAdded(added.map((r, k) => (k === i ? { ...r, [col]: normalize(col, e.target.value) } : r)))} />
                  )}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
      <div ref={sentinel} className="note">{loading ? 'Loading…' : page.hasMore ? 'Scroll for more' : ''}</div>
    </div>
  )
}
