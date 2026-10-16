<script lang="ts">
  // The m: one thought, ╭─┬─╮, with three agents hanging from it. Its middle lamp turns teal when something needs
  // you; offline, its line turns dashed and its lamps hollow.
  let { size = 22, needs = false, offline = false }: { size?: number; needs?: boolean; offline?: boolean } = $props()
</script>

<svg width={size} height={size * 0.8} viewBox="0 0 30 24" role="img" aria-label="murmur" class="block shrink-0">
  <path d="M3 14 V8 Q3 3.5 7.5 3.5 H22.5 Q27 3.5 27 8 V14 M15 3.5 V14" fill="none" stroke="currentColor" stroke-width="2.2"
    stroke-linecap="round" stroke-dasharray={offline ? '3 3' : undefined} />
  {#each [3, 15, 27] as cx, i}
    <circle {cx} cy="19.5" r="3.1" class={i === 1 && needs ? 'fill-teal' : offline ? 'fill-none stroke-current' : 'fill-current'}
      stroke-width={offline ? 1.5 : 0} />
  {/each}
</svg>
