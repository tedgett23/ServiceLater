<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import { when } from '$lib/format';
	import { realtime } from '$lib/realtime.svelte';
	import type { EmailDelivery, EmailSettings } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	let current = $state<EmailSettings | null>(null);
	let draft = $state<EmailSettings | null>(null);
	/** A new password to store; empty keeps the stored one. */
	let password = $state('');
	let clearPassword = $state(false);
	let loadError = $state('');
	let saving = $state(false);
	let saveError = $state('');

	let testTo = $state('');
	let testing = $state(false);
	let testResult = $state<{ status: string; detail?: string } | null>(null);

	let deliveries = $state<EmailDelivery[]>([]);
	/** The settings themselves, as PUT takes them; the rest of EmailSettings is about the password. */
	const settingKeys = ['enabled', 'host', 'port', 'tls', 'username', 'from_address', 'from_name', 'base_url', 'approvals'] as const;

	async function load() {
		try {
			current = await api<EmailSettings>('/api/v1/settings/email');
			draft = { ...current };
			loadError = '';
		} catch (e) {
			loadError = e instanceof Error ? e.message : 'Could not load the email settings';
		}
	}
	async function loadDeliveries() {
		try {
			deliveries = (await api<{ items: EmailDelivery[] }>('/api/v1/settings/email/deliveries')).items;
		} catch {
			/* keep what is shown */
		}
	}

	onMount(() => {
		void load();
		void loadDeliveries();
		// Emails go out from the worker: the log follows as each is queued, sent or fails.
		return realtime.on((e) => {
			if (e.type === 'email.changed' || e.type === 'resync') void loadDeliveries();
		});
	});

	async function save(e: Event) {
		e.preventDefault();
		if (!draft || saving) return;
		saving = true;
		saveError = '';
		const body: Record<string, unknown> = Object.fromEntries(settingKeys.map((k) => [k, draft![k]]));
		body.port = Number(draft.port);
		if (clearPassword) body.password = null;
		else if (password) body.password = password;
		try {
			current = await api<EmailSettings>('/api/v1/settings/email', { method: 'PUT', body });
			draft = { ...current };
			password = '';
			clearPassword = false;
			ui.announce = 'Email settings saved';
		} catch (err) {
			saveError = err instanceof Error ? err.message : 'The settings didn’t save';
		} finally {
			saving = false;
		}
	}

	async function test(e: Event) {
		e.preventDefault();
		testing = true;
		testResult = null;
		try {
			testResult = await api<{ status: string; detail?: string }>('/api/v1/settings/email/test', {
				method: 'POST',
				body: { to: testTo.trim() }
			});
		} catch (err) {
			testResult = { status: 'failed', detail: err instanceof Error ? err.message : 'Could not send' };
		} finally {
			testing = false;
		}
	}

	const dirty = $derived(!!draft && !!current && (JSON.stringify(draft) !== JSON.stringify(current) || !!password || clearPassword));
	const statusLabel: Record<EmailDelivery['status'], string> = { queued: 'Queued', sent: 'Sent', failed: 'Failed', skipped: 'Not sent' };
</script>

<svelte:head><title>Email – ServiceLater</title></svelte:head>

<div class="page">
	<h1>Email</h1>
	<p class="lede">Notifications whose template asks for it are also emailed, to people who have not turned email off.</p>

	{#if loadError}
		<p class="error" role="alert">{loadError}</p>
	{:else if draft && current}
		{#if !current.key_configured}
			<p class="warn" role="note">
				This server has no secrets key, so it cannot store a password. Set <code>SL_SECRETS_KEY</code> or
				<code>SL_SECRETS_KEY_FILE</code> on every server (<code>servicelater secrets gen-key</code> makes one) and restart.
			</p>
		{:else if current.password_set && !current.password_readable}
			<p class="warn" role="note">
				The stored password was encrypted with a key this server does not have, so it cannot sign in. Enter the password again.
			</p>
		{/if}

		<form class="settings" onsubmit={save} aria-labelledby="server-h">
			<h2 id="server-h">Mail server</h2>
			<label class="check wide"><input type="checkbox" bind:checked={draft.enabled} /> Send email notifications</label>
			<label class="field">
				<span>Host</span>
				<input bind:value={draft.host} placeholder="smtp.example.com" maxlength="253" />
			</label>
			<label class="field">
				<span>Port</span>
				<input type="number" bind:value={draft.port} min="1" max="65535" />
			</label>
			<label class="field">
				<span>Encryption</span>
				<select bind:value={draft.tls}>
					<option value="starttls">STARTTLS (usually port 587)</option>
					<option value="tls">TLS (usually port 465)</option>
					<option value="none">None (a relay on this server only)</option>
				</select>
			</label>
			<label class="field">
				<span>Username</span>
				<input bind:value={draft.username} autocomplete="off" placeholder="Leave empty to send without signing in" />
			</label>
			<div class="field">
				<label for="smtp-password">Password</label>
				<input
					id="smtp-password"
					type="password"
					bind:value={password}
					autocomplete="new-password"
					disabled={!current.key_configured || clearPassword}
					placeholder={current.password_set ? 'Stored: type a new one to replace it' : ''}
					aria-describedby="password-help"
				/>
				<small class="help" id="password-help">
					{#if current.password_set}
						Stored encrypted{current.password_updated_by ? `, set by ${current.password_updated_by}` : ''}{current.password_updated_at
							? ` on ${when(current.password_updated_at)}`
							: ''}. It is never shown again.
					{:else}
						Stored encrypted, and never shown again.
					{/if}
				</small>
			</div>
			{#if current.password_set}
				<!-- Outside the field above: .field input makes every input there full width, a checkbox too. -->
				<label class="check remove"><input type="checkbox" bind:checked={clearPassword} /> Remove the stored password</label>
			{/if}
			<label class="field">
				<span>From address</span>
				<input type="email" bind:value={draft.from_address} placeholder="servicedesk@example.com" />
			</label>
			<label class="field">
				<span>From name</span>
				<input bind:value={draft.from_name} maxlength="100" />
			</label>
			<label class="field wide">
				<span>Link address</span>
				<input type="url" bind:value={draft.base_url} placeholder="https://servicelater.example.com" />
				<small class="help">Where people open ServiceLater, for the links in emails.</small>
			</label>
			<label class="check wide"><input type="checkbox" bind:checked={draft.approvals} /> Email people when a workflow asks them to approve something</label>
			{#if saveError}<p class="error wide" role="alert">{saveError}</p>{/if}
			<div class="wide"><button class="btn primary" disabled={!dirty || saving}>Save</button></div>
		</form>

		<form class="test" onsubmit={test} aria-labelledby="test-h">
			<h2 id="test-h">Send a test email</h2>
			<p class="hint">Uses the saved settings, whether email notifications are on or not.</p>
			<div class="row">
				<label class="field grow">
					<span>To</span>
					<input type="email" bind:value={testTo} placeholder="Your own address" />
				</label>
				<button class="btn" disabled={testing || dirty} title={dirty ? 'Save the settings first' : undefined}>Send test email</button>
			</div>
			<div role="status">
				{#if testing}<p class="hint">Sending…</p>{/if}
				{#if testResult}
					<p class:ok={testResult.status === 'sent'} class:error={testResult.status !== 'sent'}>
						{testResult.status === 'sent' ? 'Sent. Check the inbox.' : `Not sent: ${testResult.detail}`}
					</p>
				{/if}
			</div>
		</form>

		<section aria-labelledby="log-h">
			<h2 id="log-h">Recent emails</h2>
			{#if deliveries.length === 0}
				<p class="hint">No emails yet.</p>
			{:else}
				<ul class="log">
					{#each deliveries as d (d.id)}
						<li>
							<span class="status {d.status}">{statusLabel[d.status]}</span>
							<span class="what">
								<strong>{d.user_name ?? d.to_address}</strong>
								<span class="hint">{d.kind === 'test' ? 'Test email' : d.what} · {d.to_address}</span>
								{#if d.detail}<span class="detail">{d.detail}</span>{/if}
							</span>
							<time datetime={d.updated_at}>{when(d.updated_at)}</time>
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
		max-width: 48rem;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
	}
	h2 {
		font: 650 1.0625rem/1.3 var(--font-display);
	}
	.lede {
		margin: 0.25rem 0 1rem;
		color: var(--ink-2);
	}
	form,
	section {
		padding: 1.25rem 0;
		border-top: 1px solid var(--rule);
	}
	.settings {
		display: grid;
		grid-template-columns: repeat(2, minmax(0, 1fr));
		gap: 0.9rem 1rem;
		align-items: start;
	}
	.settings h2,
	.wide {
		grid-column: 1 / -1;
	}
	.test {
		display: grid;
		gap: 0.6rem;
	}
	.row {
		display: flex;
		flex-wrap: wrap;
		align-items: end;
		gap: 0.75rem;
	}
	.grow {
		flex: 1 1 14rem;
	}
	.check {
		display: flex;
		align-items: center;
		gap: 0.5rem;
	}
	.remove {
		align-self: end;
		min-height: 2.5rem;
	}
	.help,
	.hint {
		color: var(--ink-2);
		font-size: 0.85rem;
	}
	.warn {
		padding: 0.6rem 0.75rem;
		border-left: 3px solid var(--marker);
		background: var(--raised);
		margin-bottom: 1rem;
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
	.ok {
		font-weight: 600;
	}
	.log {
		list-style: none;
		margin: 0.5rem 0 0;
		padding: 0;
	}
	.log li {
		display: grid;
		grid-template-columns: 5.5rem minmax(0, 1fr) auto;
		gap: 0.75rem;
		align-items: baseline;
		padding: 0.55rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.what {
		display: grid;
		gap: 0.1rem;
		min-width: 0;
		overflow-wrap: anywhere;
	}
	.detail {
		font-size: 0.85rem;
	}
	.status {
		font-weight: 700;
		font-size: 0.85rem;
	}
	.status.failed {
		color: var(--red);
	}
	.status.queued,
	.status.skipped {
		color: var(--ink-2);
	}
	time {
		color: var(--ink-2);
		font-size: 0.85rem;
		white-space: nowrap;
	}
	@media (max-width: 40rem) {
		.settings {
			grid-template-columns: minmax(0, 1fr);
		}
		.log li {
			grid-template-columns: minmax(0, 1fr) auto;
		}
		.log .what {
			grid-column: 1 / -1;
			grid-row: 2;
		}
	}
</style>
