// Single-path 16px line icons; stroke follows the text colour.
const paths = {
  chevron: 'M6 4l4 4-4 4',
  server: 'M3 3h10a1 1 0 0 1 1 1v2a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1zM3 9h10a1 1 0 0 1 1 1v2a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1v-2a1 1 0 0 1 1-1zM4.5 5h.01M4.5 11h.01',
  database: 'M2.5 4a5.5 2 0 1 0 11 0a5.5 2 0 1 0-11 0v8c0 1.1 2.5 2 5.5 2s5.5-.9 5.5-2V4M2.5 8c0 1.1 2.5 2 5.5 2s5.5-.9 5.5-2',
  table: 'M3 3h10a1 1 0 0 1 1 1v8a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1zM2 6.5h12M6.5 6.5V13',
  view: 'M1.5 8s2.5-4 6.5-4 6.5 4 6.5 4-2.5 4-6.5 4S1.5 8 1.5 8zM10 8a2 2 0 1 1-4 0 2 2 0 0 1 4 0z',
  console: 'M3 4l4 4-4 4M8 12h5',
  refresh: 'M13.5 8A5.5 5.5 0 1 1 11 3.6M13.5 3v3.5H10',
  search: 'M7 12A5 5 0 1 0 7 2a5 5 0 0 0 0 10zM14 14l-3.5-3.5',
  sortNone: 'M5 3v10M2.5 10.5L5 13l2.5-2.5M11 13V3M8.5 5.5L11 3l2.5 2.5',
  sortAsc: 'M8 13V3M4 7l4-4 4 4',
  sortDesc: 'M8 3v10M4 9l4 4 4-4',
  filter: 'M2 3h12l-4.5 5.5V13l-3-1.5V8.5z',
  pin: 'M8 2a3 3 0 0 1 3 3v3l1.5 2h-9L5 8V5a3 3 0 0 1 3-3zM8 10v4',
  sidebar: 'M3 3h10a1 1 0 0 1 1 1v8a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1zM6 3v10',
}
type Props = { name: keyof typeof paths; size?: number; filled?: boolean; className?: string }

export const Icon = ({ name, size = 14, filled, className }: Props) => (
  <svg width={size} height={size} viewBox="0 0 16 16" className={className} aria-hidden="true"
    fill={filled ? 'currentColor' : 'none'} stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
    <path d={paths[name]} />
  </svg>
)
