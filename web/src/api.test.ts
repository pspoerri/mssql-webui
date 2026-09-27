// node --test src/api.test.ts
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { api, onResuming, retry, type HttpError } from './api.ts'

retry.ms = 1

const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status })

test('api retries while the database resumes and reports the wait', async () => {
  const answers = [json(503, { error: 'db "x" is resuming', resuming: true }), json(503, { error: 'db "x" is resuming', resuming: true }), json(200, { ok: 1 })]
  globalThis.fetch = async () => answers.shift()!
  const seen: string[] = []
  const off = onResuming((m) => seen.push(m))
  assert.deepEqual(await api('/x'), { ok: 1 })
  off()
  assert.deepEqual(seen, ['db "x" is resuming', ''])
  assert.equal(answers.length, 0)
})

test('a plain 503 and proxy errors are not retried and still have a message', async () => {
  for (const [res, want] of [
    [json(503, { error: 'busy' }), 'busy'],
    [new Response('<html>Request Entity Too Large</html>', { status: 413 }), 'HTTP 413'],
    [new Response('', { status: 504 }), 'HTTP 504'],
  ] as const) {
    let calls = 0
    globalThis.fetch = async () => { calls++; return res.clone() }
    await assert.rejects(api('/x'), (e: HttpError) => e.message.includes(want) && e.status === res.status)
    assert.equal(calls, 1)
  }
})
