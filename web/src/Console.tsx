import { useEffect, useState } from 'react'
import { api, download, enc, type HttpError, type QueryResult } from './api'

// The SQL console: runs a batch and shows everything it returned in order,
// like SSMS: result sets, rows-affected counts, PRINT messages, the error.
export function Console({ srv, db }: { srv: string; db: string }) {
  const [sql, setSql] = useState('')
  const [result, setResult] = useState<(QueryResult & { ms: number }) | null>(null)
  const [err, setErr] = useState('')
  const [started, setStarted] = useState(0) // while running
  const [now, setNow] = useState(0)
  const path = `/api/s/${enc(srv)}/d/${enc(db)}/query`

  useEffect(() => {
    if (!started) return
    const id = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(id)
  }, [started])

  const run = async () => {
    if (started || !sql.trim()) return
    const t0 = Date.now()
    setStarted(t0)
    setNow(t0)
    try {
      setResult({ ...(await api<QueryResult>(path, { sql })), ms: Date.now() - t0 })
      setErr('')
    } catch (e) {
      // A SQL error still carries the results before it.
      const data = (e as HttpError).data as Partial<QueryResult> | undefined
      setResult(data?.results ? { results: data.results, messages: data.messages ?? [], ms: Date.now() - t0 } : null)
      setErr((e as Error).message)
    } finally {
      setStarted(0)
    }
  }
  // Download runs the batch again for all rows of result set n, so a batch
  // that changed data asks first.
  const save = (n: number) => {
    const changes = result?.results.filter((r) => 'rowsAffected' in r).length ?? 0
    if (changes && !confirm(`Downloading runs the whole batch again, including its ${changes} data-changing statement${changes === 1 ? '' : 's'}. Run it again?`)) return
    download(`${path}?format=csv&set=${n}`, { sql }, 'query.csv').catch((e) => setErr(e.message))
  }

  return (
    <div className="console">
      <textarea rows={8} value={sql} placeholder={`SELECT TOP 100 * FROM ...   (runs against ${db})`}
        aria-label="SQL" spellCheck={false}
        onChange={(e) => setSql(e.target.value)}
        onKeyDown={(e) => { if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) run() }} />
      <div className="toolbar">
        <button className="primary" disabled={!sql.trim() || !!started} onClick={run}>
          {started ? `Running… ${Math.round((now - started) / 1000)} s` : 'Run'}
        </button>
        <span className="note"><kbd>Ctrl</kbd>+<kbd>Enter</kbd></span>
        {result && !started && <span className="count">{result.ms < 1000 ? `${result.ms} ms` : `${(result.ms / 1000).toFixed(1)} s`}</span>}
      </div>
      {err && <div className="error">{err}</div>}
      {result && result.messages.length > 0 && <pre className="messages">{result.messages.join('\n')}</pre>}
      {result?.results.map((r, i) => 'rowsAffected' in r ? (
        <p key={i} className="note">{r.rowsAffected} row{r.rowsAffected === 1 ? '' : 's'} affected</p>
      ) : (
        <section key={i} className="result">
          <header>
            <span className="note">{r.truncated ? `First ${r.rows.length} rows shown; the query returned more (Download CSV has them all)` : `${r.rows.length} row${r.rows.length === 1 ? '' : 's'}`}</span>
            <button className="quiet" disabled={!!started} onClick={() => save(result.results.slice(0, i).filter((x) => 'columns' in x).length)}
              title="Run the batch again and download all rows of this result as CSV">Download CSV</button>
          </header>
          <table>
            <thead><tr>{r.columns.map((c, j) => <th key={j}>{c}</th>)}</tr></thead>
            <tbody>
              {r.rows.map((row, k) => (
                <tr key={k}>{row.map((v, j) => <td key={j} className={typeof v === 'number' ? 'num' : ''}>{v === null ? <span className="null">NULL</span> : String(v)}</td>)}</tr>
              ))}
            </tbody>
          </table>
        </section>
      ))}
      {result && !err && result.results.length === 0 && <p className="note">Done; the batch returned no results.</p>}
    </div>
  )
}
