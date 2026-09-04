export type DbInfo = { name: string; access: boolean }
export type ServerInfo = { name: string; databases: DbInfo[]; error?: string }
export type TableInfo = { schema: string; name: string; kind: 'table' | 'view' }
export type ColumnInfo = { name: string; type: string; nullable: boolean; identity: boolean; readonly: boolean }
export type TableMeta = { columns: ColumnInfo[]; pk: string[] }
export type Cell = string | number | boolean | null
export type RowsPage = { columns: string[]; rows: Cell[][]; hasMore: boolean }
export type QueryResult = { columns: string[]; rows: Cell[][] } | { rowsAffected: number }
export type Selection = { srv: string; db: string; table?: TableInfo; console?: boolean }

export const enc = encodeURIComponent

// GET when body is undefined, POST JSON otherwise. A 401 sends the user to login.
export async function api<T>(path: string, body?: unknown): Promise<T> {
  const res = await fetch(
    path,
    body === undefined
      ? undefined
      : { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) },
  )
  if (res.status === 401) {
    window.location.href = '/auth/login'
    throw new Error('not logged in')
  }
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(data.error ?? res.statusText)
  return data as T
}

export const cellToString = (v: Cell): string | null => (v === null ? null : String(v))
