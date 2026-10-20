<script lang="ts">
  // The front page. Signed out, it says what murmur is and signs you in. Signed in, it gives the lines that install
  // murmur and join this hub, your laptops and the team at a glance.
  import { onMount } from 'svelte'
  import Top from './Top.svelte'
  import Logo from './Logo.svelte'
  import { get, me as whoAmI, post, type Device, type Home, type Me, type Person, type SignIn, type Team } from '../lib/api'
  import { agoDate, chip, initials, plural } from '../lib/vocab'

  let me = $state<Me | null>(null)
  let signIn = $state<SignIn | null>(null)
  let home = $state<Home | null>(null)
  let people = $state<Person[]>([])
  let error = $state('')
  let copied = $state(false)
  let asking = $state('') // the laptop whose revoke waits for a yes
  let outcome = $state<{ text: string; err?: boolean } | null>(null)
  const next = new URLSearchParams(location.search).get('next') || '/'

  async function load() {
    try {
      const who = await whoAmI()
      if ('signIn' in who) { signIn = who.signIn; return }
      me = who.me
      home = await get<Home>('/api/home')
      people = (await get<Team>('/api/team')).people
    } catch (e) {
      error = (e as Error).message
    }
  }
  onMount(load)

  async function copy() {
    if (!home) return
    await navigator.clipboard.writeText(`${home.brew} && ${home.join}`)
    copied = true
    setTimeout(() => (copied = false), 1800)
  }

  async function revoke(d: Device) {
    try {
      await post('/api/devices/revoke', { id: d.id })
      outcome = { text: `${d.host || 'The laptop'} is revoked: it disconnected and can't connect again.` }
      asking = ''
      home = await get<Home>('/api/home')
    } catch (e) {
      outcome = { text: (e as Error).message, err: true }
    }
  }

  const letters = $derived(initials(people.map(p => p.handle)))
</script>

{#if signIn}
  <section class="mx-auto mt-[16vh] max-w-md space-y-6">
    <div class="flex items-center gap-3 text-strong"><Logo size={36} /><span class="text-2xl font-semibold tracking-tight">murmur</span></div>
    <p class="text-lg leading-relaxed">Your team's coding agents, in one console. They talk to each other while they work,
      and ask you only when they need a person.</p>
    {#if signIn.dev}
      <form method="post" action="/dev-login" class="space-y-2">
        <input type="hidden" name="next" value={next} />
        <label for="email" class="label">Email</label>
        <div class="flex gap-2"><input id="email" name="email" type="email" class="input" placeholder="you@example.com" required />
          <button class="btn-primary shrink-0">Sign in</button></div>
        <p class="hint">A hub on this computer: any address signs you in.</p>
      </form>
    {:else}
      <a class="btn-primary hover:no-underline" href={'/login?next=' + encodeURIComponent(next)}>Sign in with {signIn.provider}</a>
    {/if}
    <p class="text-sm text-muted">Runs on macOS and Linux, and on Windows inside WSL, next to your coding tools.</p>
  </section>
{:else if me && home}
  <Top active="home" {me} />
  <div class="mx-auto max-w-2xl">
    <div class="mt-10 mb-10 flex items-baseline gap-4">
      <h1 class="text-2xl font-semibold tracking-tight">Hello, {(me.name || me.handle).split(' ')[0]}</h1>
      <form method="post" action="/logout" class="ml-auto"><button class="btn-quiet">Sign out</button></form>
    </div>

    <section class="mb-12" aria-labelledby="install">
      <h2 id="install" class="mb-2 font-semibold">Install on a laptop</h2>
      <div class="flex items-start gap-3 rounded-md bg-raised px-4 py-2.5">
        <code class="min-w-0 flex-1 overflow-x-auto whitespace-pre select-all">{home.brew}{'\n'}{home.join}</code>
        <button class="btn-quiet shrink-0" onclick={copy}>{copied ? 'Copied' : 'Copy'}</button>
      </div>
      <p class="mt-2 text-sm text-muted">Homebrew installs murmur; <code>murmur join</code> signs the laptop in with your account and
        connects your coding tools. Then open the console with <code>murmur</code>.</p>
      <p class="mt-1 text-sm text-muted">No Homebrew? <code class="select-all">{home.install}</code> puts murmur in
        <code>~/.local/bin</code> and joins this hub.</p>
    </section>

    <section class="mb-12" aria-labelledby="laptops">
      <h2 id="laptops" class="mb-2 font-semibold">Your laptops</h2>
      {#if home.devices.length === 0}
        <p class="text-muted">None yet. Run the line above on a laptop.</p>
      {:else}
        <ul>
          {#each home.devices as d (d.id)}
            <li class="rule py-3 first:border-t-0">
              <div class="flex items-baseline gap-4">
                <span class="data text-strong">{d.host || 'unnamed'}</span>
                <span class="text-sm text-muted">{d.online ? 'online' : 'seen ' + agoDate(d.last_seen)} · added {agoDate(d.created)}</span>
                {#if asking !== d.id}<button class="btn-quiet ml-auto" onclick={() => (asking = d.id)}>Revoke</button>{/if}
              </div>
              {#if asking === d.id}
                <div class="mt-2 flex items-center gap-4 text-sm">
                  <span class="text-muted">It disconnects at once and can't connect again.</span>
                  <button class="btn-danger" onclick={() => revoke(d)}>Revoke {d.host}</button>
                  <button class="btn-quiet" onclick={() => (asking = '')}>Cancel</button>
                </div>
              {/if}
            </li>
          {/each}
        </ul>
      {/if}
      {#if outcome}<p class="mt-2 text-sm {outcome.err ? 'text-thorn' : 'text-muted'}" role="status">{outcome.text}</p>{/if}
    </section>

    <section aria-labelledby="team">
      <div class="mb-2 flex items-baseline"><h2 id="team" class="font-semibold">Team</h2><a href="/team" class="ml-auto text-sm">Open the team board</a></div>
      <ul>
        {#each people as p (p.handle)}
          <li class="rule flex items-center gap-3 py-2.5 first:border-t-0">
            <span class="chip {chip(p.colour)}">{letters[p.handle]}</span>
            <span class="text-strong">{p.handle}</span><span class="text-sm text-muted">{p.name || ''}</span>
            <span class="ml-auto text-sm text-muted">{p.online ? '' : 'offline · '}{(p.agents || []).length} {plural((p.agents || []).length, 'agent', 'agents')}</span>
          </li>
        {/each}
      </ul>
    </section>
  </div>
{:else if error}
  <Top />
  <p class="mt-10 text-thorn">{error}</p>
{/if}
