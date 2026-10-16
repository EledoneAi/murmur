<script lang="ts">
  // The event log: sign-ins, laptops, people, settings and announcements, newest first. Never a message's text.
  import { onMount } from 'svelte'
  import AdminFrame from './AdminFrame.svelte'
  import { getEvents, type Events } from '../../lib/admin'
  import { agoDate } from '../../lib/vocab'

  let view = $state<Events | null>(null)
  let error = $state('')
  async function go(skip: number) {
    try { view = await getEvents(skip); window.scrollTo(0, 0) } catch (e) { error = (e as Error).message }
  }
  onMount(() => go(0))
  const utc = (iso: string) => new Date(iso).toISOString().replace('T', ' ').slice(0, 16) + ' UTC'
</script>

<AdminFrame page="events" title="Events">
  {#snippet children()}
    {#if view}
      <p class="mb-6 text-sm text-muted">Sign-ins, laptops, people, settings and announcements, newest first. Never the text of a message.</p>
      {#if view.events.length === 0}<p class="text-muted">Nothing yet.</p>{:else}
        <table class="w-full text-sm">
          <tbody>
            {#each view.events as e, i (i + e.at)}
              <tr class="rule align-baseline {i === 0 ? 'border-t-0' : ''}">
                <td class="w-28 py-2.5 pr-4 text-muted" title={utc(e.at)}>{agoDate(e.at)}</td>
                <td class="py-2.5 pr-4"><span class="text-strong">{e.kind}</span>{#if e.subject} <span class="data">{e.subject}</span>{/if}
                  {#if e.detail}<span class="text-muted"> · {e.detail}</span>{/if}</td>
                <td class="py-2.5 text-right text-muted">{e.actor}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      {/if}
      {#if view.newer !== null || view.older !== null}
        <div class="mt-6 flex justify-between">
          {#if view.newer !== null}<button class="btn-quiet" onclick={() => go(view!.newer!)}>Newer</button>{:else}<span></span>{/if}
          {#if view.older !== null}<button class="btn-quiet" onclick={() => go(view!.older!)}>Older</button>{/if}
        </div>
      {/if}
    {:else if error}<p class="text-thorn">{error}</p>{/if}
  {/snippet}
</AdminFrame>
