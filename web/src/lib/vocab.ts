// The console's vocabulary, for the web: a small set of signs, each with one meaning. Every state has a shape, a
// word and a colour, so a page reads the same without colour.
import type { Agent } from './api'

export type State = 'working' | 'needs' | 'waits' | 'idle' | 'shell' | 'gone'

export function stateOf(a: Agent, mine: boolean, online = true): State {
  if (!online) return 'gone'
  if (a.needs || a.status === 'waiting') return mine ? 'needs' : 'waits'
  if (a.status === 'busy') return 'working'
  if (a.status === 'shell') return 'shell'
  return 'idle'
}

export const lamp: Record<State, string> = { working: '●', needs: '◆', waits: '◇', idle: '○', shell: '◌', gone: '⊘' }

export const word: Record<State, string> = {
  working: 'working', needs: 'needs you', waits: 'waits on its owner', idle: 'idle', shell: 'shell', gone: 'gone',
}

export const waiting = (s: State) => s === 'needs' || s === 'waits'

// why one of your agents waits, as Claude Code reports it
export function reason(a: Agent): string {
  if (a.needs === 'permission') return a.doing ? 'approve ' + a.doing[0].toLowerCase() + a.doing.slice(1) : 'approve a tool call'
  return 'waits for your answer'
}

// texture: the kind of each of the last n tool calls, newest on the right
const kindSign: Record<string, string> = { r: '·', e: '▪', x: '›', w: '∿', s: '⋔', m: '⇢' }
export function texture(kinds = '', n = 12): { sign: string; kind: string }[] {
  const last = [...kinds].slice(-n)
  return [...Array(n - last.length).fill({ sign: ' ', kind: '' }), ...last.map(k => ({ sign: kindSign[k] || ' ', kind: k }))]
}

export function shares(kinds = ''): string {
  const all = [...kinds]
  if (all.length === 0) return ''
  const names: [string, string][] = [['r', 'read'], ['e', 'edit'], ['x', 'run'], ['w', 'web'], ['s', 'subagents'], ['m', 'talk']]
  return names.map(([k, w]) => [w, all.filter(c => c === k).length] as const)
    .filter(([, c]) => c > 0).map(([w, c]) => `${w} ${Math.floor((c * 100) / all.length)}%`).join(' · ')
}

// the fuse burns half a cell at a time while an agent waits, full at ten minutes
export function fuse(waitedMs: number, width = 8): { burnt: string; rest: string } {
  const halves = Math.min(2 * width, Math.round((waitedMs / 600000) * 2 * width))
  const full = Math.floor(halves / 2), half = halves % 2
  return { burnt: '━'.repeat(full) + (half ? '╸' : ''), rest: '─'.repeat(width - full - half) }
}

export function clock(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000)), h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), r = s % 60
  return h > 0 ? `${h}:${String(m).padStart(2, '0')}:${String(r).padStart(2, '0')}` : `${m}:${String(r).padStart(2, '0')}`
}

export function ago(ms: number): string {
  const m = Math.floor(ms / 60000)
  if (m < 1) return 'now'
  if (m < 60) return `${m}m`
  if (m < 48 * 60) return `${Math.floor(m / 60)}h`
  return `${Math.floor(m / 1440)}d`
}

export function agoDate(iso?: string): string {
  if (!iso) return 'never'
  const t = Date.parse(iso)
  return Number.isNaN(t) ? 'never' : ago(Date.now() - t) === 'now' ? 'just now' : ago(Date.now() - t) + ' ago'
}

// place: where an agent works, beyond its repo
export function place(a: Agent, checkout = 0): { head: string; tail: string; outside: boolean } {
  if (!a.branch && !a.detached) return { head: '⌂ ' + (a.repo || ''), tail: '', outside: true }
  let tail = ''
  if (checkout > 1) tail += String(checkout).split('').map(d => '⁰¹²³⁴⁵⁶⁷⁸⁹'[+d]).join('')
  if (a.op) tail += ' ' + a.op
  else if (a.base && a.base !== 'main' && a.base !== 'master') tail += ' ⟵ ' + afterShared(a.branch || '', a.base)
  return { head: a.detached ? '∅ ' + a.detached : a.branch || '', tail, outside: false }
}

function afterShared(branch: string, base: string): string {
  let i = 0
  while (i < branch.length && i < base.length && branch[i] === base[i]) i++
  const cut = Math.max(base.lastIndexOf('-', i - 1), base.lastIndexOf('/', i - 1), base.lastIndexOf('_', i - 1))
  return base.slice(cut + 1) || base.split('/').pop() || base
}

// initials: a person's letter, two when two people share it
export function initials(handles: string[]): Record<string, string> {
  const first = (h: string) => (h[0] || '?').toUpperCase()
  const count: Record<string, number> = {}
  for (const h of handles) count[first(h)] = (count[first(h)] || 0) + 1
  return Object.fromEntries(handles.map(h => [h, count[first(h)] > 1 && h.length > 1 ? first(h) + h[1] : first(h)]))
}

export const plural = (n: number, one: string, many: string) => (n === 1 ? one : many)

// A person's colour, the same on every screen: on their initial only. The classes are written out in full so
// Tailwind finds them.
const chips = ['bg-p0/12 text-p0', 'bg-p1/12 text-p1', 'bg-p2/12 text-p2', 'bg-p3/12 text-p3',
  'bg-p4/12 text-p4', 'bg-p5/12 text-p5', 'bg-p6/12 text-p6', 'bg-p7/12 text-p7']
export const chip = (colour: number) => chips[((colour % 8) + 8) % 8]

// the texture's signs, each in its kind's colour
const kindClass: Record<string, string> = { r: 'text-muted', e: 'text-strong', x: 'text-link', w: 'text-link', s: 'text-lilac', m: 'text-strong' }
export const kindColour = (k: string) => kindClass[k] || ''
