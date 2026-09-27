import { useEffect, useRef, useState } from 'react'
import { api, del, enc, upload, type HttpError, type Job, type Selection, type ServerInfo, type TableInfo } from './api'
import { Icon } from './icons'

type DbInfo = { schemas: string[]; tables: TableInfo[]; writable: boolean }
type Props = { selected: Selection | null; onSelect: (s: Selection) => void }
type Upload = { id: number; schema: string; table: string; sent: number }

export function Tree({ selected, onSelect }: Props) {
  const [servers, setServers] = useState<ServerInfo[]>([])
  const [open, setOpen] = useState<Record<string, DbInfo>>({})
  const [loading, setLoading] = useState('') // "srv/db" being fetched; serverless databases take a while to resume
  const [err, setErr] = useState('')
  const [system, setSystem] = useState(() => localStorage.getItem('showSystemDbs') === '1')
  const [uploads, setUploads] = useState<Upload[]>([])
  const [jobs, setJobs] = useState<Job[]>([]) // CSV imports running on the server, see pollJobs

  const loadServers = () =>
    api<ServerInfo[]>(`/api/servers?system=${system ? 1 : 0}`).then(setServers).catch((e) => setErr(e.message))
  useEffect(() => { loadServers() }, [system])
  const toggleSystem = (on: boolean) => {
    localStorage.setItem('showSystemDbs', on ? '1' : '0')
    setSystem(on)
  }

  const load = async (srv: string, db: string) => {
    setLoading(`${srv}/${db}`)
    try {
      const info = await api<DbInfo>(`/api/s/${enc(srv)}/d/${enc(db)}/tables`)
      setOpen((o) => ({ ...o, [`${srv}/${db}`]: info }))
      setErr('')
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setLoading('')
    }
  }

  // Deep link: expand the database of the current selection so it is visible.
  const selKey = selected ? `${selected.srv}/${selected.db}` : ''
  useEffect(() => {
    if (selKey && !open[selKey]) load(selected!.srv, selected!.db)
  }, [selKey]) // eslint-disable-line react-hooks/exhaustive-deps

  // Reload the server's database list and the tables of every expanded database.
  const refresh = (srv: string) => {
    loadServers()
    Object.keys(open).filter((k) => k.startsWith(`${srv}/`)).forEach((k) => load(srv, k.slice(srv.length + 1)))
  }

  const toggle = (srv: string, db: string) => {
    const key = `${srv}/${db}`
    if (!open[key]) return load(srv, db)
    setOpen((o) => {
      const next = { ...o }
      delete next[key]
      return next
    })
  }

  // ponytail: window.prompt instead of a dialog; add one if names need validation hints
  const create = async (path: string, what: string, then: () => void) => {
    const name = window.prompt(`New ${what} name`)?.trim()
    if (!name) return
    try {
      await api(path, { name })
      setErr('')
      then()
    } catch (e) {
      setErr((e as Error).message)
    }
  }

  // Import a CSV: pick a file, name the table, upload it. The server runs the
  // import as a job; pollJobs follows it and opens the table when it is done.
  // An existing table gets the rows appended after a confirmation.
  // window.prompt to match create().
  const importCSV = (srv: string, db: string) => {
    const input = document.createElement('input')
    input.type = 'file'
    input.accept = '.csv,text/csv'
    input.onchange = async () => {
      const file = input.files?.[0]
      if (!file) return
      const def = 'dbo.' + (file.name.replace(/\.[^.]*$/, '').replace(/[^\w]+/g, '_') || 'imported')
      const target = window.prompt('Import into new table (schema.table)', def)?.trim()
      if (!target) return
      const dot = target.indexOf('.')
      const schema = dot < 0 ? 'dbo' : target.slice(0, dot)
      const name = dot < 0 ? target : target.slice(dot + 1)
      const base = `/api/s/${enc(srv)}/d/${enc(db)}/t/${enc(schema)}/${enc(name)}`
      let exists: boolean
      setLoading(`${srv}/${db}`) // may wait for a paused database
      try {
        exists = await api(base).then(() => true, (e: HttpError) => { if (e.status === 404) return false; throw e })
      } catch (e) {
        setErr((e as Error).message)
        return
      } finally {
        setLoading('')
      }
      if (exists && !window.confirm(`${schema}.${name} already exists. Append the file's rows to it?`)) return
      const up: Upload = { id: Date.now(), schema, table: name, sent: 0 }
      setUploads((u) => [...u, up])
      try {
        const job = await upload<Job>(`${base}/csv${exists ? '?append=1' : ''}`, file,
          (sent, total) => setUploads((u) => u.map((x) => (x.id === up.id ? { ...x, sent: sent / total } : x))))
        mine.current.add(job.id)
        seen.current[job.id] = job.state
        setJobs((j) => [...j, job])
        setErr('')
      } catch (e) {
        setErr((e as Error).message)
      } finally {
        setUploads((u) => u.filter((x) => x.id !== up.id))
      }
    }
    input.click()
  }

  // Jobs: fetched once (imports survive a reload) and every second while one
  // runs. A job that finishes refreshes its database; one started from this
  // page also opens the table.
  const mine = useRef(new Set<string>())
  const seen = useRef<Record<string, Job['state']>>({})
  const pollJobs = async () => {
    const next = await api<Job[]>('/api/jobs').catch(() => null)
    if (!next) return
    for (const j of next) {
      if (seen.current[j.id] === 'running' && j.state === 'done') {
        if (open[`${j.srv}/${j.db}`]) load(j.srv, j.db)
        if (mine.current.has(j.id)) onSelect({ srv: j.srv, db: j.db, table: { schema: j.schema, name: j.table, kind: 'table' } })
      }
      seen.current[j.id] = j.state
    }
    setJobs(next)
  }
  const poll = useRef(pollJobs)
  useEffect(() => { poll.current = pollJobs })
  const running = jobs.some((j) => j.state === 'running')
  useEffect(() => { poll.current() }, [])
  useEffect(() => {
    if (!running) return
    const id = setInterval(() => poll.current(), 1000)
    return () => clearInterval(id)
  }, [running])
  const dropJob = (j: Job) => {
    if (j.state !== 'running') setJobs((js) => js.filter((x) => x.id !== j.id))
    del(`/api/jobs/${enc(j.id)}`).then(() => poll.current(), (e) => setErr(e.message))
  }

  const isSel = (srv: string, db: string, t?: TableInfo, isConsole?: boolean) =>
    selected?.srv === srv && selected?.db === db && !!selected?.console === !!isConsole &&
    selected?.table?.schema === t?.schema && selected?.table?.name === t?.name

  return (
    <nav>
      {err && <div className="error">{err}</div>}
      {(uploads.length > 0 || jobs.length > 0) && (
        <ul className="jobs" aria-label="Imports">
          {uploads.map((u) => (
            <li key={u.id}>
              <span className="label">Import {u.schema}.{u.table}</span>
              <progress value={u.sent} />
              <span className="note">Uploading… {Math.round(u.sent * 100)}%</span>
            </li>
          ))}
          {jobs.map((j) => (
            <li key={j.id} className={j.state}>
              <span className="label" title={`${j.srv} / ${j.db}`}>{j.append ? 'Append to' : 'Import'} {j.schema}.{j.table}</span>
              <button type="button" className="quiet" onClick={() => dropJob(j)} aria-label={j.state === 'running' ? `Cancel import of ${j.table}` : 'Dismiss'}>
                {j.state === 'running' ? 'Cancel' : '×'}
              </button>
              {j.state === 'running' && <progress value={j.phase === 'checking types' || j.phase === 'inserting' ? j.bytes / j.size : undefined} />}
              <span className={j.state === 'failed' ? 'msg' : 'note'}>{jobText(j)}</span>
            </li>
          ))}
        </ul>
      )}
      <ul>
        {servers.map((s) => (
          <li key={s.name}>
            <div className={s.error ? 'server noaccess' : 'server'}>
              <Icon name="server" />
              <span className="label">{s.name}</span>
              <button type="button" className="refresh" title="Refresh" aria-label={`Refresh ${s.name}`} onClick={() => refresh(s.name)}><Icon name="refresh" /></button>
            </div>
            {s.error && <div className="error">{s.error}</div>}
            <ul>
              {s.databases.map(({ name: db, access }) => {
                const info = open[`${s.name}/${db}`]
                const busy = loading === `${s.name}/${db}`
                return (
                  <li key={db}>
                    <button type="button" onClick={() => toggle(s.name, db)} aria-busy={busy} aria-expanded={!!info}
                      className={[info && 'open', !access && 'noaccess'].filter(Boolean).join(' ')} aria-disabled={!access}
                      title={access ? undefined : `You have no access to ${db}`}>
                      <Icon name="chevron" className="chev" /><Icon name="database" />
                      <span className="label">{db}</span>
                      {busy && <Icon name="spinner" className="spinner" />}
                    </button>
                    {info && (
                      <ul>
                        <li>
                          <button type="button" className={isSel(s.name, db, undefined, true) ? 'selected' : ''}
                            onClick={() => onSelect({ srv: s.name, db, console: true })}>
                            <Icon name="console" /><span className="label">SQL console</span>
                          </button>
                        </li>
                        {info.schemas.map((schema) => (
                          <li key={schema}>
                            <span className="schema">{schema}</span>
                            <ul>
                              {info.tables.filter((t) => t.schema === schema).map((t) => (
                                <li key={t.name}>
                                  <button type="button" className={isSel(s.name, db, t) ? 'selected' : ''} title={t.kind === 'view' ? `${t.name} (view)` : t.name}
                                    onClick={() => onSelect({ srv: s.name, db, table: t })}>
                                    <Icon name={t.kind === 'view' ? 'view' : 'table'} /><span className="label">{t.name}</span>
                                  </button>
                                </li>
                              ))}
                            </ul>
                          </li>
                        ))}
                        <li>
                          <button type="button" className="add"
                            onClick={() => create(`/api/s/${enc(s.name)}/d/${enc(db)}/schemas`, 'schema', () => load(s.name, db))}>+ New schema…</button>
                        </li>
                        {info.writable && (
                          <li>
                            <button type="button" className="add"
                              onClick={() => importCSV(s.name, db)}>+ Import CSV (creates table)…</button>
                          </li>
                        )}
                      </ul>
                    )}
                  </li>
                )
              })}
              <li>
                <button type="button" className="add"
                  onClick={() => create(`/api/s/${enc(s.name)}/databases`, 'database', loadServers)}>+ New database…</button>
              </li>
            </ul>
          </li>
        ))}
      </ul>
      <label className="toggle">
        <input type="checkbox" checked={system} onChange={(e) => toggleSystem(e.target.checked)} /> Show system databases
      </label>
    </nav>
  )
}

const n = (x: number) => x.toLocaleString()

function jobText(j: Job): string {
  const pct = `${Math.round((100 * j.bytes) / Math.max(1, j.size))}%`
  switch (j.state) {
    case 'done': return `${j.append ? 'Appended' : 'Imported'} ${n(j.rows)} rows`
    case 'failed': return j.error
    case 'canceled': return 'Canceled; nothing was imported'
  }
  switch (j.phase) {
    case 'checking types': return `Checking column types… ${pct}`
    case 'inserting': return `Inserting… ${n(j.rows)} rows (${pct})`
    case 'waiting for the database to resume': return 'Waiting for the database to resume…'
    case '': case 'connecting': return 'Connecting…'
  }
  return `${j.phase[0].toUpperCase()}${j.phase.slice(1)}… ${n(j.rows)} rows`
}
