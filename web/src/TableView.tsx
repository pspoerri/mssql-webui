import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { api, cellToString, enc, type Cell, type RowsPage, type TableInfo, type TableMeta } from './api'
import { formatTerms, parseTerms, type Op } from './search'

type Props = { srv: string; db: string; table: TableInfo; onDirty: (d: boolean) => void }
type Values = Record<string, string | null>
type Focus = { i: number; col: string; added: boolean } // the cell shown in the field bar
type Sort = { col: string; desc: boolean }
type Filter = { col: string; op: Op; val: string } // the filter popover being edited

const LIMIT = 100
const ICONS = 40 // room for the sort/filter icons in a header

export function TableView({ srv, db, table, onDirty }: Props) {
  const base = `/api/s/${enc(srv)}/d/${enc(db)}/t/${enc(table.schema)}/${enc(table.name)}`
  const [meta, setMeta] = useState<TableMeta | null>(null)
  const [page, setPage] = useState<RowsPage | null>(null)
  const [loading, setLoading] = useState(false)
  const params = new URLSearchParams(location.search)
  const [q, setQ] = useState(() => params.get('q') ?? '')
  const [sort, setSort] = useState<Sort>(() => ({ col: params.get('sort') ?? '', desc: params.get('dir') === 'desc' }))
  const [filter, setFilter] = useState<Filter | null>(null)
  const startRow = useRef(Math.max(0, +(params.get('row') ?? 1) - 1)) // ?row= to scroll to once loaded; 0 = none
  const inflight = useRef(false)
  const [draft, setDraft] = useState(q)
  const [edits, setEdits] = useState<Record<number, Values>>({})
  const [deleted, setDeleted] = useState<Set<number>>(new Set())
  const [added, setAdded] = useState<Values[]>([])
  const [focus, setFocus] = useState<Focus | null>(null)
  const [err, setErr] = useState('')
  const gen = useRef(0) // bumps when q changes so a late page from the previous search is dropped
  const sentinel = useRef<HTMLDivElement>(null)
  const tableRef = useRef<HTMLTableElement>(null)
  const topRef = useRef<HTMLDivElement>(null)
  const [topH, setTopH] = useState(0) // height of the sticky toolbar block; the header sticks below it
  const [widths, setWidths] = useState<Record<string, number>>({})
  const drag = useRef<{ col: string; x: number; w: number } | null>(null)
  const width = (c: string) => widths[c] ?? 120

  const reset = () => {
    setEdits({})
    setDeleted(new Set())
    setAdded([])
    setFocus(null)
  }

  // Loads rows from offset; offset 0 replaces the page, anything else appends.
  const load = useCallback((offset: number, limit = LIMIT) => {
    if (offset && inflight.current) return
    const g = gen.current
    inflight.current = true
    setLoading(true)
    const order = sort.col ? `&sort=${enc(sort.col)}&dir=${sort.desc ? 'desc' : 'asc'}` : ''
    api<RowsPage>(`${base}/rows?offset=${offset}&limit=${limit}&q=${enc(q)}${order}`)
      .then((p) => {
        if (g !== gen.current) return
        setPage((prev) => (offset && prev ? { ...p, rows: [...prev.rows, ...p.rows] } : p))
        setErr('')
      })
      .catch((e) => g === gen.current && setErr(e.message))
      .finally(() => {
        inflight.current = false
        if (g === gen.current) setLoading(false)
      })
  }, [base, q, sort])

  useEffect(() => {
    api<TableMeta>(base).then(setMeta).catch((e) => setErr(e.message))
  }, [base])
  useEffect(() => {
    gen.current++
    setUrl(q, sort, startRow.current)
    reset()
    setPage(null)
    load(0, Math.min(500, Math.ceil((startRow.current + 1) / LIMIT) * LIMIT)) // 500 is the backend cap
  }, [load, q, sort])

  useLayoutEffect(() => {
    const h = Math.floor(topRef.current?.getBoundingClientRect().height ?? 0) // floor: overlap beats a hairline gap
    if (h !== topH) setTopH(h)
  })

  // ?row=: keep loading until the row exists with a page of rows after it (so the scroll is
  // not clamped at the end of the table), then scroll it under the sticky header.
  useEffect(() => {
    const target = startRow.current
    if (!target || !page || loading) return
    if (page.rows.length < target + LIMIT && page.hasMore) {
      load(page.rows.length)
      return
    }
    if (page.rows.length <= target) {
      startRow.current = 0
      return
    }
    startRow.current = 0
    scrollToRow(tableRef.current, target, topH)
  }, [page, loading, load, topH])

  // Scrolling updates ?row= with the first visible row (1-based).
  // ponytail: assumes uniform row height; rows are single-line, so it holds.
  useEffect(() => {
    const t = tableRef.current
    const main = t?.closest('main')
    if (!t || !main || !page) return
    let last = -1
    const onScroll = () => {
      if (startRow.current) return // still restoring
      const rowH = t.tBodies[0].offsetHeight / Math.max(1, t.tBodies[0].rows.length)
      const top = main.getBoundingClientRect().top - t.getBoundingClientRect().top + topH
      const row = Math.min(page.rows.length - 1, Math.max(0, Math.round(top / rowH)))
      if (row !== last) setUrl(q, sort, (last = row))
    }
    main.addEventListener('scroll', onScroll, { passive: true })
    return () => main.removeEventListener('scroll', onScroll)
  }, [page, q, sort, topH])

  // Infinite scroll: when the sentinel under the table becomes visible, fetch the next page.
  // ponytail: rows stay in the DOM; add virtualization if scrolling thousands of rows lags.
  useEffect(() => {
    const el = sentinel.current
    if (!el || !page?.hasMore || loading) return
    const io = new IntersectionObserver(([e]) => e.isIntersecting && load(page.rows.length))
    io.observe(el)
    return () => io.disconnect()
  }, [page, loading, load])

  // Column widths: auto-fit (capped) when a column first appears; drag the header edge to
  // resize, double-click it to fit the loaded content, double-click again to fit the label.
  // ponytail: widths are per mount; persist them in localStorage if people ask.
  const fitWidth = (col: string, cap: number) => {
    if (!page || !tableRef.current) return 120
    const j = page.columns.indexOf(col)
    // Long values never widen a column past 120 chars; the full value is in the cell's tooltip.
    const texts = page.rows.map((r, i) => (edits[i]?.[col] ?? cellToString(r[j]) ?? 'NULL').slice(0, 120))
    const w = Math.max(textWidth(tableRef.current, [col], true) + ICONS, textWidth(tableRef.current, texts, false))
    return Math.min(cap, w) + 14
  }
  useLayoutEffect(() => {
    if (!page) return
    setWidths((w) => {
      const next = { ...w }
      for (const c of page.columns) if (!(c in next)) next[c] = fitWidth(c, 300)
      return next
    })
  }, [page]) // eslint-disable-line react-hooks/exhaustive-deps
  const fit = (col: string) => {
    const content = fitWidth(col, Infinity)
    const label = (tableRef.current ? textWidth(tableRef.current, [col], true) : 100) + ICONS + 14
    setWidths((w) => ({ ...w, [col]: w[col] === content ? label : content }))
  }

  const isTable = table.kind === 'table'
  const pk = meta?.pk ?? []
  const editable = isTable && pk.length > 0 // no PK: append-only
  const dirty = Object.keys(edits).length > 0 || deleted.size > 0 || added.length > 0
  useEffect(() => {
    onDirty(dirty)
    return () => onDirty(false)
  }, [dirty, onDirty])
  const colMeta = (name: string) => meta?.columns.find((c) => c.name === name)
  const typeLabel = (name: string) => {
    const c = colMeta(name)
    return c ? [c.type, c.nullable ? 'null' : 'not null', c.identity && 'identity', c.readonly && 'read-only'].filter(Boolean).join(', ') : ''
  }
  const isReadonly = (name: string) => !editable || (colMeta(name)?.readonly ?? true)
  const canInsert = (name: string) => isTable && !(colMeta(name)?.readonly ?? true)
  // ponytail: clearing a nullable cell means NULL, a non-nullable one means "".
  // Add an explicit NULL toggle if someone needs an empty string in a nullable column.
  const normalize = (name: string, v: string) => (v === '' && colMeta(name)?.nullable ? null : v)

  // Key columns (and the delete checkbox) stay put while scrolling sideways.
  const stickyLeft = (col: string) => {
    let x = isTable ? 28 : 0
    for (const c of page?.columns ?? []) {
      if (c === col) return x
      if (pk.includes(c)) x += width(c)
    }
    return 0
  }
  const cellClass = (col: string, changed: boolean) =>
    [pk.includes(col) && 'sticky', changed && 'changed'].filter(Boolean).join(' ')

  const idx = (c: string) => page?.columns.indexOf(c) ?? -1
  const cellValue = (f: Focus): string | null =>
    f.added ? added[f.i]?.[f.col] ?? null
      : edits[f.i] && f.col in edits[f.i] ? edits[f.i][f.col] : cellToString(page!.rows[f.i][idx(f.col)])
  const setCell = (f: Focus, v: string) => {
    const val = normalize(f.col, v)
    if (f.added) setAdded(added.map((r, k) => (k === f.i ? { ...r, [f.col]: val } : r)))
    else setEdits({ ...edits, [f.i]: { ...edits[f.i], [f.col]: val } })
  }
  const isChanged = (f: Focus) => (f.added ? f.col in (added[f.i] ?? {}) : !!edits[f.i] && f.col in edits[f.i])
  // Revert one cell to its loaded value and leave edit mode (Escape, or the field bar's Revert).
  const revertCell = (f: Focus) => {
    if (f.added) {
      setAdded(added.map((r, k) => {
        if (k !== f.i) return r
        const { [f.col]: _, ...rest } = r
        return rest
      }))
    } else if (edits[f.i]) {
      const { [f.col]: _, ...rest } = edits[f.i]
      const next = { ...edits }
      if (Object.keys(rest).length) next[f.i] = rest
      else delete next[f.i]
      setEdits(next)
    }
    ;(document.activeElement as HTMLElement | null)?.blur()
  }
  // Esc reverts the cell; Enter saves everything (Ctrl/Cmd+Enter in a textarea, where Enter is a newline).
  const onKey = (f: Focus) => (e: React.KeyboardEvent) => {
    if (e.key === 'Escape') {
      e.preventDefault()
      revertCell(f)
    } else if (e.key === 'Enter' && (!(e.target instanceof HTMLTextAreaElement) || e.ctrlKey || e.metaKey)) {
      e.preventDefault()
      if (dirty) save()
    }
  }
  const toggleDelete = (row: number) => {
    const next = new Set(deleted)
    if (next.has(row)) next.delete(row)
    else next.add(row)
    setDeleted(next)
  }
  // A new query or sort reloads from the top; pending edits are index-based, so ask first.
  const applyQ = (next: string) => {
    next = next.trim()
    if (next === q) return
    if (dirty && !confirm('Discard unsaved changes?')) return
    startRow.current = 0
    setDraft(next)
    setQ(next)
  }
  const search = (e: React.FormEvent) => {
    e.preventDefault()
    applyQ(draft)
  }
  const toggleSort = (col: string) => {
    if (dirty && !confirm('Discard unsaved changes?')) return
    startRow.current = 0
    setSort(sort.col !== col ? { col, desc: false } : sort.desc ? { col: '', desc: false } : { col, desc: true })
  }
  // Column filters are terms in the search box; the popover edits the term for one column.
  const filtered = (col: string) => parseTerms(q).some((t) => t.col?.toLowerCase() === col.toLowerCase())
  const openFilter = (col: string) => {
    const t = parseTerms(q).find((t) => t.col?.toLowerCase() === col.toLowerCase())
    setFilter({ col, op: t?.op ?? '^', val: t?.val ?? '' })
  }
  const setColumnFilter = (col: string, term: { op: Op; val: string } | null) => {
    const rest = parseTerms(q).filter((t) => t.col?.toLowerCase() !== col.toLowerCase())
    applyQ(formatTerms(term ? [...rest, { col, ...term }] : rest))
    setFilter(null)
  }

  const keyOf = (row: Cell[]) => Object.fromEntries(pk.map((k) => [k, cellToString(row[idx(k)])]))
  const save = async () => {
    if (!page || !meta) return
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

  const focusValue = focus ? cellValue(focus) : null
  const focusMulti = !!focus && /char|text|xml/i.test(colMeta(focus.col)?.type ?? '') // only text types can hold line breaks
  const focusKey = focus && (focus.added ? 'new row' : Object.entries(keyOf(page.rows[focus.i])).map(([k, v]) => `${k}=${v}`).join(' '))

  return (
    <div className="grid">
      <div className="top" ref={topRef}>
        <div className="toolbar">
          <b>{table.schema}.{table.name}</b>
          <form onSubmit={search}>
            <input type="search" value={draft} placeholder="Search: word, col=value, col^prefix, col~part" aria-label="Search"
              title="Words must all occur in one column; col=value matches one column exactly; quotes keep spaces together: name='User 1002'; all terms must match"
              onChange={(e) => setDraft(e.target.value)} />
          </form>
          <span>{page.rows.length}{page.hasMore ? '+' : ''} rows</span>
          <a href={`${base}/csv`} download={`${table.schema}.${table.name}.csv`}>Download CSV</a>
          {isTable ? (
            <>
              <button onClick={() => setAdded([...added, {}])}>Add row</button>
              <button disabled={!dirty} className={dirty ? 'unsaved' : ''} title={dirty ? 'Unsaved changes (Enter)' : ''} onClick={save}>Save</button>
              <button disabled={!dirty} onClick={reset}>Discard</button>
              {meta && !editable && <span className="note">No primary key: append-only</span>}
            </>
          ) : (
            <span className="note">View: read-only</span>
          )}
        </div>
        {filter && (
          <form className="fieldbar" onSubmit={(e) => { e.preventDefault(); setColumnFilter(filter.col, filter.val ? filter : null) }}>
            <label htmlFor="filterval">Filter <b>{filter.col}</b></label>
            <select value={filter.op} aria-label="Operator" onChange={(e) => setFilter({ ...filter, op: e.target.value as Op })}>
              <option value="^">starts with</option>
              <option value="~">contains</option>
              <option value="=">equals</option>
            </select>
            <input id="filterval" type="text" autoFocus value={filter.val} onChange={(e) => setFilter({ ...filter, val: e.target.value })} />
            <button type="submit">Apply</button>
            <button type="button" aria-label="Close" onClick={() => setFilter(null)}>×</button>
          </form>
        )}
        {focus && (
          <div className="fieldbar">
            <label htmlFor="fieldbar">{focus.col} <span className="note">({focusKey}) · {typeLabel(focus.col)}</span></label>
            {focusMulti ? (
              <textarea id="fieldbar" rows={2} value={focusValue ?? ''} placeholder={focusValue === null ? 'NULL' : ''}
                onChange={(e) => setCell(focus, e.target.value)} onKeyDown={onKey(focus)} />
            ) : (
              <input id="fieldbar" type="text" value={focusValue ?? ''} placeholder={focusValue === null ? 'NULL' : ''}
                onChange={(e) => setCell(focus, e.target.value)} onKeyDown={onKey(focus)} />
            )}
            <button type="button" disabled={!isChanged(focus)} title="Undo this cell's change (Esc)" onClick={() => revertCell(focus)}>Revert</button>
            <button type="button" aria-label="Close" title="Leave the cell (keeps the change)" onClick={() => setFocus(null)}>×</button>
          </div>
        )}
      </div>
      {err && <div className="error">{err}</div>}
      <table ref={tableRef} style={{ width: (isTable ? 28 : 0) + page.columns.reduce((n, c) => n + width(c), 0) }}>
        <colgroup>
          {isTable && <col style={{ width: 28 }} />}
          {page.columns.map((c) => <col key={c} style={{ width: width(c) }} />)}
        </colgroup>
        <thead>
          <tr>
            {isTable && <th className="sticky" style={{ top: topH, left: 0 }} />}
            {page.columns.map((c) => (
              <th key={c} title={typeLabel(c)} className={cellClass(c, false)}
                style={{ top: topH, left: pk.includes(c) ? stickyLeft(c) : undefined }}>
                {c}
                <button type="button" className={`hb${sort.col === c ? ' on' : ''}`} onClick={() => toggleSort(c)}
                  title={sort.col === c ? (sort.desc ? 'Sorted descending; click to clear' : 'Sorted ascending; click for descending') : 'Sort'}
                  aria-label={`Sort by ${c}`}>{sort.col === c ? (sort.desc ? '↓' : '↑') : '⇅'}</button>
                <button type="button" className={`hb${filtered(c) ? ' on' : ''}`}
                  onClick={() => (filtered(c) ? setColumnFilter(c, null) : openFilter(c))}
                  title={filtered(c) ? 'Filtered; click to remove' : 'Filter'} aria-label={`Filter ${c}`}>{filtered(c) ? '▼' : '▽'}</button>
                <div className="resizer" title="Drag to resize; double-click to fit content, again to fit label"
                  onPointerDown={(e) => {
                    e.preventDefault()
                    e.currentTarget.setPointerCapture(e.pointerId)
                    drag.current = { col: c, x: e.clientX, w: width(c) }
                  }}
                  onPointerMove={(e) => {
                    const d = drag.current
                    if (d) setWidths((w) => ({ ...w, [d.col]: Math.max(30, d.w + e.clientX - d.x) }))
                  }}
                  onPointerUp={() => { drag.current = null }}
                  onDoubleClick={() => fit(c)} />
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {page.rows.map((row, i) => (
            <tr key={i} className={deleted.has(i) ? 'deleted' : ''}>
              {isTable && (
                <td className="sticky" style={{ left: 0 }}>{editable && <input type="checkbox" title="Delete" aria-label="Delete row" checked={deleted.has(i)} onChange={() => toggleDelete(i)} />}</td>
              )}
              {row.map((v, j) => {
                const col = page.columns[j]
                const changed = !!edits[i] && col in edits[i]
                const val = changed ? edits[i][col] : cellToString(v)
                return (
                  <td key={col} className={cellClass(col, changed)} title={`${val ?? 'NULL'}\n${typeLabel(col)}`} style={pk.includes(col) ? { left: stickyLeft(col) } : undefined}>
                    {isReadonly(col) ? (
                      v === null ? <span className="null">NULL</span> : String(v)
                    ) : (
                      <input type="text" value={val ?? ''} placeholder={val === null ? 'NULL' : ''}
                        aria-label={col}
                        onFocus={() => setFocus({ i, col, added: false })}
                        onKeyDown={onKey({ i, col, added: false })}
                        onChange={(e) => setCell({ i, col, added: false }, e.target.value)} />
                    )}
                  </td>
                )
              })}
            </tr>
          ))}
          {added.map((row, i) => (
            <tr key={`new${i}`} className="new">
              <td className="sticky" style={{ left: 0 }}><button title="Remove" aria-label="Remove new row" onClick={() => { setFocus(null); setAdded(added.filter((_, k) => k !== i)) }}>×</button></td>
              {page.columns.map((col) => (
                <td key={col} className={cellClass(col, false)} style={pk.includes(col) ? { left: stickyLeft(col) } : undefined}>
                  {canInsert(col) && (
                    <input type="text" value={row[col] ?? ''} placeholder="NULL"
                      aria-label={col}
                      onFocus={() => setFocus({ i, col, added: true })}
                      onKeyDown={onKey({ i, col, added: true })}
                      onChange={(e) => setCell({ i, col, added: true }, e.target.value)} />
                  )}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
      <div ref={sentinel} className="note more">{loading ? 'Loading…' : page.hasMore ? 'Scroll for more' : ''}</div>
    </div>
  )
}

function setUrl(q: string, sort: Sort, row: number) {
  const p = new URLSearchParams()
  if (q) p.set('q', q)
  if (sort.col) {
    p.set('sort', sort.col)
    if (sort.desc) p.set('dir', 'desc')
  }
  if (row > 0) p.set('row', String(row + 1))
  const s = p.toString()
  history.replaceState(null, '', location.pathname + (s ? `?${s}` : ''))
}

// Scrolls the enclosing <main> so that body row i sits just below the sticky toolbar and header.
function scrollToRow(t: HTMLTableElement | null, i: number, topH: number) {
  const tr = t?.tBodies[0].rows[i]
  const main = t?.closest('main')
  if (!t || !tr || !main) return
  main.scrollTop += tr.getBoundingClientRect().top - main.getBoundingClientRect().top - topH - t.tHead!.offsetHeight
}

// Widest of texts in el's font (bold for headers), measured off-screen on a canvas.
const canvas = document.createElement('canvas')
function textWidth(el: Element, texts: string[], bold: boolean): number {
  const ctx = canvas.getContext('2d')!
  const st = getComputedStyle(el)
  ctx.font = `${bold ? 'bold' : st.fontWeight} ${st.fontSize} ${st.fontFamily}`
  return Math.ceil(Math.max(0, ...texts.map((t) => ctx.measureText(t).width)))
}
