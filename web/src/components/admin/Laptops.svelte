<script lang="ts">
  // Every laptop signed in to the hub: whose, which murmur, whether it is connected, how its own check went.
  import { onMount } from 'svelte'
  import AdminFrame from './AdminFrame.svelte'
  import Confirm from './Confirm.svelte'
  import { askRevoke, getLaptops, revokeLaptop, type Laptop } from '../../lib/admin'
  import { agoDate } from '../../lib/vocab'

  let laptops = $state<Laptop[] | null>(null)
  let asking = $state('')
  let busy = $state(false)
  let outcome = $state<{ text: string; err?: boolean } | null>(null)

  const load = async () => { laptops = (await getLaptops()).laptops }
  onMount(() => load().catch(e => (outcome = { text: (e as Error).message, err: true })))

  async function revoke(l: Laptop) {
    busy = true
    try {
      await revokeLaptop(l.id)
      outcome = { text: `${l.host || 'The laptop'} is revoked: it disconnected and can't connect again.` }
      asking = ''
      await load()
    } catch (e) {
      outcome = { text: (e as Error).message, err: true }
    }
    busy = false
  }
</script>

<AdminFrame page="laptops" title="Laptops">
  {#snippet children()}
    {#if laptops}
      {#if laptops.length === 0}<p class="text-muted">No laptop has signed in yet.</p>{:else}
        <ul>
          {#each laptops as l (l.id)}
            <li class="rule py-3 first:border-t-0 {l.revoked ? 'opacity-60' : ''}">
              <div class="flex flex-wrap items-baseline gap-x-3 gap-y-1">
                <span class="data text-strong">{l.host || 'unnamed'}</span>
                <span class="text-sm text-muted">{l.handle}</span>
                <span class="data text-sm text-muted">· {l.version || 'unknown'}</span>
                <span class="text-sm">· {#if l.revoked}<span class="text-muted">revoked</span>{:else if l.expired}<span class="text-thorn">sign-in expired</span>
                  {:else if l.online}online{:else}<span class="text-muted">{l.last_seen ? 'seen ' + agoDate(l.last_seen) : 'never connected'}</span>{/if}</span>
                {#if l.check && !l.check.ok}<span class="text-sm text-thorn">· check failing</span>{/if}
                {#if !l.revoked && asking !== l.id}<button class="btn-quiet ml-auto" onclick={() => (asking = l.id)}>Revoke</button>{/if}
              </div>
              {#if l.behind}<p class="text-sm text-thorn">{l.behind}</p>{/if}
              {#each l.check && !l.check.ok ? l.check.failing : [] as f}<p class="text-sm text-muted">{f.name}{#if f.detail}: {f.detail}{/if}</p>{/each}
              {#if asking === l.id}<Confirm ask={askRevoke(l)} {busy} onyes={() => revoke(l)} onno={() => (asking = '')} />{/if}
            </li>
          {/each}
        </ul>
      {/if}
      {#if outcome}<p class="mt-2 text-sm {outcome.err ? 'text-thorn' : 'text-muted'}" role="status">{outcome.text}</p>{/if}
    {:else if outcome}<p class="text-thorn">{outcome.text}</p>{/if}
  {/snippet}
</AdminFrame>
