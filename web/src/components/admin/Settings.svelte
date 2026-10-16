<script lang="ts">
  // The team's settings, drawn from the list of fields: changes apply at once, without a restart.
  import { onMount } from 'svelte'
  import AdminFrame from './AdminFrame.svelte'
  import Hint from './Hint.svelte'
  import { getSettings, postSettings, settingFields, settingGroups, type Settings, type SettingsPost, type SettingField } from '../../lib/admin'
  import { agoDate } from '../../lib/vocab'

  let view = $state<Settings | null>(null)
  let values = $state<Record<string, unknown>>({})
  let secrets = $state<Record<string, string>>({})
  let forget = $state<Record<string, boolean>>({})
  let outcome = $state<{ text: string; err?: boolean; field?: string } | null>(null)
  let saving = $state(false)

  function take(s: Settings) {
    view = s
    values = { ...s.settings }
    secrets = {}
    forget = {}
  }
  onMount(async () => { try { take(await getSettings()) } catch (e) { outcome = { text: (e as Error).message, err: true } } })

  const inGroup = (legend: string) => settingFields.filter(f => f.group === legend)

  async function save(e: SubmitEvent) {
    e.preventDefault()
    saving = true
    const post: SettingsPost = { ...(values as Partial<SettingsPost>) }
    for (const [k, v] of Object.entries(secrets)) if (v) (post as Record<string, unknown>)[k] = v
    for (const [k, v] of Object.entries(forget)) if (v) (post as Record<string, unknown>)[k] = true
    try {
      take(await postSettings(post))
      outcome = { text: 'Saved. The team has it now.' }
    } catch (err) {
      const x = err as Error & { field?: string }
      outcome = { text: x.message, err: true, field: x.field }
      document.getElementById(x.field || '')?.focus()
    }
    saving = false
  }
  const isSet = (f: SettingField) => !!(f.setFlag && view?.[f.setFlag])
</script>

<AdminFrame page="settings" title="Settings">
  {#snippet children()}
    {#if view}
      <form class="max-w-2xl" onsubmit={save} novalidate>
        <p class="mb-10 text-sm text-muted">Changes apply at once, without a restart.{#if view.updated}{' '}Last changed by {view.updated_by}, {agoDate(view.updated)}.{/if}</p>
        {#each settingGroups as g (g.legend)}
          <fieldset class="mb-12">
            <legend class="mb-4 font-semibold text-strong">{g.legend}</legend>
            {#if g.intro}<p class="mb-5 text-sm text-muted"><Hint text={g.intro} /></p>{/if}
            <div class="grid gap-x-8 gap-y-6 sm:grid-cols-2">
              {#each inGroup(g.legend) as f (f.name)}
                <div class={f.kind === 'number' || f.kind === 'checkbox' ? 'flex flex-col' : 'sm:col-span-2'}>
                  {#if f.kind === 'checkbox'}
                    <label class="flex items-baseline gap-2.5 text-sm font-medium text-strong"><input id={f.name} type="checkbox" class="accent-current" bind:checked={values[f.name] as boolean} /> {f.label}</label>
                    {#if f.hint}<span class="hint ml-6"><Hint text={f.hint} /></span>{/if}
                  {:else if f.kind === 'choice'}
                    <span class="label">{f.label}</span>
                    {#if f.hint}<span class="hint"><Hint text={f.hint} /></span>{/if}
                    <div class="mt-2 space-y-1.5">
                      {#each f.choices ?? [] as c}
                        <label class="flex items-baseline gap-2.5 text-sm"><input type="radio" name={f.name} value={c.value} class="accent-current" bind:group={values[f.name]} />
                          <span><span class="text-strong">{c.label}</span>{#if c.hint}{' '}<span class="text-muted"><Hint text={c.hint} /></span>{/if}</span></label>
                      {/each}
                    </div>
                  {:else}
                    <label for={f.name} class="label">{f.label}</label>
                    <span class="hint mb-1.5"><Hint text={isSet(f) && f.hintWhenSet ? f.hintWhenSet : f.hint} /></span>
                    {#if f.kind === 'number'}
                      <input id={f.name} type="number" min={f.min} max={f.max} step="1" class="input data mt-auto !w-32" bind:value={values[f.name] as number} />
                    {:else if f.kind === 'text'}
                      <input id={f.name} type="text" class="input" placeholder={f.placeholder} bind:value={values[f.name] as string} />
                    {:else if f.kind === 'textarea'}
                      <textarea id={f.name} rows="4" class="input" maxlength={f.maxLength} placeholder={f.placeholder} bind:value={values[f.name] as string}></textarea>
                    {:else if f.kind === 'secret'}
                      <input id={f.name} type="password" autocomplete="off" class="input" placeholder={isSet(f) ? 'Stored. Type to replace it.' : ''} bind:value={secrets[f.name]} />
                      {#if isSet(f) && f.forget}<label class="mt-2 flex items-baseline gap-2 text-sm"><input type="checkbox" class="accent-current" bind:checked={forget[f.forget.name]} /> {f.forget.label}</label>{/if}
                    {/if}
                  {/if}
                  {#if outcome?.err && outcome.field === f.name}<p class="mt-1 text-sm text-thorn">{outcome.text}</p>{/if}
                </div>
              {/each}
            </div>
          </fieldset>
        {/each}
        <div class="sticky bottom-0 flex items-center gap-4 border-t border-line bg-bg py-4">
          <button class="btn-primary" disabled={saving}>Save settings</button>
          {#if outcome && !(outcome.err && outcome.field)}<span class="text-sm {outcome.err ? 'text-thorn' : 'text-muted'}" role="status">{outcome.text}</span>{/if}
          {#if outcome?.err && outcome.field}<span class="text-sm text-thorn" role="alert">Not saved: see the field above.</span>{/if}
        </div>
      </form>
    {:else if outcome}<p class="text-thorn">{outcome.text}</p>{/if}
  {/snippet}
</AdminFrame>
