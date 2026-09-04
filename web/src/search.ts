// Search terms, mirroring the backend's searchTerms: whitespace-separated, 'quotes'
// or "quotes" keep spaces together, col=value / col^value / col~value filter one column.
export type Op = '=' | '^' | '~'
export type Term = { col?: string; op?: Op; val: string }

export function parseTerms(q: string): Term[] {
  const out: Term[] = []
  let cur: Term = { val: '' }
  let buf = ''
  let quote = ''
  let has = false
  const flush = () => {
    if (has) out.push({ ...cur, val: buf })
    cur = { val: '' }
    buf = ''
    has = false
  }
  for (const r of q) {
    if (quote) {
      if (r === quote) quote = ''
      else buf += r
    } else if (r === "'" || r === '"') {
      quote = r
      has = true
    } else if (/\s/.test(r)) {
      flush()
    } else if ((r === '=' || r === '^' || r === '~') && !cur.op && buf) {
      cur.col = buf
      cur.op = r
      buf = ''
    } else {
      buf += r
      has = true
    }
  }
  flush()
  return out
}

const quoted = (v: string) => (/[\s'"=^~]/.test(v) ? (v.includes('"') ? `'${v}'` : `"${v}"`) : v)
export const formatTerms = (terms: Term[]) => terms.map((t) => (t.col ? t.col + t.op : '') + quoted(t.val)).join(' ')
