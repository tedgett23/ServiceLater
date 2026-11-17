<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import { when } from '$lib/format';
	import { ui } from '$lib/ui.svelte';

	/** How incoming mail is read and what new mail may do (GET /settings/inbound). Passwords are never sent back. */
	interface Inbound {
		enabled: boolean;
		mode: 'imap' | 'graph';
		address: string;
		host: string;
		port: number;
		tls: 'tls' | 'starttls' | 'none';
		username: string;
		tenant_id: string;
		client_id: string;
		mailbox: string;
		graph_base: string;
		login_base: string;
		create_incidents: boolean;
		allowed_domains: string[];
		create_accounts: boolean;
		trusted_authserv: string;
		max_per_hour: number;
		password_set: boolean;
		graph_secret_set: boolean;
		key_configured: boolean;
	}
	interface Received {
		id: string;
		from: string;
		subject: string;
		action: string;
		detail: string;
		reference?: string;
		attachments: number;
		authentication: string;
		created_at: string;
	}

	let s = $state<Inbound | null>(null);
	let domains = $state('');
	let password = $state('');
	let graphSecret = $state('');
	let log = $state<Received[]>([]);
	let error = $state('');
	let saveError = $state('');
	let busy = $state(false);
	let test = $state<{ status: string; detail: string } | null>(null);

	async function load() {
		try {
			s = await api<Inbound>('/api/v1/settings/inbound');
			domains = s.allowed_domains.join(', ');
			await loadLog();
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load the incoming mail settings';
		}
	}
	async function loadLog() {
		try {
			log = (await api<{ items: Received[] }>('/api/v1/settings/inbound/log')).items;
		} catch {
			/* keep what is shown */
		}
	}
	onMount(() => {
		void load();
		const t = setInterval(() => void loadLog(), 15000);
		return () => clearInterval(t);
	});

	async function save(e: Event) {
		e.preventDefault();
		if (!s) return;
		busy = true;
		saveError = '';
		const { password_set: _p, graph_secret_set: _g, key_configured: _k, ...settings } = s;
		const body: Record<string, unknown> = { ...settings, allowed_domains: domains.split(/[\s,]+/).filter(Boolean) };
		if (password) body.password = password;
		if (graphSecret) body.graph_secret = graphSecret;
		try {
			s = await api<Inbound>('/api/v1/settings/inbound', { method: 'PUT', body });
			domains = s.allowed_domains.join(', ');
			password = graphSecret = '';
			ui.announce = 'Incoming mail settings saved';
		} catch (err) {
			saveError = err instanceof Error ? err.message : 'That didn’t save';
		} finally {
			busy = false;
		}
	}

	async function runTest() {
		busy = true;
		test = null;
		try {
			test = await api<{ status: string; detail: string }>('/api/v1/settings/inbound/test', { method: 'POST' });
		} catch (err) {
			test = { status: 'failed', detail: err instanceof Error ? err.message : 'The test did not run' };
		} finally {
			busy = false;
		}
	}

	const actionLabel: Record<string, string> = {
		comment: 'Comment',
		incident: 'New incident',
		ignored: 'Ignored',
		refused: 'Refused',
		failed: 'Failed'
	};
</script>

<svelte:head><title>Incoming email – ServiceLater</title></svelte:head>

<div class="page">
	<h1>Incoming email</h1>
	<p class="lede">
		Replies to notification emails become comments on their record, from the person the email was sent to. Optionally, new email to
		the same mailbox files an incident, when the sender’s domain is allowed and vouches for the message (DMARC).
	</p>
	{#if error}<p class="error" role="alert">{error}</p>{/if}

	{#if s}
		<form class="block" onsubmit={save} aria-label="Incoming email settings">
			<h2>Mailbox</h2>
			<label class="check"><input type="checkbox" bind:checked={s.enabled} /> Read this mailbox every minute</label>
			<label class="field">
				<span>Mailbox address</span>
				<input bind:value={s.address} type="email" placeholder="servicedesk@example.com" autocomplete="off" />
			</label>
			<p class="hint">Notification emails ask for replies here. Mail from this address itself is never read as a request.</p>
			<div class="segmented" role="group" aria-label="Read it with">
				<button type="button" aria-pressed={s.mode === 'imap'} onclick={() => (s!.mode = 'imap')}>IMAP</button>
				<button type="button" aria-pressed={s.mode === 'graph'} onclick={() => (s!.mode = 'graph')}>Microsoft 365</button>
			</div>

			{#if s.mode === 'imap'}
				<div class="pair">
					<label class="field">
						<span>IMAP server</span>
						<input bind:value={s.host} placeholder="imap.gmail.com" autocomplete="off" />
					</label>
					<div class="pair">
						<label class="field">
							<span>Port</span>
							<input bind:value={s.port} type="number" min="1" max="65535" />
						</label>
						<label class="field">
							<span>Encryption</span>
							<select bind:value={s.tls}>
								<option value="tls">TLS</option>
								<option value="starttls">STARTTLS</option>
								<option value="none">None (this machine only)</option>
							</select>
						</label>
					</div>
				</div>
				<div class="pair">
					<label class="field">
						<span>Username</span>
						<input bind:value={s.username} autocomplete="off" />
					</label>
					<label class="field">
						<span>Password</span>
						<input type="password" bind:value={password} autocomplete="new-password"
							placeholder={s.password_set ? 'Stored: leave empty to keep it' : 'The password or an app password'} />
					</label>
				</div>
			{:else}
				<p class="hint">
					Register an app in Microsoft Entra with the application permission Mail.ReadWrite (limit it to this mailbox with an
					application access policy), and give it a client secret.
				</p>
				<div class="pair">
					<label class="field">
						<span>Tenant ID</span>
						<input bind:value={s.tenant_id} autocomplete="off" placeholder="00000000-0000-0000-0000-000000000000" />
					</label>
					<label class="field">
						<span>Client ID</span>
						<input bind:value={s.client_id} autocomplete="off" />
					</label>
				</div>
				<div class="pair">
					<label class="field">
						<span>Client secret</span>
						<input type="password" bind:value={graphSecret} autocomplete="new-password"
							placeholder={s.graph_secret_set ? 'Stored: leave empty to keep it' : ''} />
					</label>
					<label class="field">
						<span>Mailbox to read</span>
						<input bind:value={s.mailbox} autocomplete="off" placeholder="servicedesk@example.com" />
					</label>
				</div>
				<details>
					<summary>National clouds</summary>
					<div class="pair">
						<label class="field">
							<span>Graph address</span>
							<input bind:value={s.graph_base} autocomplete="off" />
						</label>
						<label class="field">
							<span>Sign-in address</span>
							<input bind:value={s.login_base} autocomplete="off" />
						</label>
					</div>
				</details>
			{/if}
			{#if !s.key_configured}<p class="warn">This server has no secrets key, so it cannot store the password.</p>{/if}

			<h2>New email</h2>
			<label class="check">
				<input type="checkbox" bind:checked={s.create_incidents} /> File an incident from new email (not a reply)
			</label>
			<label class="field">
				<span>Allowed sender domains</span>
				<input bind:value={domains} placeholder="example.com, example.org" autocomplete="off" />
			</label>
			<p class="hint">
				Only these domains may file incidents, and only mail their DMARC vouches for (an aligned DKIM signature, checked here). Mail
				from anywhere else is refused and logged below.
			</p>
			<div class="pair">
				<label class="field">
					<span>Incidents per sender per hour</span>
					<input bind:value={s.max_per_hour} type="number" min="1" max="1000" />
				</label>
				<label class="field">
					<span>Trusted receiving server</span>
					<input bind:value={s.trusted_authserv} placeholder="mx.google.com (optional)" autocomplete="off" />
				</label>
			</div>
			<p class="hint">
				Set the trusted server to accept its Authentication-Results verdict for mail without a DKIM signature ServiceLater can check
				(senders who rely on SPF alone).
			</p>
			<label class="check">
				<input type="checkbox" bind:checked={s.create_accounts} /> Give senders from allowed domains without an account one (as end users)
			</label>

			<div class="row">
				<button class="btn primary" disabled={busy}>Save</button>
				<button class="btn" type="button" disabled={busy} onclick={runTest}>Test the connection</button>
			</div>
			{#if saveError}<p class="error" role="alert">{saveError}</p>{/if}
			{#if test}
				<p class={test.status === 'ok' ? 'ok' : 'warn'} role="status">{test.status === 'ok' ? 'Connected.' : 'Not connected:'} {test.detail}</p>
			{/if}
		</form>

		<section class="block" aria-labelledby="log-h">
			<h2 id="log-h">Received</h2>
			{#if log.length === 0}
				<p class="hint">Nothing read yet.</p>
			{:else}
				<table>
					<thead><tr><th>When</th><th>From</th><th>Subject</th><th>Became</th></tr></thead>
					<tbody>
						{#each log as m (m.id)}
							<tr>
								<td class="when">{when(m.created_at)}</td>
								<td class="from">{m.from}</td>
								<td class="subject">{m.subject}</td>
								<td class="became">
									<span class="action {m.action}">{actionLabel[m.action] ?? m.action}</span>
									{#if m.reference}<a href="/tickets/{m.reference}">{m.reference}</a>{/if}
									{#if m.attachments}<span class="detail">{m.attachments} file{m.attachments === 1 ? '' : 's'}</span>{/if}
									<span class="detail">{m.detail}</span>
									{#if m.authentication}<span class="detail">{m.authentication}</span>{/if}
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			{/if}
		</section>
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 52rem;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
	}
	h2 {
		font: 650 1.0625rem/1.3 var(--font-display);
		margin-top: 0.5rem;
	}
	.lede {
		margin: 0.25rem 0 0.5rem;
		color: var(--ink-2);
	}
	.block {
		display: grid;
		gap: 0.75rem;
		margin-top: 1.5rem;
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
	}
	.pair {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: 0.75rem;
	}
	.row {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
	}
	.check {
		display: flex;
		align-items: center;
		gap: 0.45rem;
	}
	.hint {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.warn {
		padding: 0.45rem 0.6rem;
		border-left: 3px solid var(--marker);
		background: var(--marker-soft);
		font-size: 0.875rem;
	}
	.ok {
		color: var(--green);
		font-weight: 600;
	}
	summary {
		cursor: pointer;
		font-size: 0.875rem;
		color: var(--ink-2);
	}
	table {
		width: 100%;
		border-collapse: collapse;
		font-size: 0.875rem;
	}
	th {
		text-align: left;
		font-weight: 600;
		color: var(--ink-2);
		padding: 0.35rem 0.4rem;
		border-bottom: 1px solid var(--rule);
	}
	td {
		padding: 0.45rem 0.4rem;
		border-bottom: 1px solid var(--rule);
		vertical-align: top;
		overflow-wrap: anywhere;
	}
	.action {
		font-weight: 600;
		margin-right: 0.35rem;
	}
	.action.refused,
	.action.failed {
		color: var(--red);
	}
	.action.comment,
	.action.incident {
		color: var(--green);
	}
	.detail {
		display: block;
		color: var(--ink-2);
		font-size: 0.8125rem;
	}
	.error {
		padding: 0.5rem 0.75rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
		color: var(--red);
		font-weight: 600;
	}
	@media (max-width: 40rem) {
		.pair {
			grid-template-columns: 1fr;
		}
		/* On a phone each email is a card: when and from on one line, then the subject and what it became. */
		thead {
			display: none;
		}
		tr {
			display: grid;
			grid-template-columns: auto 1fr;
			column-gap: 0.6rem;
			padding: 0.6rem 0;
			border-bottom: 1px solid var(--rule);
		}
		td {
			padding: 0.1rem 0;
			border: 0;
		}
		td.when {
			color: var(--ink-2);
		}
		td.subject,
		td.became {
			grid-column: 1 / -1;
		}
		td.subject {
			font-weight: 600;
		}
	}
</style>
