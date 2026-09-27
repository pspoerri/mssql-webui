// End-to-end harness: starts the built mssql-webui binary in dev mode on a
// free port against a real SQL Server, rebuilds the steward database through
// the app's own API, and drives it with a real browser (playwright-core).
// `make e2e` builds the binary first; see README "End-to-end tests".
import { spawn } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { createServer } from 'node:net'
import { chromium, type Browser, type Page } from 'playwright-core'

const root = new URL('../../', import.meta.url).pathname
export const DB = 'steward_e2e'

// The database a data steward finds: docs/ddl-cases.sql (customers, orders,
// order lines, a heap, odd types, a view) plus a table too long for one page.
const seed = readFileSync(root + 'docs/ddl-cases.sql', 'utf8') + `
CREATE TABLE dbo.reading (id int NOT NULL PRIMARY KEY, sensor nvarchar(20) NOT NULL, value float NULL, taken datetime2(0) NOT NULL);
WITH n AS (SELECT TOP 2500 ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS i FROM sys.all_objects a CROSS JOIN sys.all_objects b)
INSERT dbo.reading SELECT i, CONCAT('sensor-', i % 7), i * 0.5, DATEADD(minute, i, '2026-01-01') FROM n;`

export type App = {
  base: string // http://127.0.0.1:port
  srv: string // the server's name in the tree and in URLs
  browser: Browser
  page: () => Promise<Page>
  rows: (sql: string) => Promise<unknown[][]> // first result set of a batch run in the steward database
  stop: () => Promise<void>
}

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const s = createServer().listen(0, '127.0.0.1', () => {
      const port = (s.address() as { port: number }).port
      s.close(() => resolve(port))
    }).on('error', reject)
  })
}

async function call(url: string, body?: unknown): Promise<any> {
  const res = await fetch(url, body === undefined ? undefined : { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(`${url}: ${res.status} ${data.error ?? ''}`)
  return data
}

export async function start(): Promise<App> {
  const port = await freePort()
  const base = `http://127.0.0.1:${port}`
  const proc = spawn(process.env.E2E_BIN ?? root + 'mssql-webui', [], {
    env: {
      ...process.env, DEV_USER: 'steward', LISTEN_ADDR: `127.0.0.1:${port}`, TENANT_ID: '', CLIENT_SECRET: '',
      SQL_SERVERS: process.env.E2E_SQL_SERVERS || process.env.SQL_SERVERS || 'sqlserver://sa:Dev_Passw0rd@localhost:1433?trustservercertificate=true',
    },
    stdio: ['ignore', 'ignore', 'pipe'],
  })
  let stderr = ''
  proc.stderr.on('data', (d) => { stderr += d })
  const stop = async () => { proc.kill() }
  try {
    let servers: { name: string }[] | undefined
    for (let i = 0; !servers; i++) {
      servers = await call(`${base}/api/servers`).catch((e) => { if (i > 100 || proc.exitCode !== null) throw e })
      if (!servers) await new Promise((r) => setTimeout(r, 100))
    }
    const srv = servers[0].name
    const api = `${base}/api/s/${encodeURIComponent(srv)}`
    await call(`${api}/d/master/query`, { sql: `IF DB_ID('${DB}') IS NOT NULL BEGIN ALTER DATABASE [${DB}] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE [${DB}] END` })
    await call(`${api}/databases`, { name: DB })
    await call(`${api}/d/${DB}/query`, { sql: seed })
    const browser = await chromium.launch({ executablePath: process.env.E2E_CHROMIUM || undefined })
    return {
      base, srv, browser,
      page: async () => {
        const p = await browser.newPage({ viewport: { width: 1280, height: 800 }, acceptDownloads: true })
        p.setDefaultTimeout(15_000)
        return p
      },
      rows: async (sql) => (await call(`${api}/d/${DB}/query`, { sql })).results.find((r: { columns?: unknown }) => r.columns).rows,
      stop: async () => { await browser.close(); await stop() },
    }
  } catch (e) {
    await stop()
    throw new Error(`${(e as Error).message}\n--- server stderr ---\n${stderr}`)
  }
}
