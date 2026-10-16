<script lang="ts">
  // The header of every page: the m and the wordmark, where you are, and you.
  import Logo from './Logo.svelte'
  import type { Me } from '../lib/api'
  import { chip } from '../lib/vocab'
  type Props = { active?: 'home' | 'team' | 'admin'; me?: Me | null; needs?: number; live?: string }
  let { active = 'home', me = null, needs = 0, live = '' }: Props = $props()
  const tab = (on: boolean) => (on ? 'text-strong font-medium' : 'text-muted hover:text-text')
</script>

<header class="flex h-14 items-center gap-8 border-b border-line">
  <a href="/" class="flex items-center gap-2.5 font-semibold text-strong hover:no-underline">
    <Logo needs={needs > 0} offline={live === 'offline'} /> murmur</a>
  {#if me}
    <nav class="flex gap-6 text-sm" aria-label="murmur">
      <a href="/team" class="{tab(active === 'team')} hover:no-underline" aria-current={active === 'team' ? 'page' : undefined}>Team</a>
      {#if me.admin}<a href="/admin" class="{tab(active === 'admin')} hover:no-underline" aria-current={active === 'admin' ? 'page' : undefined}>Admin</a>{/if}
    </nav>
  {/if}
  <div class="ml-auto flex items-center gap-5 text-sm">
    {#if needs > 0}<span class="text-teal">{needs} {needs === 1 ? 'needs' : 'need'} you</span>{/if}
    {#if live && live !== 'live'}<span class="text-muted">{live}…</span>{/if}
    {#if me}
      <a href="/" class="flex items-center gap-2 text-text hover:no-underline" title={`Signed in as ${me.email}`}>
        <span class="chip {chip(me.colour)}">{me.handle[0].toUpperCase()}</span>{me.handle}</a>
    {/if}
  </div>
</header>
