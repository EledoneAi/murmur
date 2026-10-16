<script lang="ts">
  // A notice from the hub: a sign-in that failed, a laptop that is gone. The hub writes what it says into the page.
  import Logo from './Logo.svelte'
  const meta = (name: string) => document.querySelector<HTMLMetaElement>(`meta[name="${name}"]`)?.content || ''
  const title = meta('murmur-title') || 'Nothing to say'
  const text = meta('murmur-text') || 'This page only shows what the hub has to tell you.'
</script>

<section class="mx-auto mt-[18vh] max-w-md space-y-4">
  <Logo size={30} />
  <h1 class="text-xl font-semibold">{title}</h1>
  <p>{text}</p>
  <a href="/" class="inline-block text-sm">Back to murmur</a>
</section>
