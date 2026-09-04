import { useEffect, useState } from 'react'
import { api, enc, type Selection, type ServerInfo, type TableInfo } from './api'

type DbInfo = { schemas: string[]; tables: TableInfo[] }
type Props = { selected: Selection | null; onSelect: (s: Selection) => void }

export function Tree({ selected, onSelect }: Props) {
  const [servers, setServers] = useState<ServerInfo[]>([])
  const [open, setOpen] = useState<Record<string, DbInfo>>({})
  const [loading, setLoading] = useState('') // "srv/db" being fetched; serverless databases take a while to resume
  const [err, setErr] = useState('')
  const [system, setSystem] = useState(() => localStorage.getItem('showSystemDbs') === '1')

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

  const isSel = (srv: string, db: string, t?: TableInfo, isConsole?: boolean) =>
    selected?.srv === srv && selected?.db === db && !!selected?.console === !!isConsole &&
    selected?.table?.schema === t?.schema && selected?.table?.name === t?.name

  return (
    <nav>
      {err && <div className="error">{err}</div>}
      <ul>
        {servers.map((s) => (
          <li key={s.name}>
            <b>{s.name}</b>
            {s.error && <div className="error">{s.error}</div>}
            <ul>
              {s.databases.map(({ name: db, access }) => {
                const info = open[`${s.name}/${db}`]
                return (
                  <li key={db}>
                    <button type="button" onClick={() => toggle(s.name, db)} aria-busy={loading === `${s.name}/${db}`}
                      className={access ? '' : 'noaccess'} aria-disabled={!access}
                      title={access ? undefined : `You have no access to ${db}`}>
                      {info ? '▾' : '▸'} {db}{loading === `${s.name}/${db}` && <span className="kind">loading…</span>}
                    </button>
                    {info && (
                      <ul>
                        <li>
                          <button type="button" className={isSel(s.name, db, undefined, true) ? 'selected' : ''}
                            onClick={() => onSelect({ srv: s.name, db, console: true })}>SQL console</button>
                        </li>
                        {info.schemas.map((schema) => (
                          <li key={schema}>
                            <span className="schema">{schema}</span>
                            <ul>
                              {info.tables.filter((t) => t.schema === schema).map((t) => (
                                <li key={t.name}>
                                  <button type="button" className={isSel(s.name, db, t) ? 'selected' : ''}
                                    onClick={() => onSelect({ srv: s.name, db, table: t })}>
                                    {t.name}{t.kind === 'view' && <span className="kind">view</span>}
                                  </button>
                                </li>
                              ))}
                            </ul>
                          </li>
                        ))}
                        <li>
                          <button type="button" className="add"
                            onClick={() => create(`/api/s/${enc(s.name)}/d/${enc(db)}/schemas`, 'schema', () => load(s.name, db))}>+ schema</button>
                        </li>
                      </ul>
                    )}
                  </li>
                )
              })}
              <li>
                <button type="button" className="add"
                  onClick={() => create(`/api/s/${enc(s.name)}/databases`, 'database', loadServers)}>+ database</button>
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
