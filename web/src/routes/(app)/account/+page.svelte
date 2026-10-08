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
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 40rem;
	}
	h1 {
		font: 800 1.6rem/1.2 var(--font-display);
	}
	h2 {
		font: 700 1.25rem/1.2 var(--font-display);
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
