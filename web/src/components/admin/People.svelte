<script lang="ts">
  // Everyone on the team, what they may do, and who may join.
  import { onMount } from 'svelte'
  import AdminFrame from './AdminFrame.svelte'
  import Confirm from './Confirm.svelte'
  import { askDemote, askRemove, demote, getPeople, joinFields, joinIntro, joinNote, postJoin, promote, remove, restore,
    type Ask, type Join, type People, type Person } from '../../lib/admin'
  import { agoDate, chip, initials } from '../../lib/vocab'

  let view = $state<People | null>(null)
  let join = $state<Join>({ allowed_domains: '', allowed_emails: '', allowed_orgs: '' })
  let asking = $state<{ handle: string; ask: Ask; go: () => Promise<unknown> } | null>(null)
  let busy = $state(false)
  let outcome = $state<{ text: string; err?: boolean } | null>(null)
  let joinOutcome = $state<{ text: string; err?: boolean; field?: string } | null>(null)

  async function load() {
    view = await getPeople()
    join = { ...view.join }
  }
  onMount(() => load().catch(e => (outcome = { text: (e as Error).message, err: true })))

  const letters = $derived(initials((view?.people ?? []).map(p => p.handle)))

  async function act(go: () => Promise<{ you: boolean; admin: boolean }>, done: string) {
    busy = true
    try {
      const r = await go()
      if (r.you && !r.admin) { location.href = '/'; return } // you stepped down: the panel is no longer yours
      outcome = { text: done }
      asking = null
      await load()
    } catch (e) {
      outcome = { text: (e as Error).message, err: true }
    }
    busy = false
  }

  async function saveJoin(e: SubmitEvent) {
    e.preventDefault()
    try {
      const r = await postJoin(join)
      join = { ...r.join }
      joinOutcome = { text: 'Saved.' }
    } catch (err) {
      const x = err as Error & { field?: string }
      joinOutcome = { text: x.message, err: true, field: x.field }
    }
  }
  const role = (p: Person) => (p.removed ? 'Removed' : p.admin ? 'Admin' : 'Member')
</script>

<AdminFrame page="people" title="People">
  {#snippet children()}
    {#if view}
      <section aria-label="Everyone">
        <ul>
          {#each view.people as p (p.handle)}
            <li class="rule py-3 first:border-t-0 {p.removed ? 'opacity-60' : ''}">
              <div class="flex flex-wrap items-center gap-x-3 gap-y-1">
                <span class="chip {chip(p.colour)}">{letters[p.handle]}</span>
                <span class="font-medium text-strong">{p.handle}</span>
                <span class="text-sm text-muted">{p.you ? 'you' : p.name || p.email}</span>
                <span class="text-sm {p.removed ? 'text-thorn' : 'text-muted'}">· {role(p)}{p.in_admins_env ? ' (from ADMINS)' : ''}</span>
                <span class="text-sm text-muted">· {p.laptops} {p.laptops === 1 ? 'laptop' : 'laptops'}, {p.online ? 'online' : p.last_seen ? 'seen ' + agoDate(p.last_seen) : 'never connected'}</span>
                <span class="ml-auto flex gap-4">
                  {#if p.removed}
                    {#if !p.you}<button class="btn-quiet" onclick={() => act(() => restore(p.handle), `${p.handle} may sign in again.`)}>Let back in</button>{/if}
                  {:else}
                    {#if !p.admin}<button class="btn-quiet" onclick={() => act(() => promote(p.handle), `${p.handle} is an admin.`)}>Make admin</button>
                    {:else if !view.only_admin}<button class="btn-quiet" onclick={() => (asking = { handle: p.handle, ask: askDemote(p), go: () => demote(p.handle) })}>Remove admin rights</button>{/if}
                    {#if !p.you}<button class="btn-quiet !text-thorn" onclick={() => (asking = { handle: p.handle, ask: askRemove(p), go: () => remove(p.handle) })}>Remove</button>{/if}
                  {/if}
                </span>
              </div>
              {#if asking?.handle === p.handle}
                {@const a = asking}
                <Confirm ask={a.ask} {busy} onyes={() => act(a.go as () => Promise<{ you: boolean; admin: boolean }>, 'Done.')} onno={() => (asking = null)} />
              {/if}
            </li>
          {/each}
        </ul>
        {#if outcome}<p class="mt-2 text-sm {outcome.err ? 'text-thorn' : 'text-muted'}" role="status">{outcome.text}</p>{/if}
      </section>

      <section class="mt-14 max-w-2xl" aria-labelledby="join">
        <h2 id="join" class="mb-2 font-semibold">Who may join</h2>
        <form class="space-y-5" onsubmit={saveJoin}>
          <p>{joinIntro(view.provider)}</p>
          <p class="text-sm text-muted">{joinNote}</p>
          {#each joinFields as f}
            <div><label for={f.name} class="label">{f.label}</label><span class="hint mb-1.5">{f.hint}</span>
              <textarea id={f.name} rows="3" class="input data {joinOutcome?.field === f.name ? '!border-thorn' : ''}" bind:value={join[f.name]}></textarea></div>
          {/each}
          <div class="flex items-center gap-4"><button class="btn-primary">Save</button>
            {#if joinOutcome}<span class="text-sm {joinOutcome.err ? 'text-thorn' : 'text-muted'}" role="status">{joinOutcome.text}</span>{/if}</div>
        </form>
      </section>
    {:else if outcome}<p class="text-thorn">{outcome.text}</p>{/if}
  {/snippet}
</AdminFrame>
