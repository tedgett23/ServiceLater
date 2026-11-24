<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import { ui } from '$lib/ui.svelte';

	type Limits = {
		api_people_per_minute: number;
		api_keys_per_minute: number;
		sign_in_attempts: number;
		sign_in_window_minutes: number;
		webhook_per_minute: number;
		queries_per_hour: number;
		queries_at_once: number;
		sms_codes_per_hour: number;
		live_connections: number;
	};
	type Name = keyof Limits;
	type Field = { name: Name; label: string; unit: string; min: number; max: number; off?: boolean };

	/** The page's sections, each limit with what it counts and the range the server accepts. */
	const sections: { id: string; title: string; note: string; fields: Field[] }[] = [
		{
			id: 'api',
			title: 'API requests',
			note: 'Every request a signed-in person or an API key makes counts, the web app’s own included. Over the limit, a request is refused until the minute is up. Every server counts, and they share one count to within a second. A key can have its own limit, on its service account’s page.',
			fields: [
				{ name: 'api_people_per_minute', label: 'A person', unit: 'a minute', min: 10, max: 1_000_000, off: true },
				{ name: 'api_keys_per_minute', label: 'An API key', unit: 'a minute', min: 10, max: 1_000_000, off: true }
			]
		},
		{
			id: 'sign-in',
			title: 'Signing in',
			note: 'Counted per email address and place, right or wrong; past it, even the right password waits.',
			fields: [
				{ name: 'sign_in_attempts', label: 'Attempts', unit: 'tries', min: 3, max: 1000 },
				{ name: 'sign_in_window_minutes', label: 'In', unit: 'minutes', min: 1, max: 1440 }
			]
		},
		{
			id: 'webhooks',
			title: 'Webhooks',
			note: 'Calls that start workflows, refused ones included, so a secret cannot be guessed at speed.',
			fields: [{ name: 'webhook_per_minute', label: 'Calls to one webhook', unit: 'a minute', min: 1, max: 1_000_000, off: true }]
		},
		{
			id: 'queries',
			title: 'SQL queries',
			note: 'For those who may run queries in the workbench or through the API.',
			fields: [
				{ name: 'queries_per_hour', label: 'Queries a person may run', unit: 'an hour', min: 1, max: 100_000 },
				{ name: 'queries_at_once', label: 'Running at once', unit: 'per person, per server', min: 1, max: 20 }
			]
		},
		{
			id: 'people',
			title: 'People',
			note: 'Live connections are open tabs and devices getting updates; one past the limit is turned away.',
			fields: [
				{ name: 'sms_codes_per_hour', label: 'SMS confirmation codes', unit: 'an hour', min: 1, max: 50 },
				{ name: 'live_connections', label: 'Live connections', unit: 'per person, per server', min: 1, max: 100 }
			]
		}
	];
	const fields = sections.flatMap((s) => s.fields);

	let saved = $state<Limits | null>(null);
	let defaults = $state<Limits | null>(null);
	let form = $state<Limits | null>(null);
	let saving = $state(false);
	let error = $state('');

	function take(r: Limits & { defaults: Limits }) {
		const { defaults: d, ...l } = r;
		saved = l;
		defaults = d;
		form = { ...l };
	}

	onMount(async () => {
		try {
			take(await api<Limits & { defaults: Limits }>('/api/v1/settings/limits'));
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load the rate limits';
		}
	});

	const dirty = $derived(!!form && !!saved && fields.some((f) => form![f.name] !== saved![f.name]));
	const valid = (f: Field, v: number) => Number.isInteger(v) && ((f.off && v === 0) || (v >= f.min && v <= f.max));
	const allValid = $derived(!!form && fields.every((f) => valid(f, form![f.name])));
	const isDefault = $derived(!!form && !!defaults && fields.every((f) => form![f.name] === defaults![f.name]));

	async function save(e: Event) {
		e.preventDefault();
		if (!form) return;
		saving = true;
		error = '';
		try {
			take(await api<Limits & { defaults: Limits }>('/api/v1/settings/limits', { method: 'PUT', body: form }));
			ui.announce = 'Rate limits saved';
		} catch (err) {
			error = err instanceof Error ? err.message : 'The rate limits didn’t save';
		} finally {
			saving = false;
		}
	}

	const fmt = (n: number) => n.toLocaleString();
</script>

<svelte:head><title>Rate limits – ServiceLater</title></svelte:head>

<div class="page">
	<h1>Rate limits</h1>
	<p class="lede">How much one person, key or webhook may do, so that nobody, by mistake or on purpose, can slow everyone else down.</p>

	{#if error}<p class="error" role="alert">{error}</p>{/if}

	{#if form && defaults}
		<form onsubmit={save}>
			{#each sections as s (s.id)}
				<fieldset aria-describedby="{s.id}-note">
					<legend>{s.title}</legend>
					<p class="hint" id="{s.id}-note">{s.note}</p>
					<div class="grid">
						{#each s.fields as f (f.name)}
							{@const v = form[f.name]}
							<div class="limit">
								<label for="l-{f.name}">{f.label}</label>
								<div class="row">
									<input id="l-{f.name}" type="number" inputmode="numeric" min={f.off ? 0 : f.min} max={f.max} step="1" value={v === 0 && f.off ? '' : v}
										disabled={f.off && v === 0} aria-invalid={!valid(f, v)}
										oninput={(e) => (form![f.name] = e.currentTarget.value === '' ? NaN : Number(e.currentTarget.value))} />
									<span class="unit">{f.unit}</span>
								</div>
								{#if f.off}
									<label class="check">
										<input type="checkbox" checked={v === 0} onchange={(e) => (form![f.name] = e.currentTarget.checked ? 0 : defaults![f.name] || f.min)} />
										No limit
									</label>
								{/if}
								<small class="hint" class:bad={!valid(f, v)}>
									{#if !valid(f, v)}{fmt(f.min)} to {fmt(f.max)}{f.off ? ', or no limit' : ''}.{:else}Default {defaults[f.name] === 0 ? 'no limit' : fmt(defaults[f.name])}.{/if}
								</small>
							</div>
						{/each}
					</div>
				</fieldset>
			{/each}
			<div class="actions">
				<button class="btn primary" disabled={!dirty || !allValid || saving}>{saving ? 'Saving…' : 'Save'}</button>
				<button type="button" class="btn" disabled={isDefault} onclick={() => (form = { ...defaults! })}>Use the defaults</button>
			</div>
		</form>
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 48rem;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
	}
	.lede {
		margin: 0.25rem 0 1rem;
		color: var(--ink-2);
	}
	form {
		display: grid;
	}
	fieldset {
		display: grid;
		gap: 0.6rem;
		margin: 0;
		padding: 1.1rem 0;
		border: 0;
		border-top: 1px solid var(--rule);
		min-width: 0;
	}
	legend {
		float: left;
		width: 100%;
		padding: 0;
		font: 650 1.0625rem/1.3 var(--font-display);
	}
	.grid {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(14rem, 1fr));
		gap: 0.9rem 1.25rem;
	}
	.limit {
		display: grid;
		gap: 0.3rem;
		align-content: start;
	}
	.limit > label:not(.check) {
		font-size: 0.875rem;
		font-weight: 600;
	}
	.row {
		display: flex;
		align-items: center;
		gap: 0.5rem;
	}
	.row input {
		width: 8.5rem;
		min-height: 2.25rem;
		padding: 0.4rem 0.6rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--raised);
		color: var(--ink);
		font: inherit;
		font-variant-numeric: tabular-nums;
	}
	.row input:disabled {
		opacity: 0.55;
	}
	.row input[aria-invalid='true'] {
		border-color: var(--red);
	}
	.unit {
		color: var(--ink-2);
		font-size: 0.85rem;
	}
	.check {
		display: flex;
		align-items: center;
		gap: 0.45rem;
		font-size: 0.875rem;
	}
	.hint {
		margin: 0;
		color: var(--ink-2);
		font-size: 0.85rem;
	}
	.hint.bad {
		color: var(--red);
	}
	.actions {
		display: flex;
		gap: 0.5rem;
		padding-top: 1rem;
		border-top: 1px solid var(--rule);
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
</style>
