<script lang="ts">
  // Connecting a laptop: the laptop's murmur join sends you here. You say yes, and the hub either hands the laptop
  // its sign-in through the browser, or shows a code to paste into its terminal.
  import { onMount } from 'svelte'
  import Top from './Top.svelte'
  import { me as whoAmI, post, signIn, type Me } from '../lib/api'

  const q = new URLSearchParams(location.search)
  const host = q.get('host') || 'this laptop'
  let me = $state<Me | null>(null)
  let code = $state<{ code: string; phrase: string } | null>(null)
  let error = $state('')
  let busy = $state(false)

  onMount(async () => {
    const who = await whoAmI().catch(() => null)
    if (!who || 'signIn' in who) return signIn()
    me = who.me
  })

  async function connect() {
    busy = true
    try {
      const r = await post<{ redirect?: string; code?: string; phrase?: string }>('/api/cli/login',
        { port: q.get('port') || '0', challenge: q.get('challenge') || '', state: q.get('state') || '', host: q.get('host') || '' })
      if (r.redirect) location.href = r.redirect
      else if (r.code) code = { code: r.code, phrase: r.phrase || '' }
    } catch (e) {
      error = (e as Error).message
    }
    busy = false
  }
</script>

<Top {me} />
{#if me}
  <section class="mx-auto mt-16 max-w-xl space-y-4">
    {#if code}
      <h1 class="text-2xl font-semibold tracking-tight">Paste this code on <span class="data text-xl">{host}</span></h1>
      <p>Your terminal shows <code>{code.phrase}</code>. If it doesn't, or you didn't just run <code>murmur join</code>, close
        this page: someone else started this sign-in.</p>
      <p class="rounded-md bg-raised px-4 py-3"><code class="select-all text-base">{code.code}</code></p>
      <p class="text-sm text-muted">It works once, in the next ten minutes, and only for the laptop that asked. Never give it to anyone.</p>
    {:else}
      <h1 class="text-2xl font-semibold tracking-tight">Connect <span class="data text-xl">{host}</span>?</h1>
      <p>It asks to join murmur as <b class="font-semibold text-strong">{me.handle}</b> <span class="text-muted">({me.email})</span>.
        Its agents will see your team, and your team will see them.</p>
      <p class="text-sm text-muted">Connect only if you just ran <code>murmur join</code> on this laptop.</p>
      <div class="flex items-center gap-4 pt-2"><button class="btn-primary" disabled={busy} onclick={connect}>Connect {host}</button><a href="/" class="text-sm">Cancel</a></div>
      {#if error}<p class="text-sm text-thorn" role="alert">{error}</p>{/if}
    {/if}
  </section>
{/if}
