import { useState } from 'react'
import { api, enc, type QueryResult } from './api'

export function Console({ srv, db }: { srv: string; db: string }) {
  const [sql, setSql] = useState('')
  const [result, setResult] = useState<QueryResult | null>(null)
  const [err, setErr] = useState('')

  const run = () =>
    api<QueryResult>(`/api/s/${enc(srv)}/d/${enc(db)}/query`, { sql })
      .then((r) => { setResult(r); setErr('') })
      .catch((e) => setErr(e.message))

  return (
    <div>
      <div className="toolbar"><b>{db}</b> SQL console</div>
      <textarea rows={8} value={sql} placeholder="SELECT TOP 100 * FROM ..."
        aria-label="SQL"
        onChange={(e) => setSql(e.target.value)}
        onKeyDown={(e) => { if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) run() }} />
      <div className="toolbar"><button onClick={run}>Run</button><span className="note">Ctrl+Enter</span></div>
      {err && <div className="error">{err}</div>}
      {result && ('rowsAffected' in result ? (
        <p>{result.rowsAffected} row(s) affected</p>
      ) : (
        <table>
          <thead><tr>{result.columns.map((c, i) => <th key={i}>{c}</th>)}</tr></thead>
          <tbody>
            {result.rows.map((row, i) => (
              <tr key={i}>{row.map((v, j) => <td key={j}>{v === null ? <span className="null">NULL</span> : String(v)}</td>)}</tr>
            ))}
          </tbody>
        </table>
      ))}
    </div>
  )
}
