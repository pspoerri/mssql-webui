// node --test src/search.test.ts
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { formatTerms, parseTerms } from './search.ts'

test('parseTerms mirrors the backend tokenizer', () => {
  assert.deepEqual(parseTerms(`User 1001 name='User 1002' city^Z "a=b" id= x~"it's"`), [
    { val: 'User' },
    { val: '1001' },
    { col: 'name', op: '=', val: 'User 1002' },
    { col: 'city', op: '^', val: 'Z' },
    { val: 'a=b' },
    { col: 'id', op: '=', val: '' },
    { col: 'x', op: '~', val: "it's" },
  ])
})

test('formatTerms round-trips', () => {
  const q = `User name="User 1002" city^Z "a=b" id= x~'it"s'`
  assert.equal(formatTerms(parseTerms(q)), q)
})
