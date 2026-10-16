<script lang="ts">
  // A hint, where `backticks` mark code.
  let { text = '' }: { text?: string } = $props()
  const parts = $derived(text.split('`'))
</script>
{#each parts as p, i}{#if i % 2}<code>{p}</code>{:else}{p}{/if}{/each}
