<script lang="ts">
	import { api } from '$lib/api';
	import { when } from '$lib/format';
	import { realtime } from '$lib/realtime.svelte';
	import { ui } from '$lib/ui.svelte';
	import type { Webhook, WebhookDelivery, WorkflowDef } from './workflow';

	/**
	 * The workflow panel's webhook: whether another system may start the workflow, how it proves itself (a token, or a
	 * signature of the body), the address to give it, whether the secret is set, and the latest calls.
	 */
	let {
		def,
		webhook,
		editable,
		onchange
	}: {
		def: WorkflowDef;
		webhook: Webhook | null;
		editable: boolean;
		onchange: (h: Webhook | null) => void;
	} = $props();

	const secrets = $derived(def.app_secrets ?? []);
	const url = $derived(`${location.origin}/api/v1/hooks/${encodeURIComponent(def.app)}/${encodeURIComponent(def.name)}`);
	/** The saved webhook: what the server answers to now (the form may hold unsaved changes). */
	const saved = $derived(def.webhook ?? null);

	let deliveries = $state<WebhookDelivery[] | null>(null);
	let copied = $state(false);

	async function load() {
		if (!saved) {
			deliveries = null;
			return;
		}
		try {
			deliveries = await api<WebhookDelivery[]>(`/api/v1/workflows/${encodeURIComponent(def.app)}/${encodeURIComponent(def.name)}/deliveries`);
		} catch {
			deliveries = null; // those who may edit but not read every run do not see the log
		}
	}

	$effect(() => {
		void saved;
		void load();
	});
	// A call that starts a run shows at once; refused ones on Refresh (they change nothing anyone is told about).
	$effect(() =>
		realtime.on((e) => {
			if ((e.type === 'run.changed' && e.app === def.app && e.workflow === def.name) || e.type === 'resync') void load();
		})
	);

	function set(patch: Partial<Webhook>) {
		if (!webhook) return;
		const next: Webhook = { ...webhook, ...patch };
		for (const k of ['signature_header', 'signature_prefix', 'dedupe_header'] as const) if (!next[k]) delete next[k];
		if (next.auth === 'token') {
			delete next.signature_header;
			delete next.signature_prefix;
		}
		onchange(next);
	}

	async function copy() {
		try {
			await navigator.clipboard.writeText(url);
			copied = true;
			ui.announce = 'Address copied';
		} catch {
			/* the address is selectable text as well */
		}
	}

	const outcomes: Record<WebhookDelivery['outcome'], string> = { started: 'Started a run', duplicate: 'Repeat, nothing started', refused: 'Refused' };
</script>

<fieldset class="hook">
	<legend>Webhook</legend>
	<label class="check">
		<input type="checkbox" checked={!!webhook} disabled={!editable || (!webhook && secrets.length === 0)}
			onchange={(e) => onchange(e.currentTarget.checked ? { secret: secrets[0] ?? '', auth: 'token' } : null)} />
		Also start it when another system calls it
	</label>
	{#if !webhook && secrets.length === 0}
		<p class="hint">The {def.app} app declares no secrets. A webhook needs one: add it to the app's manifest config with type: secret.</p>
	{/if}
	{#if webhook}
		<div class="field">
			<label for="wh-auth">The caller proves itself with</label>
			<select id="wh-auth" value={webhook.auth} disabled={!editable} onchange={(e) => set({ auth: e.currentTarget.value as Webhook['auth'] })}>
				<option value="token">A token (Authorization: Bearer …)</option>
				<option value="hmac">A signature of the body (HMAC-SHA256)</option>
			</select>
		</div>
		<div class="field">
			<label for="wh-secret">Secret</label>
			<select id="wh-secret" value={webhook.secret} disabled={!editable} onchange={(e) => set({ secret: e.currentTarget.value })}>
				{#each secrets as s (s)}<option value={s}>{s}</option>{/each}
			</select>
		</div>
		{#if webhook.auth === 'hmac'}
			<div class="field">
				<label for="wh-sig">Signature header</label>
				<input id="wh-sig" value={webhook.signature_header ?? ''} disabled={!editable} placeholder="X-Signature-256" onchange={(e) => set({ signature_header: e.currentTarget.value.trim() })} />
			</div>
			<div class="field">
				<label for="wh-prefix">Before the signature</label>
				<input id="wh-prefix" value={webhook.signature_prefix ?? ''} disabled={!editable} placeholder="sha256=" onchange={(e) => set({ signature_prefix: e.currentTarget.value.trim() })} />
				<small class="hint">GitHub: X-Hub-Signature-256 with sha256=. A signature without it is accepted too.</small>
			</div>
		{/if}
		<div class="field">
			<label for="wh-dedupe">Repeat header</label>
			<input id="wh-dedupe" value={webhook.dedupe_header ?? ''} disabled={!editable} placeholder="Idempotency-Key" onchange={(e) => set({ dedupe_header: e.currentTarget.value.trim() })} />
			<small class="hint">A call with the same value as an earlier one starts nothing (GitHub: X-GitHub-Delivery).</small>
		</div>
		<div class="field">
			<label for="wh-url">Address to give the other system</label>
			<div class="url">
				<input id="wh-url" class="mono" value={url} readonly onfocus={(e) => e.currentTarget.select()} />
				<button type="button" class="btn" onclick={copy}>{copied ? 'Copied' : 'Copy'}</button>
			</div>
			<small class="hint">POST, with a JSON body: the run reads it as vars.payload, acts as the {def.app} app for nobody, with no record.</small>
		</div>
		{#if saved && webhook.secret === saved.secret}
			{#if def.webhook_secret_set}
				<p class="hint">Its secret is set.</p>
			{:else}
				<p class="warn">Its secret is not set yet, so every call is refused. An admin sets {saved.secret} for {def.app} under <a href="/admin/connections">Admin → Connections</a>.</p>
			{/if}
		{:else}
			<p class="hint">Save to start answering at this address.</p>
		{/if}
		{#if deliveries}
			<section aria-labelledby="wh-log">
				<div class="log-head">
					<h3 id="wh-log">Latest calls</h3>
					<button type="button" class="btn" onclick={load}>Refresh</button>
				</div>
				{#if deliveries.length === 0}
					<p class="hint">No calls yet.</p>
				{:else}
					<ul class="log">
						{#each deliveries.slice(0, 10) as d (d.id)}
							<li class:refused={d.outcome === 'refused'}>
								<span>{#if d.run_id}<a href="/runs/{d.run_id}">{outcomes[d.outcome]}</a>{:else}{outcomes[d.outcome]}{/if}</span>
								<span class="hint">{when(d.received_at)}{d.remote ? ` from ${d.remote}` : ''}</span>
								{#if d.reason}<span class="hint">{d.reason}</span>{/if}
							</li>
						{/each}
					</ul>
				{/if}
			</section>
		{/if}
	{/if}
</fieldset>

<style>
	.hook {
		display: grid;
		gap: 0.55rem;
		margin: 0.4rem 0 0;
		padding: 0.6rem 0 0;
		border: 0;
		border-top: 1px solid var(--rule);
		min-width: 0;
	}
	legend {
		padding: 0;
		font-size: 0.85rem;
		font-weight: 700;
	}
	h3 {
		margin: 0.2rem 0 0.3rem;
		font-size: 0.85rem;
	}
	.check {
		display: flex;
		align-items: center;
		gap: 0.45rem;
		font-size: 0.875rem;
	}
	.hint {
		color: var(--ink-2);
		font-size: 0.85rem;
	}
	.warn {
		margin: 0;
		padding: 0.45rem 0.6rem;
		border-left: 3px solid var(--marker);
		background: var(--marker-soft);
		color: var(--marker-ink);
		font-size: 0.85rem;
	}
	.url {
		display: flex;
		gap: 0.4rem;
		min-width: 0;
	}
	.url input {
		flex: 1;
		min-width: 0;
	}
	.mono {
		font-family: var(--font-mono);
		font-size: 0.8rem;
	}
	.log-head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.5rem;
	}
	.log {
		display: grid;
		gap: 0.4rem;
		margin: 0;
		padding: 0;
		list-style: none;
	}
	.log li {
		display: grid;
		gap: 0.1rem;
		padding-left: 0.5rem;
		border-left: 2px solid var(--rule);
		font-size: 0.85rem;
	}
	.log li.refused {
		border-left-color: var(--red);
	}
</style>
