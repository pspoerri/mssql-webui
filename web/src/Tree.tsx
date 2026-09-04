import { useEffect, useState } from 'react'
import { api, enc, type Selection, type ServerInfo, type TableInfo } from './api'

type Props = { selected: Selection | null; onSelect: (s: Selection) => void }

export function Tree({ selected, onSelect }: Props) {
  const [servers, setServers] = useState<ServerInfo[]>([])
  const [open, setOpen] = useState<Record<string, TableInfo[]>>({})
  const [err, setErr] = useState('')

  useEffect(() => {
    api<ServerInfo[]>('/api/servers').then(setServers).catch((e) => setErr(e.message))
  }, [])

  const toggle = async (srv: string, db: string) => {
    const key = `${srv}/${db}`
    if (open[key]) {
      setOpen((o) => {
        const next = { ...o }
        delete next[key]
        return next
      })
      return
    }
    try {
      const tables = await api<TableInfo[]>(`/api/s/${enc(srv)}/d/${enc(db)}/tables`)
      setOpen((o) => ({ ...o, [key]: tables }))
      setErr('')
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
              {s.databases.map((db) => (
                <li key={db}>
                  <button type="button" onClick={() => toggle(s.name, db)}>{open[`${s.name}/${db}`] ? '▾' : '▸'} {db}</button>
                  {open[`${s.name}/${db}`] && (
                    <ul>
                      <li>
                        <button type="button" className={isSel(s.name, db, undefined, true) ? 'selected' : ''}
                          onClick={() => onSelect({ srv: s.name, db, console: true })}>SQL console</button>
                      </li>
                      {open[`${s.name}/${db}`].map((t) => (
                        <li key={`${t.schema}.${t.name}`}>
                          <button type="button" className={isSel(s.name, db, t) ? 'selected' : ''}
                            onClick={() => onSelect({ srv: s.name, db, table: t })}>
                            {t.schema}.{t.name}{t.kind === 'view' && <span className="kind">view</span>}
                          </button>
                        </li>
                      ))}
                    </ul>
                  )}
                </li>
              ))}
            </ul>
          </li>
        ))}
      </ul>
    </nav>
  )
}
