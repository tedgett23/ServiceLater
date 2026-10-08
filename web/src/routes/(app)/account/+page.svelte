<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import { session } from '$lib/session.svelte';
	import type { Account } from '$lib/types';
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
