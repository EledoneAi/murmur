<script lang="ts">
  // Asks before an action that signs someone out or takes rights away: what it does, then the button that does it.
  import type { Ask } from '../../lib/admin'
  let { ask, onyes, onno, busy = false }: { ask: Ask; onyes: () => void; onno: () => void; busy?: boolean } = $props()
</script>

<div class="mt-2 mb-1 max-w-2xl space-y-2 text-sm" role="alertdialog" aria-label={ask.title}>
  <p><span class="font-medium text-strong">{ask.title}</span> <span class="text-muted">{ask.text}</span></p>
  <div class="flex items-center gap-4"><button class="btn-danger" disabled={busy} onclick={onyes}>{ask.button}</button>
    <button class="btn-quiet" onclick={onno}>Cancel</button></div>
</div>
