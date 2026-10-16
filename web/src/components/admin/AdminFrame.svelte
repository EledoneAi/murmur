<script lang="ts">
  // Every admin page: the header, the panel's tabs, and the page itself once we know you are an admin.
  import { onMount, type Snippet } from 'svelte'
  import Top from '../Top.svelte'
  import { me as whoAmI, signIn, type Me } from '../../lib/api'

  type Props = { page: 'overview' | 'people' | 'laptops' | 'settings' | 'events'; title: string; children: Snippet<[Me]> }
  let { page, title, children }: Props = $props()
  let me = $state<Me | null>(null)
  let refused = $state(false)

  onMount(async () => {
    const who = await whoAmI().catch(() => null)
    if (!who || 'signIn' in who) return signIn()
    me = who.me
    refused = !me.admin
  })

  const tabs = [['overview', 'Overview', '/admin'], ['people', 'People', '/admin/people'], ['laptops', 'Laptops', '/admin/laptops'],
    ['settings', 'Settings', '/admin/settings'], ['events', 'Events', '/admin/events']] as const
</script>

<Top active="admin" {me} />
{#if me && !refused}
  <div class="mx-auto max-w-4xl">
    <nav class="mt-8 flex gap-6 text-sm" aria-label="Admin panel">
      {#each tabs as [key, label, href]}
        <a {href} aria-current={key === page ? 'page' : undefined}
          class="hover:no-underline {key === page ? 'font-medium text-strong' : 'text-muted hover:text-text'}">{label}</a>
      {/each}
    </nav>
    <h1 class="mt-6 mb-8 text-2xl font-semibold tracking-tight">{title}</h1>
    {@render children(me)}
  </div>
{:else if refused}
  <section class="mx-auto mt-16 max-w-xl space-y-3">
    <h1 class="text-2xl font-semibold tracking-tight">Admins only</h1>
    <p class="text-muted">The admin panel is for the team's admins. An admin can make you one on its People page.</p>
    <a href="/team" class="text-sm">Back to the team</a>
  </section>
{/if}
