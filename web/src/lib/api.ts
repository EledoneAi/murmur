// What the hub answers, as it sends it. Everything that came from a laptop is text and only ever drawn as text.

export type Agent = {
  name: string
  repo?: string
  branch?: string
  title?: string
  pr?: number
  pr_url?: string
  app?: string // vscode, desktop or sdk; empty in a terminal
  status: 'busy' | 'idle' | 'waiting' | 'shell' | string
  needs?: string // permission or input, when it waits for its person
  since?: number // unix ms of the last status change
  doing?: string
  kinds?: string // the kinds of its last tool calls, oldest first: r read, e edit, x run, w web, s subagent, m message
  loop?: boolean
  context?: number // percent of its context window
  added?: number
  deleted?: number
  changed?: number
  base?: string
  detached?: string
  op?: string
  dir?: string
  also?: string[]
}

export type Person = { handle: string; name?: string; online: boolean; colour: number; agents?: Agent[] }

export type PullRequest = {
  name: string; number?: number; url?: string; title?: string; person: string; agent: string
  repo?: string; branch?: string; since?: number; review: string
}

export type News = { from: string; repo: string; text: string; at: number; until: number }

export type Team = {
  me: string
  people: Person[] // people with a waiting agent first
  prs: PullRequest[]
  mine: string[] // your running agents, which you can ask to review
  repos: string[]
  news: News[]
}

export type Me = { handle: string; name?: string; email: string; admin: boolean; colour: number; version: string }

// SignIn is what a signed-out visitor needs to sign in.
export type SignIn = { provider: string; dev: boolean }

export type Device = { id: string; host: string; online: boolean; created: string; last_seen?: string }

export type Home = { install: string; devices: Device[] }

export class HubError extends Error {
  constructor(message: string, readonly status: number, readonly field?: string) { super(message) }
}

async function call<T>(path: string, init?: RequestInit): Promise<T> {
  let res: Response
  try {
    res = await fetch(path, { credentials: 'same-origin', headers: { Accept: 'application/json', 'Content-Type': 'application/json' }, ...init })
  } catch {
    throw new HubError('The hub did not answer. Check your connection and try again.', 0)
  }
  const body = await res.json().catch(() => ({}))
  if (!res.ok) throw new HubError(body.error || body.result || `The hub answered ${res.status}.`, res.status, body.field)
  return body as T
}

export const get = <T>(path: string) => call<T>(path)
export const post = <T>(path: string, data: unknown) => call<T>(path, { method: 'POST', body: JSON.stringify(data) })

// me is who is signed in, or how to sign in.
export async function me(): Promise<{ me: Me } | { signIn: SignIn }> {
  const res = await fetch('/api/me', { credentials: 'same-origin', headers: { Accept: 'application/json' } })
  const body = await res.json().catch(() => ({}))
  if (res.status === 401) return { signIn: body as SignIn }
  if (!res.ok) throw new HubError(body.error || `The hub answered ${res.status}.`, res.status)
  return { me: body as Me }
}

// signIn sends a signed-out visitor to sign in, and back here afterwards.
export function signIn() {
  location.href = '/login?next=' + encodeURIComponent(location.pathname + location.search)
}
