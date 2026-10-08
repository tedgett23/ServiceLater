<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import { when } from '$lib/format';
	import { ui } from '$lib/ui.svelte';

	type Settings = {
		record_types: string[];
		cmdb: boolean;
		record_type_options: { type: string; label: string }[];
		exclusions: { app: string; subject: 'task' | 'ci'; kind: string; field: string; label: string }[];
		log: { at: string; actor_name: string | null; setting: string; old: unknown; new: unknown }[];
	};

	let current = $state<Settings | null>(null);
	let all = $state(true);
	let types = $state<string[]>([]);
	let cmdb = $state(true);
	let saving = $state(false);
	let error = $state('');

	function take(s: Settings) {
		current = s;
		all = s.record_types.includes('*');
		types = all ? s.record_type_options.map((o) => o.type) : [...s.record_types];
		cmdb = s.cmdb;
	}

	onMount(async () => {
		try {
			take(await api<Settings>('/api/v1/settings/history'));
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load the history settings';
		}
	});

	const chosen = $derived(all ? ['*'] : [...types].sort());
	const dirty = $derived(!!current && (JSON.stringify(chosen) !== JSON.stringify([...current.record_types].sort()) || cmdb !== current.cmdb));

	async function save(e: Event) {
		e.preventDefault();
		saving = true;
		error = '';
		try {
			take(await api<Settings>('/api/v1/settings/history', { method: 'PUT', body: { record_types: chosen, cmdb } }));
			ui.announce = 'History settings saved';
		} catch (err) {
			error = err instanceof Error ? err.message : 'The settings didn’t save';
		} finally {
			saving = false;
		}
	}

	function toggle(type: string, on: boolean) {
		types = on ? [...types, type] : types.filter((t) => t !== type);
	}

	const label = (type: string) => current?.record_type_options.find((o) => o.type === type)?.label ?? type;
	function describe(setting: string, v: unknown): string {
		if (v === null || v === undefined) return 'not set';
		if (setting === 'history.cmdb') return v ? 'on' : 'off';
		if (Array.isArray(v)) return v.includes('*') ? 'every record type' : v.length ? v.map((t) => label(String(t))).join(', ') : 'no record type';
		return String(v);
	}
	const settingName = (s: string) => (s === 'history.cmdb' ? 'Configuration items' : 'Record types');
</script>

<svelte:head><title>History – ServiceLater</title></svelte:head>

<div class="page">
	<h1>History</h1>
	<p class="lede">
		What is kept, field by field, of what happens to records and configuration items: who changed what, from what to what.
	</p>

	{#if error}<p class="error" role="alert">{error}</p>{/if}

	{#if current}
		<form onsubmit={save} aria-labelledby="what-h">
			<h2 id="what-h">What is recorded</h2>
			<p class="note">
				Always recorded, whatever is chosen here: when a record is created, and every change of its status, assignee and
				assignment group; when an item is created or deleted, and its status. Turning something off only leaves out the
				other fields, from then on.
			</p>
			<fieldset>
				<legend>Records</legend>
				<label class="check"><input type="checkbox" bind:checked={all} /> Every record type, including ones added later</label>
				{#if !all}
					<div class="types">
						{#each current.record_type_options as o (o.type)}
							<label class="check">
								<input type="checkbox" checked={types.includes(o.type)} onchange={(e) => toggle(o.type, e.currentTarget.checked)} />
								{o.label}
							</label>
						{/each}
					</div>
				{/if}
			</fieldset>
			<fieldset>
				<legend>Configuration items</legend>
				<label class="check"><input type="checkbox" bind:checked={cmdb} /> Every field of every item</label>
			</fieldset>
			<div><button class="btn primary" disabled={!dirty || saving}>Save</button></div>
		</form>

		<section aria-labelledby="left-out-h">
			<h2 id="left-out-h">Fields apps leave out</h2>
			{#if current.exclusions.length === 0}
				<p class="hint">None. A form or class field marked <code>history: false</code> (in the app, or in its local layer) is listed here.</p>
			{:else}
				<ul class="rows">
					{#each current.exclusions as x (x.app + x.subject + x.kind + x.field)}
						<li>
							<span><strong>{x.label}</strong> on {x.subject === 'task' ? `${label(x.kind)} records` : `${x.kind} items`}</span>
							<span class="hint">{x.app}</span>
						</li>
					{/each}
				</ul>
			{/if}
		</section>

		<section aria-labelledby="log-h">
			<h2 id="log-h">Changes to these settings</h2>
			{#if current.log.length === 0}
				<p class="hint">Never changed: everything has been recorded.</p>
			{:else}
				<ul class="rows">
					{#each current.log as l, i (i)}
						<li>
							<span>
								<strong>{l.actor_name ?? 'ServiceLater'}</strong> set {settingName(l.setting)} to {describe(l.setting, l.new)}
								{#if l.old !== null && l.old !== undefined}<span class="hint">(was {describe(l.setting, l.old)})</span>{/if}
							</span>
							<time class="hint" datetime={l.at}>{when(l.at)}</time>
						</li>
					{/each}
				</ul>
			{/if}
		</section>
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 44rem;
	}
	h1 {
		font: 800 1.6rem/1.2 var(--font-display);
	}
	h2 {
		font: 700 1.25rem/1.2 var(--font-display);
	}
	.lede {
		margin: 0.25rem 0 1rem;
		color: var(--ink-2);
	}
	form,
	section {
		display: grid;
		gap: 0.75rem;
		padding: 1.25rem 0;
		border-top: 1px solid var(--rule);
	}
	.note {
		padding: 0.6rem 0.75rem;
		border-left: 3px solid var(--blue);
		background: var(--raised);
		font-size: 0.9rem;
	}
	fieldset {
		display: grid;
		gap: 0.45rem;
		margin: 0;
		padding: 0;
		border: 0;
	}
	legend {
		margin-bottom: 0.35rem;
		font-weight: 700;
		font-size: 0.9rem;
	}
	.types {
		display: grid;
		gap: 0.4rem;
		padding-left: 1.6rem;
	}
	.check {
		display: flex;
		align-items: center;
		gap: 0.5rem;
	}
	.rows {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.rows li {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		justify-content: space-between;
		gap: 0.25rem 1rem;
		padding: 0.5rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.hint {
		color: var(--ink-2);
		font-size: 0.85rem;
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
</style>
