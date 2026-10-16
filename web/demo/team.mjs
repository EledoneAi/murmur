// A made-up team for the pages' demo mode (pnpm demo): the console demo's team, as the hub would send it. Times are
// relative to now, so the clocks run.
const min = 60000

export function me() {
  return { handle: 'alice', name: 'Alice Martin', email: 'alice@acme.dev', admin: true, colour: 0, version: 'demo' }
}

export function home() {
  const now = Date.now()
  return {
    brew: 'brew install eledoneai/tap/murmur',
    join: 'murmur join https://murmur.acme.dev',
    install: 'curl -fsSL https://murmur.acme.dev/install.sh | sh',
    devices: [
      { id: 'd1', host: 'alice-mbp', online: true, created: new Date(now - 40 * 24 * 60 * min).toISOString(), last_seen: new Date(now).toISOString() },
      { id: 'd2', host: 'build-box', online: false, created: new Date(now - 9 * 24 * 60 * min).toISOString(), last_seen: new Date(now - 26 * 60 * min).toISOString() },
    ],
  }
}

export function team(step = 0) {
  const now = Date.now()
  const ago = m => now - m * min
  const rot = (s, i) => s.slice(i % s.length) + s.slice(0, i % s.length)
  const people = [
    { handle: 'alice', name: 'Alice Martin', online: true, colour: 0, agents: [
      { name: 'api-1a', repo: 'acme/api', branch: 'feat/billing-ocr', base: 'feat/billing-v2', title: 'Read invoices with OCR', status: 'waiting',
        needs: 'permission', since: ago(3.2), doing: 'Running go test ./ocr/...', kinds: 'rreeeererxx', added: 51, deleted: 4, changed: 3, context: 31 },
      { name: 'api-9b', repo: 'acme/api', branch: 'feat/billing-v2', title: 'Rerun billing on staging', pr: 1822,
        pr_url: 'https://github.com/acme/api/pull/1822', status: 'busy', since: ago(12), doing: 'Running go test ./billing/ -run Rerun',
        kinds: rot('rrrxeerxxxexrrmexxxerxxxmxrexx', step), added: 214, deleted: 38, changed: 6, context: 78 },
      { name: 'web-4c', repo: 'acme/web', branch: 'feat/search-ui', title: 'Search filters and empty states', status: 'busy', since: ago(25),
        doing: 'Editing src/search/Filters.tsx', kinds: rot('eerreeeereeerreeeereeeerreeere', step + 3), added: 96, deleted: 12, changed: 4, context: 44,
        pr: 1840, pr_url: 'https://github.com/acme/web/pull/1840', app: 'vscode' },
      { name: 'notes', repo: 'notes', title: 'Draft the weekly plan', status: 'busy', since: ago(5), doing: 'Fetching the release calendar',
        kinds: rot('rrwwrrrerrewrrre', step + 1) },
    ] },
    { handle: 'carol', name: 'Carol Diaz', online: true, colour: 2, agents: [
      { name: 'billing-2', repo: 'acme/api', branch: 'feat/billing-certifier', title: 'Split the certifier', status: 'waiting', needs: 'input',
        since: ago(8.7), kinds: 'eeereerrem', context: 52 },
      { name: 'web-2', repo: 'acme/web', branch: 'feat/search-api', title: 'Search endpoint pagination', status: 'busy', since: ago(15),
        kinds: rot('rreeexxeeerreexxe', step + 2), doing: 'Editing api/search.ts', added: 33, deleted: 9, changed: 2, context: 27 },
    ] },
    { handle: 'bob', name: 'Bob Okafor', online: true, colour: 1, agents: [
      { name: 'main-1', repo: 'acme/api', branch: 'main', title: 'Review the billing config', status: 'busy', since: ago(4),
        doing: 'Reading billing/config.go', kinds: rot('rrwrrrrerrrwrrrrrr', step), context: 12 },
    ] },
    { handle: 'dave', name: 'Dave Lin', online: true, colour: 3, agents: [
      { name: 'ingest-1', repo: 'acme/data', branch: 'queue-v2', title: 'Move the rerun queue', status: 'busy', since: ago(9),
        doing: 'Running make migrate', kinds: 'xxxxrxxxxxxx', loop: true },
      { name: 'infra-1', repo: 'acme/infra', branch: 'pilot/terraform-6', title: 'Pilot Terraform 6', status: 'idle', since: ago(60) },
    ] },
    { handle: 'erin', name: 'Erin Novak', online: false, colour: 4, agents: [] },
  ]
  return {
    me: 'alice', people,
    prs: [
      { name: 'acme/api#1822', number: 1822, url: 'https://github.com/acme/api/pull/1822', title: 'Rerun billing on staging', person: 'alice',
        agent: 'api-9b', repo: 'acme/api', branch: 'feat/billing-v2', since: ago(190), review: 'Review acme/api#1822 with gh pr diff, and tell alice what you find.' },
      { name: 'acme/web#1840', number: 1840, url: 'https://github.com/acme/web/pull/1840', title: 'Search filters and empty states', person: 'alice',
        agent: 'web-4c', repo: 'acme/web', branch: 'feat/search-ui', since: ago(25), review: 'Review acme/web#1840 with gh pr diff, and tell alice what you find.' },
    ],
    mine: ['api-1a', 'api-9b', 'web-4c', 'notes'],
    repos: ['acme/api', 'acme/data', 'acme/infra', 'acme/web'],
    news: [{ from: 'bob', repo: 'acme/api', text: 'main is frozen until 15:00 for the billing release.', at: ago(30), until: now + 23 * 60 * min }],
  }
}

const iso = m => new Date(Date.now() - m * min).toISOString()

export function adminOverview() {
  return { version: '1.0.0', latest: { version: '1.1.0', url: 'https://github.com/EledoneAi/murmur/releases/tag/v1.1.0', checked: iso(40) },
    newer: true, team: 5, removed: 1, admins: ['alice', 'bob'], signed_in: 7, online: 5, seen_this_week: 7,
    versions: [{ version: '1.0.0', count: 6, behind: '' }, { version: '0.9.4', count: 1, behind: 'older than 1.0.0, the latest' }],
    failing: [adminLaptops().laptops[3]], reported: true, check_hours: 24 }
}

export function adminPeople() {
  const p = (handle, name, colour, x = {}) => ({ handle, name, email: `${handle}@acme.dev`, admin: false, in_admins_env: false, removed: false,
    laptops: 1, online: true, last_seen: iso(1), colour, you: false, ...x })
  return {
    people: [p('alice', 'Alice Martin', 0, { admin: true, you: true, laptops: 2 }), p('bob', 'Bob Okafor', 1, { admin: true, in_admins_env: true }),
      p('carol', 'Carol Diaz', 2), p('dave', 'Dave Lin', 3, { laptops: 2 }), p('erin', 'Erin Novak', 4, { online: false, last_seen: iso(60 * 26) }),
      p('frank', 'Frank Ito', 5, { removed: true, laptops: 0, online: false, last_seen: iso(60 * 24 * 12) })],
    only_admin: false, provider: 'Google',
    join: { allowed_domains: 'acme.dev', allowed_emails: 'contractor@studio.example', allowed_orgs: '' },
  }
}

export function adminLaptops() {
  const l = (id, handle, host, x = {}) => ({ id, handle, email: `${handle}@acme.dev`, host, version: '1.0.0', behind: '', created: iso(60 * 24 * 30),
    last_seen: iso(1), online: true, revoked: false, expired: false, check: { ok: true, at: iso(60 * 3), failing: [] }, ...x })
  return { laptops: [l('d1', 'alice', 'alice-mbp'), l('d2', 'alice', 'build-box', { online: false, last_seen: iso(60 * 26) }),
    l('d3', 'bob', 'bob-mbp'), l('d4', 'dave', 'dave-x1', { version: '0.9.4', behind: 'older than 1.0.0, the latest',
      check: { ok: false, at: iso(60 * 5), failing: [{ name: 'hooks', detail: 'no murmur hook in settings.json' }] } }),
    l('d5', 'carol', 'carol-mbp'), l('d6', 'dave', 'dave-desk'), l('d7', 'frank', 'frank-mbp', { revoked: true, online: false, last_seen: iso(60 * 24 * 12) })] }
}

export function adminSettings() {
  return { settings: { token_days: 90, sends_per_minute: 20, loop_max: 12, loop_minutes: 10, max_agents: 64, max_laptops: 10,
    downloads_per_hour: 120, ignored_paths: 'traces/**\n*.lock', update_policy: 'auto', min_version: '1.0.0', release_check: true,
    agreements: 'Before you change a file another agent changes, message that agent and agree on who does what.\nAsk a person only for decisions, with options.',
    announce_hours: 24, chat_webhook: 'https://chat.googleapis.com/v1/spaces/AAAA/messages', chat_kind: 'google-chat',
    wait_ping_minutes: 15, work_start: 9, work_end: 19, time_zone: 'Europe/Paris', weekdays: true, weekly_report: true, check_hours: 24 },
    github_secret_set: true, github_token_set: false, updated: iso(60 * 5), updated_by: 'alice' }
}

export function adminEvents() {
  const e = (m, kind, actor, subject = '', detail = '') => ({ at: iso(m), kind, actor, subject, detail })
  return { events: [e(3, 'laptop signed in', 'carol', 'carol-mbp'), e(42, 'announcement', 'bob', 'acme/api', 'to 6 agents'),
    e(60 * 5, 'settings changed', 'alice', '', 'agreements, wait_ping_minutes'), e(60 * 26, 'signed in', 'erin'),
    e(60 * 24 * 12, 'person removed', 'alice', 'frank'), e(60 * 24 * 12, 'laptop revoked', 'alice', 'd7', 'frank-mbp')],
    skip: 0, newer: null, older: null }
}
