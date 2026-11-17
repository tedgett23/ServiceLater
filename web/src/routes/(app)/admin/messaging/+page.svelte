<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import { when } from '$lib/format';
	import { realtime } from '$lib/realtime.svelte';
	import { ui } from '$lib/ui.svelte';

	/** How text messages are sent: Twilio's Messages API. The auth token is never sent back. */
	interface SMS {
		enabled: boolean;
		account_sid: string;
		from: string;
		api_base: string;
		token_set: boolean;
		token_readable: boolean;
		key_configured: boolean;
	}
	interface Teams {
		enabled: boolean;
		key_configured: boolean;
		groups: { id: string; name: string; connected: boolean }[];
	}
	interface Delivery {
		id: string;
		channel: 'sms' | 'teams';
		to: string;
		user_name: string | null;
		kind: string;
		what: string;
		status: string;
		detail: string;
		created_at: string;
	}

	let sms = $state<SMS | null>(null);
	let token = $state('');
	let teams = $state<Teams | null>(null);
	let deliveries = $state<Delivery[]>([]);
	let error = $state('');
	let busy = $state(false);
	let testTo = $state('');
	let testResult = $state<{ status: string; detail?: string } | null>(null);
	let connecting = $state('');
	let webhook = $state('');

	async function load() {
		try {
			[sms, teams] = await Promise.all([api<SMS>('/api/v1/settings/sms'), api<Teams>('/api/v1/settings/teams')]);
			await loadDeliveries();
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load the messaging settings';
		}
	}
	async function loadDeliveries() {
		try {
			deliveries = (await api<{ items: Delivery[] }>('/api/v1/settings/messaging/deliveries')).items;
		} catch {
			/* keep what is shown */
		}
	}
	onMount(() => {
		void load();
		// Deliveries change as the worker sends them (the same event as email's log).
		return realtime.on((e) => {
			if (e.type === 'email.changed' || e.type === 'resync') void loadDeliveries();
		});
	});

	async function act(run: () => Promise<void>, done: string) {
		busy = true;
		error = '';
		try {
			await run();
			ui.announce = done;
			return true;
		} catch (e) {
			error = e instanceof Error ? e.message : 'That didn’t save';
			return false;
		} finally {
			busy = false;
		}
	}

	function saveSMS(e: Event) {
		e.preventDefault();
		if (!sms) return;
		const body: Record<string, unknown> = { enabled: sms.enabled, account_sid: sms.account_sid, from: sms.from, api_base: sms.api_base };
		if (token) body.auth_token = token;
		void act(async () => {
			sms = await api<SMS>('/api/v1/settings/sms', { method: 'PUT', body });
			token = '';
		}, 'SMS settings saved');
	}

	function testSMS(e: Event) {
		e.preventDefault();
		testResult = null;
		void act(async () => {
			testResult = await api<{ status: string; detail?: string }>('/api/v1/settings/sms/test', { method: 'POST', body: { to: testTo } });
			await loadDeliveries();
		}, 'Test sent');
	}

	function setTeams(enabled: boolean) {
		void act(async () => {
			teams = await api<Teams>('/api/v1/settings/teams', { method: 'PUT', body: { enabled } });
		}, enabled ? 'Teams posts on' : 'Teams posts off');
	}

	function connect(e: Event, id: string) {
		e.preventDefault();
		void act(async () => {
			await api(`/api/v1/groups/${id}/teams`, { method: 'PUT', body: { webhook: webhook.trim() } });
			webhook = '';
			connecting = '';
			teams = await api<Teams>('/api/v1/settings/teams');
			await loadDeliveries();
		}, 'Channel connected');
	}

	function disconnect(id: string) {
		void act(async () => {
			await api(`/api/v1/groups/${id}/teams`, { method: 'DELETE' });
			teams = await api<Teams>('/api/v1/settings/teams');
		}, 'Channel disconnected');
	}
</script>

<svelte:head><title>Messaging – ServiceLater</title></svelte:head>

<div class="page">
	<h1>Messaging</h1>
	<p class="lede">
		Text messages and Microsoft Teams, beside email. Each notification template says which channels it uses: approvals waiting and
		missed deadlines are texted, and a ticket routed to a team is posted in its channel.
	</p>
	{#if error}<p class="error" role="alert">{error}</p>{/if}

	{#if sms}
		<form class="block" onsubmit={saveSMS} aria-labelledby="sms-h">
			<h2 id="sms-h">Text messages (Twilio)</h2>
			<label class="check"><input type="checkbox" bind:checked={sms.enabled} /> Text notifications to people who turned it on</label>
			<p class="hint">People add and confirm their own mobile number on their account page, and can turn texts off there.</p>
			<div class="pair">
				<label class="field">
					<span>Account SID</span>
					<input bind:value={sms.account_sid} placeholder="AC…" autocomplete="off" spellcheck="false" />
				</label>
				<label class="field">
					<span>Auth token</span>
					<input type="password" bind:value={token} autocomplete="new-password"
						placeholder={sms.token_set ? (sms.token_readable ? 'Stored: leave empty to keep it' : 'Stored with another key: set it again') : ''} />
				</label>
			</div>
			<div class="pair">
				<label class="field">
					<span>Send from</span>
					<input bind:value={sms.from} placeholder="+15550100000 or a messaging service MG…" autocomplete="off" />
				</label>
				<label class="field">
					<span>API address</span>
					<input bind:value={sms.api_base} placeholder="https://api.twilio.com" autocomplete="off" />
				</label>
			</div>
			{#if !sms.key_configured}<p class="warn">This server has no secrets key, so the auth token cannot be stored.</p>{/if}
			<div><button class="btn primary" disabled={busy}>Save</button></div>
		</form>

		<form class="block" onsubmit={testSMS} aria-labelledby="smstest-h">
			<h2 id="smstest-h">Send a test text</h2>
			<div class="row">
				<label class="field grow">
					<span>To</span>
					<input bind:value={testTo} type="tel" placeholder="+15550101234" required />
				</label>
				<button class="btn" disabled={busy || !testTo.trim()}>Send test</button>
			</div>
			{#if testResult}
				<p class={testResult.status === 'sent' ? 'ok' : 'warn'} role="status">
					{testResult.status === 'sent' ? 'Sent.' : `Not sent: ${testResult.detail}`}
				</p>
			{/if}
		</form>
	{/if}

	{#if teams}
		<section class="block" aria-labelledby="teams-h">
			<h2 id="teams-h">Microsoft Teams</h2>
			<label class="check">
				<input type="checkbox" checked={teams.enabled} disabled={busy} onchange={(e) => setTeams(e.currentTarget.checked)} />
				Post notifications for a group in its Teams channel
			</label>
			<p class="hint">
				In the channel, add a workflow from the template “Post to a channel when a webhook request is received” and paste its address
				here. Posts carry the notification’s title and text, so everyone in the channel sees them: connect channels whose members may.
			</p>
			{#if teams.groups.length === 0}
				<p class="msg">No groups yet.</p>
			{/if}
			<ul class="groups">
				{#each teams.groups as g (g.id)}
					<li>
						<div class="gname">
							<span>{g.name}</span>
							<span class={g.connected ? 'on' : 'off'}>{g.connected ? 'Connected' : 'No channel'}</span>
						</div>
						{#if connecting === g.id}
							<form class="row" onsubmit={(e) => connect(e, g.id)}>
								<label class="field grow">
									<span>Webhook address for {g.name}</span>
									<input bind:value={webhook} type="url" required placeholder="https://…logic.azure.com/workflows/…" autocomplete="off" />
								</label>
								<button class="btn primary" disabled={busy || !webhook.trim()}>Connect</button>
								<button class="btn" type="button" onclick={() => (connecting = '')}>Cancel</button>
							</form>
						{:else}
							<div class="acts">
								<button class="btn quiet-btn" type="button" onclick={() => ((connecting = g.id), (webhook = ''))}
									>{g.connected ? 'Change channel' : 'Connect a channel'}</button
								>
								{#if g.connected}<button class="btn quiet-btn" type="button" disabled={busy} onclick={() => disconnect(g.id)}
										>Disconnect</button
									>{/if}
							</div>
						{/if}
					</li>
				{/each}
			</ul>
		</section>
	{/if}

	<section class="block" aria-labelledby="log-h">
		<h2 id="log-h">Recent messages</h2>
		{#if deliveries.length === 0}
			<p class="msg">Nothing texted or posted yet.</p>
		{:else}
			<table>
				<thead><tr><th>When</th><th>Channel</th><th>To</th><th>What</th><th>Status</th></tr></thead>
				<tbody>
					{#each deliveries as d (d.id)}
						<tr>
							<td>{when(d.created_at)}</td>
							<td>{d.channel === 'sms' ? 'Text' : 'Teams'}</td>
							<td>{d.user_name ? `${d.user_name} · ` : ''}{d.to}</td>
							<td>{d.kind === 'notification' ? d.what : d.kind}</td>
							<td>
								<span class="status {d.status}">{d.status}</span>
								{#if d.detail}<span class="detail">{d.detail}</span>{/if}
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
		{/if}
	</section>
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
		align-items: end;
		gap: 0.6rem;
	}
	.grow {
		flex: 1 1 16rem;
	}
	.check {
		display: flex;
		align-items: center;
		gap: 0.45rem;
	}
	.hint,
	.msg {
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
	.groups {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.groups li {
		display: grid;
		gap: 0.4rem;
		padding: 0.6rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.gname {
		display: flex;
		align-items: baseline;
		gap: 0.75rem;
		font-weight: 600;
	}
	.gname .on,
	.gname .off {
		font-size: 0.8125rem;
		font-weight: 500;
	}
	.gname .on {
		color: var(--green);
	}
	.gname .off {
		color: var(--ink-2);
	}
	.acts {
		display: flex;
		gap: 0.5rem;
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
	.status {
		font-weight: 600;
	}
	.status.failed {
		color: var(--red);
	}
	.status.sent {
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
	}
</style>
