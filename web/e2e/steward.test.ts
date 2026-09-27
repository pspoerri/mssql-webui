// A data steward working with an existing database: find it, browse and
// search, fix rows, check definitions, move data in and out as CSV, run SQL.
// Run with `make e2e` (needs `make run-sqlserver`); see harness.ts.
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { after, before, test } from 'node:test'
import type { Dialog, Page } from 'playwright-core'
import { DB, start, type App } from './harness.ts'

let app: App
before(async () => { app = await start() })
after(async () => { await app?.stop() })

const table = (t: string, kind = 't') => `${app.base}/s/${encodeURIComponent(app.srv)}/d/${DB}/${kind}/dbo/${t}`
const cell = (p: Page, col: string) => p.getByRole('textbox', { name: col, exact: true })
const csv = (name: string, text: string) => ({ name, mimeType: 'text/csv', buffer: Buffer.from(text) })

// Opens the tree's import for the steward database and answers the dialogs.
async function importFile(p: Page, target: string, file: ReturnType<typeof csv>, append = false) {
  // One handler for both dialogs: listeners all see every dialog, and an
  // unanswered one is dismissed. The prompt names the table, then an append
  // is confirmed.
  const answer = (d: Dialog) => {
    if (d.type() === 'prompt') {
      if (!append) p.off('dialog', answer)
      return d.accept(target)
    }
    p.off('dialog', answer)
    assert.ok(append && /already exists/.test(d.message()), d.message())
    return d.accept()
  }
  p.on('dialog', answer)
  const chooser = p.waitForEvent('filechooser')
  await p.getByRole('button', { name: /Import CSV/ }).click()
  await (await chooser).setFiles(file)
}

async function openDB(p: Page) {
  await p.goto(app.base)
  await p.getByRole('button', { name: DB, exact: true }).click()
  await p.getByRole('button', { name: 'customer', exact: true }).waitFor()
}

test('finds the database and its tables in the tree', async () => {
  const p = await app.page()
  await openDB(p)
  for (const t of ['customer', 'order', 'order_line', 'event_log', 'reading']) await p.getByRole('button', { name: t, exact: true }).waitFor()
  await p.getByTitle('order_totals (view)').waitFor()
  await p.getByLabel('Show system databases').check()
  await p.getByRole('button', { name: 'master', exact: true }).waitFor()
  await p.close()
})

test('browses, searches, sorts, and a link reopens the same view', async () => {
  const p = await app.page()
  await p.goto(table('reading'))
  await assert.doesNotReject(p.getByText(/^\d+\+ rows$/).waitFor()) // more pages than one
  await p.getByLabel('Search').fill('sensor=sensor-3')
  await p.getByLabel('Search').press('Enter')
  await p.waitForURL(/q=sensor/)
  await p.waitForFunction(() => [...document.querySelectorAll<HTMLInputElement>('input[aria-label="sensor"]')].every((i) => i.value === 'sensor-3'))
  await p.getByRole('columnheader', { name: 'id' }).hover() // header icons show on hover
  await p.getByLabel('Sort by id').click() // ascending
  await p.getByLabel('Sort by id').click() // descending
  await p.waitForURL(/dir=desc/)
  const firstId = () => p.waitForFunction(() => document.querySelector<HTMLInputElement>('input[aria-label="id"]')?.value === '2495') // largest id with id % 7 = 3
  await firstId()
  await p.reload()
  assert.equal(await p.getByLabel('Search').inputValue(), 'sensor=sensor-3')
  await firstId()
  await p.close()
})

test('fixes data: edit, add and delete rows, saved in one go', async () => {
  const p = await app.page()
  await p.goto(table('customer') + '?q=name%3DAlice')
  await cell(p, 'name').first().fill('Alice Ng')
  await p.getByRole('button', { name: 'Add row' }).click()
  await cell(p, 'name').last().fill('Carol')
  await cell(p, 'email').last().fill('carol@example.com') // UQ_customer_email allows one NULL, Bob has it
  await cell(p, 'country').last().fill('FR')
  await p.getByRole('button', { name: /^Save 2 rows$/ }).click()
  await p.getByText('All changes saved').waitFor()
  assert.deepEqual(await app.rows("SELECT name FROM dbo.customer WHERE name IN ('Alice Ng', 'Carol') ORDER BY name"), [['Alice Ng'], ['Carol']])

  await p.goto(table('order_line'))
  await p.getByLabel('Delete row').first().check()
  await p.getByRole('button', { name: /^Save 1 row$/ }).click()
  await p.getByText('All changes saved').waitFor()
  assert.deepEqual(await app.rows('SELECT COUNT(*) FROM dbo.order_line'), [[2]])
  await p.close()
})

test('a value SQL Server rejects shows the error and writes nothing', async () => {
  const p = await app.page()
  await p.goto(table('reading') + '?q=id%3D1')
  await cell(p, 'value').first().fill('not a number')
  await p.getByRole('button', { name: /^Save 1 row$/ }).click()
  await p.locator('.error').filter({ hasText: /convert/i }).waitFor()
  assert.deepEqual(await app.rows('SELECT value FROM dbo.reading WHERE id = 1'), [[0.5]])
  await p.close()
})

test('clearing a required number is refused, not saved as 0', async () => {
  const p = await app.page()
  await p.goto(table('order') + '?q=order_no%3D2')
  await cell(p, 'net').first().fill('')
  await p.getByRole('button', { name: /^Save 1 row$/ }).click()
  await p.locator('.error').filter({ hasText: /NULL/ }).waitFor()
  assert.deepEqual(await app.rows('SELECT net FROM dbo.[order] WHERE order_no = 2'), [['250.50']])
  await p.close()
})

test('a table without a primary key is append-only', async () => {
  const p = await app.page()
  await p.goto(table('event_log'))
  await p.getByText('No primary key: append-only').waitFor()
  assert.equal(await p.getByLabel('Delete row').count(), 0)
  await p.close()
})

test('shows a table definition and a view definition', async () => {
  const p = await app.page()
  await p.goto(table('customer'))
  await p.getByRole('button', { name: 'Definition' }).click()
  const ddl = p.locator('pre.ddl')
  await ddl.filter({ hasText: 'CREATE TABLE [dbo].[customer]' }).waitFor()
  assert.match(await ddl.innerText(), /CONSTRAINT \[PK_customer\] PRIMARY KEY CLUSTERED \(\[id\]\)/)
  await p.getByRole('button', { name: 'Definition' }).click()
  await ddl.waitFor({ state: 'detached' })
  await p.goto(table('order_totals', 'v'))
  await p.getByRole('button', { name: 'Definition' }).click()
  await ddl.filter({ hasText: 'CREATE VIEW dbo.order_totals' }).waitFor()
  await p.close()
})

test('downloads a whole table as CSV', async () => {
  const p = await app.page()
  await p.goto(table('reading'))
  const download = p.waitForEvent('download')
  await p.getByRole('link', { name: 'Download CSV' }).click()
  const lines = readFileSync((await (await download).path())!, 'utf8').trim().split('\n')
  assert.equal(lines[0], 'id,sensor,value,taken')
  assert.equal(lines.length, 2501)
  await p.close()
})

test('imports a CSV into a new table, then appends to it', async () => {
  const p = await app.page()
  await openDB(p)
  const people = csv('people.csv', 'name;zip;joined\nAnn;08001;2024-01-02\nBob;8400;2024-02-03\nCem;;2024-03-04\n')
  await importFile(p, 'dbo.people', people)
  await p.getByText('Imported 3 rows').waitFor()
  await p.waitForURL(/\/t\/dbo\/people/) // the finished import opens its table
  await p.getByText('3 rows', { exact: true }).waitFor()
  // Leading zeros survive: zip is text, not a number.
  assert.deepEqual(await app.rows("SELECT TYPE_NAME(system_type_id) FROM sys.columns WHERE object_id = OBJECT_ID('dbo.people') ORDER BY column_id"), [['nvarchar'], ['nvarchar'], ['date']])
  assert.deepEqual(await app.rows("SELECT zip FROM dbo.people WHERE name = 'Ann'"), [['08001']])

  await importFile(p, 'dbo.people', people, true)
  await p.getByText('Appended 3 rows').waitFor()
  assert.deepEqual(await app.rows('SELECT COUNT(*) FROM dbo.people'), [[6]])
  await p.close()
})

test('a broken CSV fails visibly and leaves no table behind', async () => {
  const p = await app.page()
  await openDB(p)
  await importFile(p, 'dbo.broken', csv('broken.csv', 'a,b\n1,2\n3\n'))
  const job = p.getByRole('listitem').filter({ hasText: 'dbo.broken' })
  await job.getByText(/bad csv: record on line 3/).waitFor()
  await job.getByRole('button', { name: 'Dismiss' }).click()
  await job.waitFor({ state: 'detached' })
  assert.deepEqual(await app.rows("SELECT OBJECT_ID('dbo.broken')"), [[null]])
  await p.close()
})

test('a proxy refusing the upload is reported, not swallowed', async () => {
  const p = await app.page()
  await p.route('**/csv', (r) => r.fulfill({ status: 413, contentType: 'text/html', body: '<html>Request Entity Too Large</html>' }))
  await openDB(p)
  await importFile(p, 'dbo.too_big', csv('big.csv', 'a\n1\n'))
  await p.locator('nav .error').filter({ hasText: 'HTTP 413' }).waitFor()
  await p.close()
})

test('says when a paused database is resuming, then carries on', async () => {
  const p = await app.page()
  let refused = 0
  await p.route(`**/d/${DB}/tables`, (r) => refused++ < 2
    ? r.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: `database "${DB}" is paused and resuming; this can take a minute`, resuming: true }) })
    : r.continue())
  await p.goto(app.base)
  await p.getByRole('button', { name: DB, exact: true }).click()
  await p.getByRole('status').filter({ hasText: 'is paused and resuming' }).waitFor()
  await p.getByRole('button', { name: 'customer', exact: true }).waitFor({ timeout: 30_000 })
  await p.getByRole('status').filter({ hasText: 'resuming' }).waitFor({ state: 'detached' })
  assert.equal(refused, 3)
  await p.close()
})

test('the SQL console shows every result, message and error, and downloads a result', async () => {
  const p = await app.page()
  await p.goto(`${app.base}/s/${encodeURIComponent(app.srv)}/d/${DB}/console`)
  const run = async (sql: string) => {
    await p.getByLabel('SQL').fill(sql)
    await p.getByRole('button', { name: 'Run' }).click()
    await p.getByRole('button', { name: 'Run' }).waitFor() // not "Running…"
  }
  await run("-- how many?\nPRINT 'counting';\nSELECT sensor, COUNT(*) AS n FROM dbo.reading GROUP BY sensor ORDER BY sensor;\nUPDATE dbo.reading SET value = value WHERE id <= 3;\nSELECT 1/0 AS boom")
  await p.getByText('counting', { exact: true }).waitFor()
  await p.getByText('7 rows', { exact: true }).waitFor()
  await p.getByText('3 rows affected').waitFor()
  await p.locator('.error').filter({ hasText: 'Divide by zero' }).waitFor()

  // This batch changed data, so downloading (which runs it again) asks first.
  const asked = new Promise<string>((resolve) => p.once('dialog', (d) => { resolve(d.message()); d.dismiss() }))
  await p.getByRole('button', { name: 'Download CSV' }).first().click()
  assert.match(await asked, /runs the whole batch again/)

  await run('SELECT id FROM dbo.reading ORDER BY id')
  await p.getByText(/First 1000 rows shown/).waitFor()
  const download = p.waitForEvent('download')
  await p.getByRole('button', { name: 'Download CSV' }).click()
  const lines = readFileSync((await (await download).path())!, 'utf8').trim().split('\n')
  assert.equal(lines.length, 2501) // all rows, not the 1000 shown

  await run('BEGIN TRAN; DELETE FROM dbo.reading')
  await p.locator('.error').filter({ hasText: 'left a transaction open' }).waitFor()
  assert.deepEqual(await app.rows('SELECT COUNT(*) FROM dbo.reading'), [[2500]])
  await p.close()
})
