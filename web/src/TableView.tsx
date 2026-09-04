import { useCallback, useEffect, useState } from 'react'
import { api, cellToString, enc, type Cell, type RowsPage, type TableInfo, type TableMeta } from './api'

type Props = { srv: string; db: string; table: TableInfo }
type Values = Record<string, string | null>

const LIMIT = 100

export function TableView({ srv, db, table }: Props) {
  const base = `/api/s/${enc(srv)}/d/${enc(db)}/t/${enc(table.schema)}/${enc(table.name)}`
  const [meta, setMeta] = useState<TableMeta | null>(null)
  const [page, setPage] = useState<RowsPage | null>(null)
  const [offset, setOffset] = useState(0)
  const [edits, setEdits] = useState<Record<number, Values>>({})
  const [deleted, setDeleted] = useState<Set<number>>(new Set())
  const [added, setAdded] = useState<Values[]>([])
  const [err, setErr] = useState('')

  const reset = () => {
    setEdits({})
    setDeleted(new Set())
    setAdded([])
  }

  const load = useCallback(() => {
    api<RowsPage>(`${base}/rows?offset=${offset}&limit=${LIMIT}`)
      .then((p) => { setPage(p); setErr('') })
      .catch((e) => setErr(e.message))
  }, [base, offset])

  useEffect(() => {
    api<TableMeta>(base).then(setMeta).catch((e) => setErr(e.message))
  }, [base])
  useEffect(() => { load() }, [load])

  const editable = !!meta && meta.pk.length > 0 && table.kind === 'table'
  const dirty = Object.keys(edits).length > 0 || deleted.size > 0 || added.length > 0
  const colMeta = (name: string) => meta?.columns.find((c) => c.name === name)
  const isReadonly = (name: string) => !editable || (colMeta(name)?.readonly ?? true)
  // ponytail: clearing a nullable cell means NULL, a non-nullable one means "".
  // Add an explicit NULL toggle if someone needs an empty string in a nullable column.
  const normalize = (name: string, v: string) => (v === '' && colMeta(name)?.nullable ? null : v)

  const edit = (row: number, col: string, v: string) =>
    setEdits({ ...edits, [row]: { ...edits[row], [col]: normalize(col, v) } })
  const toggleDelete = (row: number) => {
    const next = new Set(deleted)
    next.has(row) ? next.delete(row) : next.add(row)
    setDeleted(next)
  }
  const go = (next: number) => {
    if (dirty && !confirm('Discard unsaved changes?')) return
    reset()
    setOffset(next)
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
      load()
    } catch (e) {
      setErr((e as Error).message)
    }
  }

  if (!page) return err ? <div className="error">{err}</div> : <p>Loading…</p>

  return (
    <div>
      <div className="toolbar">
        <b>{table.schema}.{table.name}</b>
        <button disabled={offset === 0} onClick={() => go(offset - LIMIT)}>Prev</button>
        <span>{offset + 1}–{offset + page.rows.length}</span>
        <button disabled={!page.hasMore} onClick={() => go(offset + LIMIT)}>Next</button>
        {editable ? (
          <>
            <button onClick={() => setAdded([...added, {}])}>Add row</button>
            <button disabled={!dirty} onClick={save}>Save</button>
            <button disabled={!dirty} onClick={reset}>Discard</button>
          </>
        ) : (
          <span className="note">{table.kind === 'view' ? 'View: read-only' : 'No primary key: read-only'}</span>
        )}
      </div>
      {err && <div className="error">{err}</div>}
      <table>
        <thead>
          <tr>
            {editable && <th />}
            {page.columns.map((c) => <th key={c} title={colMeta(c)?.type}>{c}</th>)}
          </tr>
        </thead>
        <tbody>
          {page.rows.map((row, i) => (
            <tr key={i} className={deleted.has(i) ? 'deleted' : ''}>
              {editable && (
                <td><input type="checkbox" title="Delete" aria-label="Delete row" checked={deleted.has(i)} onChange={() => toggleDelete(i)} /></td>
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
                  {isReadonly(col) ? '' : (
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
    </div>
  )
}
