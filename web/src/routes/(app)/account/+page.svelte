<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import { session } from '$lib/session.svelte';
	import { theme } from '$lib/theme.svelte';
	import type { Account, Theme } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	let account = $state<Account | null>(null);
	let error = $state('');
	let saving = $state(false);

	onMount(async () => {
		try {
			account = await api<Account>('/api/v1/account');
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load your account';
		}
	});

	const themes: { id: Theme; label: string; hint: string }[] = [
		{ id: 'auto', label: 'Automatic', hint: 'Follows your device' },
		{ id: 'light', label: 'Light', hint: 'Always light' },
		{ id: 'dark', label: 'Dark', hint: 'Always dark' }
	];

	// Applies at once and saves to the account; puts the old one back if the save fails.
	async function setTheme(t: Theme) {
		if (!account || account.theme === t) return;
		const before = account.theme;
		account.theme = t;
		theme.apply(t);
		error = '';
		try {
			account = await api<Account>('/api/v1/account', { method: 'PATCH', body: { theme: t } });
			ui.announce = `Appearance: ${themes.find((x) => x.id === t)?.label}`;
		} catch (e) {
			account.theme = before;
			theme.apply(before);
			error = e instanceof Error ? e.message : 'That change didn’t save';
		}
	}

	// A mobile number for text messages: set it, type back the code texted to it, then turn SMS on and off.
	let number = $state('');
	let code = $state('');
	let codeSent = $state(false);

	async function phone(path: string, method: 'PUT' | 'POST' | 'DELETE', body: unknown, done: string) {
		saving = true;
		error = '';
		try {
			account = await api<Account>(`/api/v1/account/phone${path}`, { method, body });
			ui.announce = done;
			return true;
		} catch (e) {
			error = e instanceof Error ? e.message : 'That didn’t work';
			return false;
		} finally {
			saving = false;
		}
	}

	async function sendCode(e: Event) {
		e.preventDefault();
		if (await phone('', 'PUT', { number: number.trim() }, 'A code is on its way by text')) {
			codeSent = true;
			code = '';
		}
	}

	async function verify(e: Event) {
		e.preventDefault();
		if (await phone('/verify', 'POST', { code: code.trim() }, 'Number confirmed; text messages are on')) codeSent = false;
	}

	async function setSMS(on: boolean) {
		saving = true;
		error = '';
		try {
			account = await api<Account>('/api/v1/account', { method: 'PATCH', body: { sms_notifications: on } });
			ui.announce = on ? 'Text messages on' : 'Text messages off';
		} catch (err) {
			error = err instanceof Error ? err.message : 'That change didn’t save';
		} finally {
			saving = false;
		}
	}

	async function setEmail(on: boolean) {
		saving = true;
		error = '';
		try {
			account = await api<Account>('/api/v1/account', { method: 'PATCH', body: { email_notifications: on } });
			ui.announce = on ? 'Email notifications on' : 'Email notifications off';
		} catch (e) {
			error = e instanceof Error ? e.message : 'That change didn’t save';
		} finally {
			saving = false;
		}
	}
</script>

<svelte:head><title>Your account – ServiceLater</title></svelte:head>

<div class="page">
	<h1>Your account</h1>
	<p class="lede">{session.user?.name}{account ? ` · ${account.email}` : ''}</p>
	{#if error}<p class="error" role="alert">{error}</p>{/if}
	{#if account}
		<section aria-labelledby="look-h">
			<h2 id="look-h">Appearance</h2>
			<div class="themes" role="radiogroup" aria-labelledby="look-h">
				{#each themes as t (t.id)}
					<label class="theme" class:on={account.theme === t.id}>
						<input type="radio" name="theme" value={t.id} checked={account.theme === t.id} onchange={() => setTheme(t.id)} />
						<span class="swatch {t.id}" aria-hidden="true"></span>
						<span class="name">{t.label}</span>
						<span class="sub">{t.hint}</span>
					</label>
				{/each}
			</div>
		</section>
		<section aria-labelledby="notify-h">
			<h2 id="notify-h">Notifications</h2>
			<label class="check">
				<input
					type="checkbox"
					checked={account.email_notifications}
					disabled={saving}
					onchange={(e) => setEmail(e.currentTarget.checked)}
				/>
				Also email me my notifications
			</label>
			<p class="hint">
				{#if account.email_enabled}
					They always appear under the bell; this sends them to {account.email} too.
				{:else}
					This server does not send email yet, so for now they appear only under the bell.
				{/if}
			</p>
		</section>
		{#if account.sms_enabled || account.phone}
			<section aria-labelledby="sms-h">
				<h2 id="sms-h">Text messages</h2>
				<p class="hint">Only urgent notifications are texted, such as an approval waiting for you or a missed deadline.</p>
				{#if account.phone && account.phone_verified && !codeSent}
					<label class="check">
						<input type="checkbox" checked={account.sms_notifications} disabled={saving} onchange={(e) => setSMS(e.currentTarget.checked)} />
						Text me urgent notifications at {account.phone}
					</label>
					<div class="row">
						<button class="btn" type="button" onclick={() => ((codeSent = false), (number = account!.phone), (account!.phone_verified = false))}>Change number</button>
						<button class="btn quiet-btn" type="button" disabled={saving} onclick={() => phone('', 'DELETE', undefined, 'Number removed')}>Remove number</button>
					</div>
				{:else if codeSent || (account.phone && !account.phone_verified && !number)}
					<form class="row" onsubmit={verify}>
						<label class="field">
							<span>Code texted to {account.phone}</span>
							<input bind:value={code} inputmode="numeric" autocomplete="one-time-code" maxlength="6" required />
						</label>
						<button class="btn primary" disabled={saving || code.trim().length !== 6}>Confirm</button>
						<button class="btn" type="button" onclick={() => ((codeSent = false), (number = account!.phone))}>Send a new code</button>
					</form>
				{:else}
					<form class="row" onsubmit={sendCode}>
						<label class="field">
							<span>Mobile number</span>
							<input bind:value={number} type="tel" autocomplete="tel" placeholder="+1 555 010 1234" required />
						</label>
						<button class="btn primary" disabled={saving || !number.trim()}>Text me a code</button>
					</form>
					<p class="hint">In international form, with + and the country code. We text a six-digit code to confirm it is yours.</p>
				{/if}
			</section>
		{/if}
	{/if}
</div>

<style>
	.row {
		display: flex;
		flex-wrap: wrap;
		align-items: end;
		gap: 0.6rem;
		margin-top: 0.5rem;
	}
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 40rem;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
	}
	h2 {
		font: 650 1.0625rem/1.3 var(--font-display);
		margin-bottom: 0.6rem;
	}
	.lede {
		margin: 0.25rem 0 1rem;
		color: var(--ink-2);
		overflow-wrap: anywhere;
	}
	section {
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
	}
	section + section {
		margin-top: 1.5rem;
	}
	.themes {
		display: grid;
		grid-template-columns: repeat(3, minmax(0, 1fr));
		gap: 0.6rem;
	}
	.theme {
		position: relative;
		display: grid;
		justify-items: center;
		gap: 0.15rem;
		padding: 0.75rem 0.5rem 0.65rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--raised);
		text-align: center;
		cursor: pointer;
	}
	.theme:hover {
		background: var(--blue-soft);
	}
	.theme.on {
		border-color: var(--blue);
		box-shadow: inset 0 0 0 1px var(--blue);
	}
	.theme:has(input:focus-visible) {
		outline: 2px solid var(--focus);
		outline-offset: 2px;
	}
	.theme input {
		position: absolute;
		opacity: 0;
		pointer-events: none;
	}
	/* A small window drawn in each theme's own app-bar and page colours: the one place fixed colours are right. */
	.swatch {
		width: 4rem;
		height: 2.6rem;
		margin-bottom: 0.35rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: linear-gradient(#0f1440 0 25%, #e8ebf1 25%);
	}
	.swatch.dark {
		background: linear-gradient(#060820 0 25%, #0a0d29 25%);
	}
	.swatch.auto {
		background:
			linear-gradient(#0f1440 0 25%, transparent 25%),
			linear-gradient(90deg, #e8ebf1 50%, #0a0d29 50%);
	}
	.name {
		font-weight: 600;
		font-size: 0.9rem;
	}
	.sub {
		font-size: 0.8rem;
		color: var(--ink-2);
	}
	@media (max-width: 26rem) {
		.themes {
			gap: 0.4rem;
		}
		.swatch {
			width: 3.25rem;
		}
	}
	.check {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		font-weight: 600;
	}
	.hint {
		margin-top: 0.4rem;
		color: var(--ink-2);
		font-size: 0.9rem;
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
</style>
