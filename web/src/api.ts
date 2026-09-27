export type DbInfo = { name: string; access: boolean }
export type ServerInfo = { name: string; databases: DbInfo[]; error?: string }
export type TableInfo = { schema: string; name: string; kind: 'table' | 'view' }
export type ColumnInfo = { name: string; type: string; nullable: boolean; identity: boolean; readonly: boolean }
export type TableMeta = { columns: ColumnInfo[]; pk: string[] }
export type Cell = string | number | boolean | null
export type RowsPage = { columns: string[]; rows: Cell[][]; hasMore: boolean }
export type ResultSet = { columns: string[]; rows: Cell[][]; truncated: boolean }
export type QueryResult = { results: (ResultSet | { rowsAffected: number })[]; messages: string[]; error?: string }
export type Selection = { srv: string; db: string; table?: TableInfo; console?: boolean }

export const enc = encodeURIComponent

export type Job = {
  id: string; kind: 'import'; srv: string; db: string; schema: string; table: string; append: boolean
  state: 'running' | 'done' | 'failed' | 'canceled'; phase: string; bytes: number; size: number; rows: number; error: string
}

// GET when body is undefined, POST JSON otherwise. A 401 sends the user to login.
export async function api<T>(path: string, body?: unknown): Promise<T> {
  return handle(await send(
    path,
    body === undefined
      ? undefined
      : { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) },
  ))
}

export async function del<T>(path: string): Promise<T> {
  return handle(await send(path, { method: 'DELETE' }))
}

// POST a raw file body, e.g. a CSV upload. XMLHttpRequest because fetch
// reports no upload progress, and a big file takes a while to send.
export function upload<T>(path: string, file: Blob, onProgress?: (sent: number, total: number) => void): Promise<T> {
  return new Promise((resolve, reject) => {
    const x = new XMLHttpRequest()
    x.open('POST', path)
    x.setRequestHeader('Content-Type', 'text/csv')
    x.upload.onprogress = (e) => onProgress?.(e.loaded, e.total)
    x.onload = () => handle<T>(new Response(x.responseText, { status: x.status, statusText: x.statusText })).then(resolve, reject)
    x.onerror = () => reject(new Error('Upload failed: the connection was dropped (network, or a proxy refusing the file).'))
    x.send(file)
  })
}

// POST JSON and save the answer as a file, e.g. a console result as CSV.
export async function download(path: string, body: unknown, filename: string): Promise<void> {
  const res = await send(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) })
  if (!res.ok) return handle(res)
  const a = document.createElement('a')
  a.href = URL.createObjectURL(await res.blob())
  a.download = filename
  a.click()
  setTimeout(() => URL.revokeObjectURL(a.href), 10_000)
}

// Requests waiting for a paused serverless database to resume. App shows the
// server's message while any are waiting.
let waiting = 0
let waitMsg = ''
const waitListeners = new Set<(msg: string) => void>()
export function onResuming(fn: (msg: string) => void): () => void {
  waitListeners.add(fn)
  return () => { waitListeners.delete(fn) }
}
function setWaiting(delta: number, msg = waitMsg) {
  waiting += delta
  waitMsg = msg
  waitListeners.forEach((fn) => fn(waiting > 0 ? waitMsg : ''))
}
export const retry = { ms: 5000, max: 3 * 60_000 }

// fetch, retrying while the server answers 503 {resuming: true}: nothing ran
// yet, so any request can be repeated.
async function send(path: string, init?: RequestInit): Promise<Response> {
  const until = Date.now() + retry.max
  let waited = false
  try {
    for (;;) {
      const res = await fetch(path, init)
      if (res.status !== 503 || Date.now() > until) return res
      const data = await res.clone().json().catch(() => ({}))
      if (!data.resuming) return res
      if (!waited) { waited = true; setWaiting(1, data.error) }
      await new Promise((r) => setTimeout(r, retry.ms))
    }
  } finally {
    if (waited) setWaiting(-1)
  }
}

export type HttpError = Error & { status: number; data: Record<string, unknown> }

async function handle<T>(res: Response): Promise<T> {
  if (res.status === 401) {
    window.location.href = '/auth/login'
    throw new Error('not logged in')
  }
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw Object.assign(new Error(data.error || httpError(res)), { status: res.status, data }) as HttpError
  return data as T
}

// A failure without our JSON error comes from a proxy in front of the app.
// Over HTTP/2 statusText is empty, so the status must be spelled out.
function httpError(res: Response): string {
  if (res.status === 413) return 'The file is larger than the proxy in front of mssql-webui accepts (HTTP 413).'
  if (res.status === 502 || res.status === 504) return `The server did not answer in time (HTTP ${res.status}).`
  return `HTTP ${res.status}${res.statusText ? ' ' + res.statusText : ''}`
}

export const cellToString = (v: Cell): string | null => (v === null ? null : String(v))

// URL <-> selection: /s/{srv}/d/{db}/t|v/{schema}/{table} or /s/{srv}/d/{db}/console.
export const toPath = (s: Selection): string => {
  const db = `/s/${enc(s.srv)}/d/${enc(s.db)}`
  if (s.console) return `${db}/console`
  if (s.table) return `${db}/${s.table.kind === 'view' ? 'v' : 't'}/${enc(s.table.schema)}/${enc(s.table.name)}`
  return db
}
export const fromPath = (path: string): Selection | null => {
  let p: string[]
  try {
    p = path.split('/').slice(1).map(decodeURIComponent)
  } catch {
    return null
  }
  const [s, srv, d, db, kind, schema, name] = p
  if (s !== 's' || !srv || d !== 'd' || !db) return null
  if (kind === 'console') return { srv, db, console: true }
  if ((kind === 't' || kind === 'v') && schema && name) return { srv, db, table: { schema, name, kind: kind === 'v' ? 'view' : 'table' } }
  return null
}
