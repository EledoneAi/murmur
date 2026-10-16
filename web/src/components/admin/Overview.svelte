<script lang="ts">
  // The panel's front page: this hub's release, the team and its laptops, and the laptops whose own check fails.
  import { onMount } from 'svelte'
  import AdminFrame from './AdminFrame.svelte'
  import { getOverview, type Overview } from '../../lib/admin'
  import { agoDate, plural } from '../../lib/vocab'

  let view = $state<Overview | null>(null)
  let error = $state('')
  onMount(async () => { try { view = await getOverview() } catch (e) { error = (e as Error).message } })
</script>

<AdminFrame page="overview" title="Admin">
  {#snippet children()}
    {#if view}
      <div class="space-y-12">
        <section aria-labelledby="hub">
          <h2 id="hub" class="mb-2 font-semibold">This hub</h2>
          <p>It runs murmur <span class="data">{view.version}</span>.
            {#if view.latest && view.newer}<span class="text-strong">murmur {view.latest.version} is out.</span> Upgrade the hub to move the team to it.
              {#if view.latest.url}<a href={view.latest.url} rel="noreferrer">What's new</a>{/if}
            {:else if view.latest}That is the latest release.{/if}</p>
          {#if view.latest}<p class="text-sm text-muted">Checked {agoDate(view.latest.checked)}.</p>{/if}
        </section>

        <section aria-labelledby="team">
          <h2 id="team" class="mb-2 font-semibold">Team</h2>
          <p>{view.team} {plural(view.team, 'person', 'people')} on the team{#if view.removed}, {view.removed} removed{/if}.
            {plural(view.admins.length, 'Admin', 'Admins')}: {view.admins.join(', ')}. <a href="/admin/people">People</a></p>
        </section>

        <section aria-labelledby="laptops">
          <h2 id="laptops" class="mb-2 font-semibold">Laptops</h2>
          <p>{view.signed_in} signed in, {view.online} online now, {view.seen_this_week} seen this week. <a href="/admin/laptops">Laptops</a></p>
          {#if view.versions.length}
            <table class="mt-4 w-full text-sm">
              <thead><tr class="text-left text-muted"><th class="pb-2 font-normal">Version</th><th class="pb-2 font-normal">Laptops</th><th class="pb-2 font-normal"></th></tr></thead>
              <tbody>{#each view.versions as v}<tr class="rule"><td class="data py-2">{v.version || 'unknown'}</td><td class="py-2">{v.count}</td>
                <td class="py-2 text-thorn">{v.behind}</td></tr>{/each}</tbody>
            </table>
          {/if}
        </section>

        <section aria-labelledby="checks">
          <h2 id="checks" class="mb-2 font-semibold">Laptop checks</h2>
          {#if view.failing.length}
            <ul class="space-y-3">
              {#each view.failing as l (l.id)}
                <li><p><span class="data text-strong">{l.host || 'unnamed'}</span> <span class="text-muted">· {l.handle} · {agoDate(l.check?.at)}</span></p>
                  {#each l.check?.failing ?? [] as f}<p class="text-sm"><span class="text-thorn">{f.name}</span>{#if f.detail}: {f.detail}{/if}</p>{/each}</li>
              {/each}
            </ul>
          {:else if view.reported}<p>Every laptop passed its last check.</p>
          {:else}<p class="text-muted">No laptop has sent a check yet.{#if !view.check_hours} Checks are off in <a href="/admin/settings">settings</a>.{/if}</p>{/if}
        </section>
      </div>
    {:else if error}<p class="text-thorn">{error}</p>{/if}
  {/snippet}
</AdminFrame>
