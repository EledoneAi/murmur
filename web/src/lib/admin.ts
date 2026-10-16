// The admin panel's API: what the hub's /api/admin endpoints take and answer, a typed fetch for each, and the words
// the old admin pages said, so the pages can draw their forms and ask before an action.
//
// Only signed-in admins get answers. Anyone else gets an AdminError with status 401 (signed out: send them to sign
// in) or 403 (not an admin). Every post is JSON. Times are RFC 3339 strings in UTC, like "2026-10-16T09:12:00Z".

// ---------------------------------------------------------------- errors

/** What the hub answers when it refuses or fails. */
export interface Problem {
  /** A sentence for a person, ready to show. */
  error: string;
  /** The field at fault, by its name in the request, when one is. */
  field?: string;
}

/** Thrown by every call below when the hub refuses or fails: the message is the hub's sentence. */
export class AdminError extends Error {
  /** The HTTP status; 0 when the hub couldn't be reached. */
  readonly status: number;
  /** The field at fault, when one is. */
  readonly field?: string;

  constructor(message: string, status: number, field?: string) {
    super(message);
    this.name = 'AdminError';
    this.status = status;
    this.field = field;
  }
}

/** A time that may never have come: RFC 3339 in UTC, or null. */
export type Stamp = string | null;

// ---------------------------------------------------------------- overview: GET /api/admin

export interface Release {
  version: string;
  /** Where to read what's new in it. */
  url: string;
  /** When the release check last looked. */
  checked: string;
}

export interface VersionCount {
  /** Empty for laptops that never said which they run. */
  version: string;
  /** Signed-in laptops on it. */
  count: number;
  /** Why it needs a look, like "older than 1.4.0, the oldest accepted"; empty when it doesn't. */
  behind: string;
}

export interface Overview {
  /** The murmur this hub runs. */
  version: string;
  /** What the release check found last; null until it ran. */
  latest: Release | null;
  /** latest is newer than this hub: upgrading the hub moves the team to it. */
  newer: boolean;
  /** People on the team, removed people aside. */
  team: number;
  removed: number;
  /** The admins' handles, sorted. */
  admins: string[];
  /** Laptops neither revoked nor expired. */
  signed_in: number;
  /** Signed-in laptops connected now. */
  online: number;
  /** Signed-in laptops online now or seen in the last 7 days. */
  seen_this_week: number;
  /** Signed-in laptops by version, newest first, versions that aren't numbers last. */
  versions: VersionCount[];
  /** Signed-in laptops whose last self-check failed. */
  failing: Laptop[];
  /** Whether any signed-in laptop has sent a self-check. */
  reported: boolean;
  /** Hours between laptop self-checks; 0 when checks are off in the settings. */
  check_hours: number;
}

// ---------------------------------------------------------------- laptops: GET /api/admin/laptops

export interface FailedCheck {
  name: string;
  /** Empty when the laptop gave none. */
  detail: string;
}

/** A laptop's last self-check. */
export interface Check {
  /** Whether everything passed. */
  ok: boolean;
  /** When it ran. */
  at: string;
  /** The checks that failed. Empty while ok is false means "not OK, without details". */
  failing: FailedCheck[];
}

export interface Laptop {
  id: string;
  /** Its person's handle, or their email when the hub no longer knows them. */
  handle: string;
  email: string;
  /** The laptop's name; empty when unnamed. */
  host: string;
  /** The murmur it runs; empty until it connects. */
  version: string;
  /** Why its version needs a look, like "older than this hub's 1.2.0"; empty when it doesn't. */
  behind: string;
  /** When it was added: signed in with murmur init. */
  created: string;
  /** null when it never connected. */
  last_seen: Stamp;
  online: boolean;
  /** Revoked: it can't connect again. */
  revoked: boolean;
  /** Signed in longer ago than the team allows (token_days): its person signs it in again. */
  expired: boolean;
  /** Its last self-check; null until it sends one. */
  check: Check | null;
}

export interface Laptops {
  /** Revoked ones last, then by handle, the most recently seen first. */
  laptops: Laptop[];
}

/** What POST /api/admin/laptops/revoke answers. */
export interface Revoked {
  ok: true;
  id: string;
}

// ---------------------------------------------------------------- people: GET /api/admin/people

export interface Person {
  handle: string;
  /** Empty when the identity provider gave none. */
  name: string;
  email: string;
  admin: boolean;
  /** Listed in ADMINS in the hub's configuration: made an admin again at each sign-in, whatever the panel did. */
  in_admins_env: boolean;
  /** Removed from the team: every laptop revoked, refused at sign-in until let back in. Says nothing of admin. */
  removed: boolean;
  /** Laptops signed in: neither revoked nor expired. */
  laptops: number;
  /** One of those laptops is connected now. */
  online: boolean;
  /** When any of their laptops was last seen; null when none ever connected. */
  last_seen: Stamp;
  /** Their colour, 0 to 7, the same on every screen. */
  colour: number;
  /** This is the admin looking: they can't remove themselves. */
  you: boolean;
}

/** Who may join, each list one entry per line. With nothing listed, only admins can sign in. */
export interface Join {
  /** Email domains, like example.com. */
  allowed_domains: string;
  /** Email addresses, for people outside those domains. */
  allowed_emails: string;
  /** GitHub organisations, by their name in GitHub addresses. Only for sign-in with GitHub. */
  allowed_orgs: string;
}

export interface People {
  /** Removed people last, then by handle. */
  people: Person[];
  /** You are the team's only admin: nobody's admin rights can be taken away until you make someone else one. */
  only_admin: boolean;
  /** What people sign in with, like "Google" or "email (local)". */
  provider: string;
  join: Join;
}

/** What POST /api/admin/people/join answers. */
export interface JoinSaved {
  ok: true;
  /** The lists as saved: lowercased, sorted, without duplicates or a leading @. */
  join: Join;
}

/** What promote, demote, remove and restore answer: where the person stands now. */
export interface PersonDone {
  ok: true;
  handle: string;
  admin: boolean;
  removed: boolean;
  /** It was you: after taking your own admin rights away, leave the panel. */
  you: boolean;
}

// ---------------------------------------------------------------- settings: GET /api/admin/settings

export type UpdatePolicy = 'auto' | 'notify' | 'off';
export type ChatKind = '' | 'slack' | 'google-chat' | 'discord' | 'teams' | 'generic';

/** The settings' values, by the names the settings form uses. Boxes of one entry per line are text, a line each. */
export interface SettingsValues {
  token_days: number;
  sends_per_minute: number;
  loop_max: number;
  loop_minutes: number;
  max_agents: number;
  max_laptops: number;
  downloads_per_hour: number;
  ignored_paths: string;
  update_policy: UpdatePolicy;
  /** Empty accepts any version. */
  min_version: string;
  release_check: boolean;
  agreements: string;
  announce_hours: number;
  /** Empty posts nothing. */
  chat_webhook: string;
  chat_kind: ChatKind;
  wait_ping_minutes: number;
  work_start: number;
  work_end: number;
  time_zone: string;
  weekdays: boolean;
  weekly_report: boolean;
  check_hours: number;
}

export interface Settings {
  settings: SettingsValues;
  /** A GitHub webhook secret is stored. The hub never sends it. */
  github_secret_set: boolean;
  /** A GitHub token is stored. The hub never sends it. */
  github_token_set: boolean;
  /** When an admin last changed the settings or who may join; null until one did. */
  updated: Stamp;
  /** That admin's handle; empty until one did. */
  updated_by: string;
}

/** What POST /api/admin/settings takes: any of the values, those left out staying as they are, and GitHub's. */
export interface SettingsPost extends Partial<SettingsValues> {
  /** A new webhook secret; empty or left out keeps the stored one. */
  github_secret?: string;
  /** A new token; empty or left out keeps the stored one. */
  github_token?: string;
  /** Clears the stored secret. */
  forget_github_secret?: boolean;
  /** Clears the stored token. */
  forget_github_token?: boolean;
}

/** What POST /api/admin/settings answers: the settings as saved. */
export interface SettingsSaved extends Settings {
  ok: true;
}

// ---------------------------------------------------------------- events: GET /api/admin/events?skip=N

/** One entry in the event log. Never the text of a message. */
export interface LogEvent {
  at: string;
  /** What happened, like "signed in", "laptop revoked", "person removed" or "settings changed". */
  kind: string;
  /** Who did it, by handle; empty when nobody did. */
  actor: string;
  /** Whom or what it was done to: a handle, a laptop's id; empty when nothing. */
  subject: string;
  /** More about it, like a laptop's name or the settings that changed. */
  detail: string;
}

export interface Events {
  /** Newest first, up to 200. */
  events: LogEvent[];
  /** How many newer events this page leaves out. */
  skip: number;
  /** The skip of the page of newer events; null on the first page. */
  newer: number | null;
  /** The skip of the page of older events; null on the last page. */
  older: number | null;
}

// ---------------------------------------------------------------- calls

async function call<T>(path: string, body?: unknown): Promise<T> {
  const init: RequestInit = { credentials: 'same-origin', headers: { Accept: 'application/json' } };
  if (body !== undefined) {
    init.method = 'POST';
    init.headers = { Accept: 'application/json', 'Content-Type': 'application/json' };
    init.body = JSON.stringify(body);
  }
  let res: Response;
  try {
    res = await fetch(path, init);
  } catch {
    throw new AdminError("The hub can't be reached. Check your connection and try again.", 0);
  }
  const data: unknown = await res.json().catch(() => null); // a proxy's error page, or the hub refusing a post from another site
  if (!res.ok) {
    const p = (data ?? {}) as Partial<Problem>;
    throw new AdminError(p.error || `The hub answered ${res.status}. Try again.`, res.status, p.field || undefined);
  }
  return data as T;
}

export const getOverview = () => call<Overview>('/api/admin');
export const getPeople = () => call<People>('/api/admin/people');
/** Lists left out stay as they are. */
export const postJoin = (join: Partial<Join>) => call<JoinSaved>('/api/admin/people/join', join);
export const promote = (handle: string) => call<PersonDone>('/api/admin/people/promote', { handle });
/** Refused for the team's only admin. */
export const demote = (handle: string) => call<PersonDone>('/api/admin/people/demote', { handle });
/** Refused for yourself. */
export const remove = (handle: string) => call<PersonDone>('/api/admin/people/remove', { handle });
export const restore = (handle: string) => call<PersonDone>('/api/admin/people/restore', { handle });
export const getLaptops = () => call<Laptops>('/api/admin/laptops');
export const revokeLaptop = (id: string) => call<Revoked>('/api/admin/laptops/revoke', { id });
export const getSettings = () => call<Settings>('/api/admin/settings');
export const postSettings = (s: SettingsPost) => call<SettingsSaved>('/api/admin/settings', s);
export const getEvents = (skip = 0) => call<Events>(skip > 0 ? `/api/admin/events?skip=${skip}` : '/api/admin/events');

// ---------------------------------------------------------------- the settings form

export type FieldKind = 'number' | 'text' | 'textarea' | 'choice' | 'checkbox' | 'secret';

export interface Choice {
  value: string;
  label: string;
  /** Said after the label. */
  hint?: string;
}

export interface SettingField {
  /** Its name in SettingsValues; for a secret, the name a new one is sent under. */
  name: keyof SettingsValues | 'github_secret' | 'github_token';
  /** The fieldset it belongs to: one of settingGroups' legends. */
  group: string;
  label: string;
  /** Said under the label. `Backticks` mark code. */
  hint?: string;
  kind: FieldKind;
  /** number: the smallest and the largest the hub takes. Every number is a whole number and required. */
  min?: number;
  max?: number;
  /** textarea: the most characters the hub takes. */
  maxLength?: number;
  placeholder?: string;
  /** text: a pattern the hub holds the value to, when it isn't empty. */
  pattern?: string;
  /** choice: the values, in order. */
  choices?: Choice[];
  /** secret: the field of Settings that says one is stored, and what to say then instead of hint. */
  setFlag?: 'github_secret_set' | 'github_token_set';
  hintWhenSet?: string;
  /** secret: the checkbox, shown when one is stored, that clears it. */
  forget?: { name: 'forget_github_secret' | 'forget_github_token'; label: string };
}

/** The settings form's fieldsets, in order. */
export const settingGroups: { legend: string; intro?: string }[] = [
  { legend: 'Sign-in' },
  { legend: 'Limits' },
  { legend: 'Overlaps' },
  { legend: 'Updates' },
  { legend: 'Agents' },
  { legend: 'Team chat' },
  {
    legend: 'GitHub merge notices',
    intro:
      'When a pull request merges, agents whose unpushed work touches its files are told to rebase. In GitHub, add a ' +
      'webhook for pull request events to `/github/webhook` on this hub, with the secret below, and give the hub a ' +
      'read-only token.',
  },
  { legend: 'Laptop checks' },
];

/** Every setting, in the form's order. The hub checks every limit again, and names the field it refuses. */
export const settingFields: SettingField[] = [
  {
    name: 'token_days', group: 'Sign-in', kind: 'number', min: 0, max: 3650, label: 'Days a laptop stays signed in',
    hint: 'Then its person signs it in again. 0 keeps laptops signed in until someone revokes them.',
  },
  {
    name: 'sends_per_minute', group: 'Limits', kind: 'number', min: 1, max: 1000, label: 'Messages per minute',
    hint: 'What one person or one agent can send.',
  },
  {
    name: 'loop_max', group: 'Limits', kind: 'number', min: 1, max: 1000, label: 'Messages between two agents',
    hint: 'How many two agents can exchange within the loop window before a person must step in.',
  },
  { name: 'loop_minutes', group: 'Limits', kind: 'number', min: 1, max: 1440, label: 'Loop window, in minutes' },
  {
    name: 'max_agents', group: 'Limits', kind: 'number', min: 1, max: 1000, label: 'Agents per laptop',
    hint: 'Claude Code sessions one laptop can report.',
  },
  {
    name: 'max_laptops', group: 'Limits', kind: 'number', min: 1, max: 1000, label: 'Laptops per person',
    hint: 'How many laptops one person can have signed in at once.',
  },
  {
    name: 'downloads_per_hour', group: 'Limits', kind: 'number', min: 1, max: 100000, label: 'Downloads per hour',
    hint: 'Installs and updates one network address can start.',
  },
  {
    name: 'ignored_paths', group: 'Overlaps', kind: 'textarea', label: 'Ignored paths',
    hint:
      "Files that never count as an overlap, one per line, on top of each repo's .gitattributes. `traces/**` covers " +
      'a folder, `*.lock` any file with that name, `docs/*.md` the files right in docs.',
  },
  {
    name: 'update_policy', group: 'Updates', kind: 'choice', label: 'Update policy',
    choices: [
      { value: 'auto', label: 'Automatic.', hint: 'Laptops update themselves when their agents are idle.' },
      { value: 'notify', label: 'Notify.', hint: 'Laptops say an update is ready, and their people choose when.' },
      { value: 'off', label: 'Off.', hint: 'Laptops stay on their version until their people run `murmur update`.' },
    ],
  },
  {
    name: 'min_version', group: 'Updates', kind: 'text', label: 'Oldest accepted version', placeholder: '1.4.0',
    pattern: '^v?\\d+\\.\\d+\\.\\d+$',
    hint: 'Older laptops are refused until they update. Leave empty to accept any version.',
  },
  {
    name: 'release_check', group: 'Updates', kind: 'checkbox', label: 'Look for new murmur releases once a day',
    hint: 'Admins see them on the overview.',
  },
  {
    name: 'agreements', group: 'Agents', kind: 'textarea', maxLength: 4000, label: 'Team agreements',
    hint:
      'How your agents should work together, in plain sentences. Every agent reads them at the start of each ' +
      'session. For example: message the agent changing a file before you change it; ask a person only for ' +
      'decisions, with options.',
  },
  {
    name: 'announce_hours', group: 'Agents', kind: 'number', min: 1, max: 720, label: 'Hours an announcement stays up',
    hint: 'Announcements to a repo reach its agents and stay on the team page this long.',
  },
  {
    name: 'chat_webhook', group: 'Team chat', kind: 'text', label: 'Chat webhook',
    placeholder: 'https://hooks.slack.com/services/…', pattern: '^https://',
    hint:
      'An incoming webhook address from your chat app. The hub posts there when an agent waits too long for its ' +
      'person, and the weekly report. Empty posts nothing.',
  },
  {
    name: 'chat_kind', group: 'Team chat', kind: 'choice', label: 'Chat app',
    choices: [
      { value: '', label: 'Choose…' },
      { value: 'slack', label: 'Slack' },
      { value: 'google-chat', label: 'Google Chat' },
      { value: 'discord', label: 'Discord' },
      { value: 'teams', label: 'Microsoft Teams' },
      { value: 'generic', label: 'Other (JSON with a text field)' },
    ],
  },
  {
    name: 'wait_ping_minutes', group: 'Team chat', kind: 'number', min: 0, max: 1440, label: 'Minutes before a wait is posted',
    hint: 'How long an agent waits for its person before the chat hears of it. 0 never posts waits.',
  },
  {
    name: 'work_start', group: 'Team chat', kind: 'number', min: 0, max: 23, label: 'Working day starts at',
    hint: 'Hour of the day, 0 to 23. Nothing is posted outside working hours.',
  },
  {
    name: 'work_end', group: 'Team chat', kind: 'number', min: 1, max: 24, label: 'Working day ends at',
    hint: 'Hour of the day, 1 to 24.', // and after the start: the hub refuses it otherwise
  },
  {
    name: 'time_zone', group: 'Team chat', kind: 'text', label: 'Time zone',
    hint: 'Like Europe/Paris, America/New_York or UTC.',
  },
  { name: 'weekdays', group: 'Team chat', kind: 'checkbox', label: 'Monday to Friday only' },
  {
    name: 'weekly_report', group: 'Team chat', kind: 'checkbox', label: 'Post a weekly report',
    hint:
      "Monday at the start of the working day: how long agents waited, the files that collided most, and " +
      "conversations between people's agents. Team totals only.",
  },
  {
    name: 'github_secret', group: 'GitHub merge notices', kind: 'secret', label: 'Webhook secret',
    hint: 'None yet.', setFlag: 'github_secret_set', hintWhenSet: 'A secret is set. Type a new one to replace it.',
    forget: { name: 'forget_github_secret', label: 'Forget the secret' },
  },
  {
    name: 'github_token', group: 'GitHub merge notices', kind: 'secret', label: 'Read-only token',
    hint: 'A fine-grained token with read access to pull requests of your repositories.',
    setFlag: 'github_token_set', hintWhenSet: 'A token is set. Type a new one to replace it.',
    forget: { name: 'forget_github_token', label: 'Forget the token' },
  },
  {
    name: 'check_hours', group: 'Laptop checks', kind: 'number', min: 0, max: 720, label: 'Hours between checks',
    hint: 'Each laptop checks its own setup this often and reports to the admins. 0 turns checks off.',
  },
];

// ---------------------------------------------------------------- who may join

/** The who-may-join form's fields, in order. */
export const joinFields: { name: keyof Join; label: string; hint: string }[] = [
  { name: 'allowed_domains', label: 'Email domains', hint: 'One per line, like example.com.' },
  { name: 'allowed_emails', label: 'Email addresses', hint: 'One per line, for people outside those domains.' },
  {
    name: 'allowed_orgs', label: 'GitHub organisations',
    hint: 'One per line, by their name in GitHub addresses. Only for sign-in with GitHub.',
  },
];

/** What the people page says about who may join; provider comes from People. */
export const joinIntro = (provider: string) =>
  `People sign in with ${provider}. They join when their email is in one of these domains or on the list, or when ` +
  'they belong to one of these GitHub organisations. With nothing listed, only admins can sign in.';

export const joinNote =
  'Taking someone off these lists stops their next sign-in, not their laptops: remove them from the team for that.';

// ---------------------------------------------------------------- asking first

/** What to ask before an action that signs someone out or takes rights away. */
export interface Ask {
  title: string;
  text: string;
  /** The button that goes ahead. */
  button: string;
}

/** Before demote. Don't offer it when People.only_admin is true: the hub refuses. */
export function askDemote(p: Person): Ask {
  const title = p.you ? 'Remove your admin rights?' : `Remove ${p.handle}'s admin rights?`;
  let text = p.you
    ? 'You can no longer open this panel, change settings or remove anyone. Another admin can give them back.'
    : `${p.handle} can no longer open this panel, change settings or remove anyone.`;
  if (p.in_admins_env) {
    text += ` ${p.handle} is in ADMINS in the hub's configuration, so becomes an admin again at the next sign-in.`;
  }
  return { title, text, button: title.slice(0, -1) };
}

/** Before remove. Don't offer it for yourself: the hub refuses. */
export function askRemove(p: Person): Ask {
  return {
    title: `Remove ${p.handle}?`,
    text:
      'This signs out every laptop of theirs now, deletes the messages waiting for them, takes them off the team and ' +
      'stops them from signing in again. Their account with your identity provider is not touched, and you can let ' +
      'them back in later.',
    button: `Remove ${p.handle}`,
  };
}

/** Before revokeLaptop. */
export function askRevoke(l: Laptop): Ask {
  const host = l.host || `laptop ${l.id}`;
  return {
    title: `Revoke ${host}?`,
    text: `It disconnects now and can't connect again. To use murmur on it again, ${l.handle} runs murmur init on it.`,
    button: `Revoke ${host}`,
  };
}
