<script lang="ts">
  // The team board: the console's home screen in a browser, live. What needs you first, then every person and
  // their agents, the team's pull requests and the announcements. The hub streams the team whenever it changes.
  import { onDestroy, onMount } from 'svelte'
  import Top from './Top.svelte'
  import { me as whoAmI, signIn, type Agent, type Me, type Person, type Team } from '../lib/api'
  import { ago, chip, clock, fuse, initials, kindColour, lamp, place, plural, reason, shares, stateOf, texture, waiting, word } from '../lib/vocab'

  let me = $state<Me | null>(null)
  let team = $state<Team | null>(null)
  let live = $state('connecting')
  let now = $state(Date.now())
  let open = $state<Record<string, boolean>>({}) // the message forms opened, by address
  let drafts = $state<Record<string, string>>({})
  let notes = $state<Record<string, { text: string; err?: boolean }>>({})
  let reviewer = $state<Record<string, string>>({})
  let announceTo = $state('')
  let announceText = $state('')
  let announcing = $state(false)
  let reviewing = $state<Record<string, boolean>>({})
  let stream: EventSource | null = null
  let tick: ReturnType<typeof setInterval>

  onMount(async () => {
    const who = await whoAmI().catch(() => null)
    if (!who || 'signIn' in who) return signIn()
    me = who.me
    stream = new EventSource('/team/events')
    stream.onopen = () => (live = 'live')
    stream.onmessage = e => {
      const next = JSON.parse(e.data) as Team
      team = next
      if (!announceTo && next.repos.length) announceTo = next.repos[0]
    }
    stream.onerror = () => (live = stream?.readyState === EventSource.CLOSED ? 'offline' : 'reconnecting')
    tick = setInterval(() => (now = Date.now()), 1000)
  })
  onDestroy(() => { stream?.close(); clearInterval(tick) })

  const lampColour: Record<string, string> = { working: 'text-muted', needs: 'text-teal', waits: 'text-muted', idle: 'text-muted', shell: 'text-line', gone: 'text-line' }
  const people = $derived(team?.people ?? [])
  const letters = $derived(initials(people.map(p => p.handle)))
  const mine = (p: Person) => p.handle === me?.handle
  const mineWaiting = $derived(people.filter(mine).flatMap(p => (p.agents || []).filter(a => stateOf(a, true) === 'needs').map(a => ({ p, a }))))
  const agents = $derived(people.reduce((n, p) => n + (p.agents?.length || 0), 0))
  const online = $derived(people.filter(p => p.online).length)

  // second checkouts: one person's agents on the same repo and branch in different checkouts
  function checkoutOf(p: Person, a: Agent): number {
    const same = (p.agents || []).filter(b => b.repo === a.repo && b.branch === a.branch && b.branch).map(b => b.name).sort()
    return same.length > 1 ? same.indexOf(a.name) + 1 : 0
  }

  async function send(to: string, text: string, key: string, clear?: () => void) {
    notes[key] = { text: 'Sending…' }
    try {
      const res = await fetch('/team/send', { method: 'POST', credentials: 'same-origin',
        headers: { Accept: 'application/json' }, body: new URLSearchParams({ to, text }) })
      const r = await res.json()
      notes[key] = { text: r.result, err: !r.ok }
      if (r.ok) clear?.()
    } catch {
      notes[key] = { text: 'Not sent: the hub did not answer. Try again.', err: true }
    }
  }
</script>

<Top active="team" {me} needs={mineWaiting.length} {live} />

{#snippet fuseOf(waited: number, mine: boolean)}
  {@const f = fuse(waited)}
  <span class="data whitespace-pre"><span class={mine ? 'text-teal' : 'text-muted'}>{f.burnt}</span><span class="text-line">{f.rest}</span> <span class="text-muted">{clock(waited)}</span></span>
{/snippet}

{#if team}
  <div class="mt-10 mb-8 flex items-baseline gap-4">
    <h1 class="text-2xl font-semibold tracking-tight">Team</h1>
    <p class="text-sm text-muted">{online} online · {agents} {plural(agents, 'agent', 'agents')}</p>
  </div>

  <div class="grid gap-x-14 gap-y-12 lg:grid-cols-[minmax(0,1fr)_17rem]">
    <div>
      {#if mineWaiting.length}
        <section class="mb-10" aria-labelledby="needs">
          <h2 id="needs" class="mb-2 text-sm font-semibold text-teal">Needs you</h2>
          <ul>
            {#each mineWaiting as { a } (a.name)}
              <li class="grid grid-cols-[1rem_9.5rem_minmax(0,1fr)_auto] items-baseline gap-x-4 py-1">
                <span class="text-teal">◆</span><span class="data truncate text-strong">{a.name}</span>
                <span class="truncate text-teal">{reason(a)}</span>{@render fuseOf(now - (a.since || now), true)}
              </li>
            {/each}
          </ul>
          <p class="mt-2 text-[13px] text-muted">Answer it in its terminal; <code>g</code> in the murmur console takes you there.</p>
        </section>
      {/if}

      {#each people as p, i (p.handle)}
        <section class="py-6 {i > 0 || mineWaiting.length ? 'rule' : ''}" aria-label={p.handle}>
          <div class="mb-2 flex items-center gap-3">
            <span class="chip {chip(p.colour)}">{letters[p.handle]}</span>
            <h2 class="font-semibold">{p.handle}</h2>
            {#if mine(p)}<span class="text-sm text-muted">you</span>{:else if p.name}<span class="text-sm text-muted">{p.name}</span>{/if}
            <span class="ml-auto text-sm text-muted">{p.online ? '' : 'offline · '}{(p.agents || []).length} {plural((p.agents || []).length, 'agent', 'agents')}</span>
          </div>
          {#if (p.agents || []).length === 0}
            <p class="text-sm text-muted">No agent running.</p>
          {:else}
            <ul>
              {#each p.agents || [] as a (a.name)}
                {@const s = stateOf(a, mine(p), p.online)}
                {@const pl = place(a, checkoutOf(p, a))}
                {@const waited = now - (a.since || now)}
                {@const to = `${p.handle}/${a.name}`}
                <li>
                  <button class="-mx-2 grid w-[calc(100%+1rem)] cursor-pointer grid-cols-[1rem_9.5rem_minmax(0,1fr)_8.5rem] items-baseline gap-x-4 rounded-md px-2 py-1.5 text-left hover:bg-raised md:grid-cols-[1rem_9.5rem_12rem_minmax(0,1fr)_8.5rem] {open[to] ? 'bg-raised' : ''}"
                    aria-expanded={!!open[to]} onclick={() => (open[to] = !open[to])}>
                    <span class={lampColour[s]} title={word[s]}>{lamp[s]}</span>
                    <span class="data truncate text-strong">{a.name}</span>
                    <span class="data hidden truncate text-muted md:block">{pl.head}{#if pl.tail}<span class="text-lilac">{pl.tail}</span>{/if}</span>
                    <span class="truncate">
                      {#if s === 'needs'}<span class="text-teal">{reason(a)}</span>
                      {:else if s === 'waits'}<span class="text-muted">waiting for {p.handle}</span>
                      {:else if a.loop}<span class="text-strong">looping?</span> <span class="text-muted">the same command again and again</span>
                      {:else}{a.title || a.repo}{/if}
                    </span>
                    <span class="text-right">
                      {#if waiting(s)}{@render fuseOf(waited, s === 'needs')}
                      {:else if s === 'idle'}<span class="text-sm text-muted">idle {ago(waited)}</span>
                      {:else if s === 'working' && a.kinds}
                        <span class="data whitespace-pre" title={shares(a.kinds)}>{#each texture(a.kinds) as t}<span class={kindColour(t.kind)}>{t.sign}</span>{/each}</span>
                      {:else}<span class="text-sm text-muted">{word[s]}</span>{/if}
                    </span>
                  </button>
                  {#if open[to]}
                    <div class="mb-3 ml-8 mt-1 space-y-3 text-sm">
                      <dl class="grid grid-cols-[7rem_minmax(0,1fr)] gap-x-4 gap-y-1">
                        <dt class="text-muted">Where</dt><dd class="data">{a.repo}{#if a.dir}/{a.dir}{/if} · {pl.head}{pl.tail}</dd>
                        {#if a.doing}<dt class="text-muted">Now</dt><dd>{a.doing}</dd>{/if}
                        {#if a.changed}<dt class="text-muted">Changes</dt><dd>+{a.added} <span class="text-thorn">−{a.deleted}</span> in {a.changed} {plural(a.changed, 'file', 'files')}</dd>{/if}
                        {#if a.pr}<dt class="text-muted">Pull request</dt><dd>{#if a.pr_url}<a href={a.pr_url} rel="noreferrer">#{a.pr}</a>{:else}#{a.pr}{/if}</dd>{/if}
                        {#if a.context}<dt class="text-muted">Context</dt><dd>{a.context}% full</dd>{/if}
                        {#if a.app}<dt class="text-muted">Runs in</dt><dd>{a.app}</dd>{/if}
                      </dl>
                      <form class="space-y-2" onsubmit={e => { e.preventDefault(); send(to, drafts[to] || '', to, () => (drafts[to] = '')) }}>
                        <textarea class="input" rows="2" bind:value={drafts[to]} placeholder={`Message ${to}`} aria-label={`Message to ${to}`} required></textarea>
                        <div class="flex items-center gap-4">
                          <button class="btn-primary">Send</button>
                          <span class="text-[13px] text-muted">{s === 'working' ? `It reads it between two tool calls.` : waiting(s) ? `It reads it when its turn ends.` : `This wakes it.`}</span>
                          {#if notes[to]}<span class="ml-auto text-[13px] {notes[to].err ? 'text-thorn' : 'text-muted'}" role="status">{notes[to].text}</span>{/if}
                        </div>
                      </form>
                    </div>
                  {/if}
                </li>
              {/each}
            </ul>
          {/if}
        </section>
      {/each}
    </div>

    <aside class="space-y-10 text-sm">
      {#if team.news.filter(n => n.until > now).length}
        <section aria-labelledby="news">
          <h2 id="news" class="mb-3 font-semibold">Announcements</h2>
          <ul class="space-y-3">
            {#each team.news.filter(n => n.until > now) as n (n.at + n.repo)}
              <li><p class="whitespace-pre-wrap text-text">{n.text}</p>
                <p class="text-[13px] text-muted"><span class="data">{n.repo}</span> · {n.from}, {ago(now - n.at)} ago</p></li>
            {/each}
          </ul>
        </section>
      {/if}

      <section aria-labelledby="prs">
        <h2 id="prs" class="mb-3 font-semibold">Pull requests</h2>
        {#if team.prs.length === 0}
          <p class="text-muted">None open.</p>
        {:else}
          <ul class="space-y-4">
            {#each team.prs as pr (pr.name)}
              {@const owner = people.find(p => p.handle === pr.person)}
              <li class="space-y-1">
                <p class="text-strong">{pr.title || pr.name}</p>
                <p class="text-[13px] text-muted">{#if pr.url}<a href={pr.url} rel="noreferrer">{pr.name}</a>{:else}{pr.name}{/if}
                  · <span class="chip {chip(owner?.colour ?? 0)} !h-5 !min-w-5 !text-[11px]">{letters[pr.person]}</span> {pr.agent}{#if pr.since} · {ago(now - pr.since)}{/if}</p>
                {#if team.mine.length && pr.review && !reviewing[pr.name]}
                  <button class="btn-quiet" onclick={() => (reviewing[pr.name] = true)}>Ask one of your agents to review</button>
                {:else if team.mine.length && pr.review}
                  <form class="flex items-center gap-2" onsubmit={e => { e.preventDefault(); send(`${team!.me}/${reviewer[pr.name] || team!.mine[0]}`, pr.review, 'review ' + pr.name) }}>
                    <select class="input !w-auto !py-1" bind:value={reviewer[pr.name]} aria-label="Which of your agents reviews it">
                      {#each team.mine as n}<option value={n}>{n}</option>{/each}
                    </select>
                    <button class="btn">Ask</button>
                  </form>
                  {#if notes['review ' + pr.name]}<p class="text-[13px] {notes['review ' + pr.name].err ? 'text-thorn' : 'text-muted'}">{notes['review ' + pr.name].text}</p>{/if}
                {/if}
              </li>
            {/each}
          </ul>
        {/if}
      </section>

      {#if team.repos.length}
        <section aria-labelledby="announce">
          <h2 id="announce" class="mb-3 font-semibold">Tell a repo's agents</h2>
          {#if !announcing}
            <p class="mb-2 text-muted">Every agent working in the repo gets it, like that main is frozen.</p>
            <button class="btn" onclick={() => (announcing = true)}>Write an announcement</button>
          {:else}
            <form class="space-y-2" onsubmit={e => { e.preventDefault(); send('repo:' + announceTo, announceText, 'announce', () => (announceText = '')) }}>
              <select class="input" aria-label="Repo" bind:value={announceTo}>{#each team.repos as r}<option value={r}>{r}</option>{/each}</select>
              <textarea class="input" rows="3" bind:value={announceText} required aria-label="Announcement" placeholder="main is frozen until 15:00"></textarea>
              <div class="flex items-center gap-3"><button class="btn-primary">Announce</button><button type="button" class="btn-quiet" onclick={() => (announcing = false)}>Cancel</button></div>
              {#if notes.announce}<p class="text-[13px] {notes.announce.err ? 'text-thorn' : 'text-muted'}" role="status">{notes.announce.text}</p>{/if}
            </form>
          {/if}
        </section>
      {/if}
    </aside>
  </div>
{:else}
  <p class="mt-10 text-muted">Connecting to the hub…</p>
{/if}
