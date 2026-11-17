<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { ApiError } from '$lib/api';
	import { session } from '$lib/session.svelte';

	let email = $state('');
	let password = $state('');
	let busy = $state(false);
	let error = $state('');

	const expired = $derived(page.url.searchParams.has('expired'));
	// Why a single sign-on attempt came back here, in the server's words.
	const ssoError = $derived(page.url.searchParams.get('sso_error') ?? '');

	// The organisation's sign-in providers, and whether passwords are for admins only (the rest use a provider).
	let providers = $state<{ id: string; label: string }[]>([]);
	let adminsOnly = $state(false);
	let showPassword = $state(false);
	const passwordForm = $derived(!adminsOnly || providers.length === 0 || showPassword);

	onMount(async () => {
		try {
			const res = await fetch('/api/v1/auth/sso');
			if (!res.ok) return;
			const opts: { providers: { id: string; label: string }[]; passwords: string } = await res.json();
			providers = opts.providers;
			adminsOnly = opts.passwords === 'admins';
		} catch {
			/* the password form still works */
		}
	});

	const startURL = (id: string) => `/api/v1/auth/sso/${id}/start?${new URLSearchParams({ next: destination() })}`;

	// Only follow same-site paths, so a crafted ?next= can't bounce people to another origin.
	function destination() {
		const next = page.url.searchParams.get('next') ?? '';
		return next.startsWith('/') && !next.startsWith('//') ? next : '/';
	}

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			await session.login(email, password);
			await goto(destination());
		} catch (err) {
			if (err instanceof ApiError && err.status === 429) {
				error = 'Too many sign-in attempts. Wait a few minutes and try again.';
			} else if (err instanceof ApiError && err.status === 401) {
				error = 'That email and password don’t match. Check both and try again.';
			} else if (err instanceof ApiError && err.status === 403) {
				error = err.message;
			} else {
				error = 'Can’t reach the server. Check your connection and try again.';
			}
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head><title>Sign in · ServiceLater</title></svelte:head>

<main>
	<div class="feed">
		<div class="stub">
			<span class="logo" aria-hidden="true">SL</span>
			<h1>ServiceLater</h1>
		</div>
		<p class="lead">Sign in to your workspace</p>

		{#if ssoError}<p class="error sso-error" role="alert">{ssoError}</p>{/if}

		{#if providers.length}
			<div class="providers">
				{#each providers as p (p.id)}
					<a class="btn" class:primary={adminsOnly} href={startURL(p.id)}>Sign in with {p.label}</a>
				{/each}
			</div>
			{#if adminsOnly && !showPassword}
				<button class="link" type="button" onclick={() => (showPassword = true)}>Admins: sign in with a password</button>
			{:else}
				<p class="or"><span>or</span></p>
			{/if}
		{/if}

		{#if passwordForm}
		<form onsubmit={submit}>
			{#if expired && !error}
				<p class="note" role="status">Your session ended. Sign in to pick up where you left off.</p>
			{/if}

			<label class="field">
				<span>Email</span>
				<input type="email" name="email" autocomplete="username" required bind:value={email} />
			</label>
			<label class="field">
				<span>Password</span>
				<input type="password" name="password" autocomplete="current-password" required bind:value={password} />
			</label>

			{#if error}
				<p class="error" role="alert">{error}</p>
			{/if}

			<button class="btn primary" disabled={busy}>{busy ? 'Signing in…' : 'Sign in'}</button>
		</form>
		{/if}
	</div>
</main>

<style>
	main {
		min-height: 100dvh;
		display: grid;
		justify-items: center;
		align-content: start;
		padding: clamp(2rem, 16vh, 8rem) 1rem 3rem;
		background: var(--chrome);
	}

	.feed {
		width: min(23rem, 100%);
		padding: 1.75rem 1.75rem 2rem;
		border: 1px solid var(--rule);
		border-radius: 14px;
		background: var(--ground);
		box-shadow: var(--shadow-pop);
	}

	/* The mark: the yellow ticket and the name. */
	.stub {
		display: flex;
		align-items: center;
		gap: 0.7rem;
	}
	.logo {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		flex: none;
		width: 2.25rem;
		height: 2.25rem;
		border-radius: 10px;
		background: var(--marker);
		color: var(--on-marker);
		font-size: 0.9375rem;
		font-weight: 800;
		letter-spacing: -0.02em;
	}
	h1 {
		font-size: clamp(1.25rem, 6vw, 1.5rem);
		font-weight: 680;
		letter-spacing: -0.02em;
		white-space: nowrap;
	}
	.lead {
		margin-top: 0.9rem;
		color: var(--ink-2);
	}

	form {
		display: grid;
		gap: 1rem;
		padding-top: 1.25rem;
	}
	.btn {
		min-height: 2.5rem;
		margin-top: 0.25rem;
	}
	.error {
		color: var(--red);
		font-weight: 600;
		font-size: 0.875rem;
	}
	.providers {
		display: grid;
		gap: 0.5rem;
		padding-top: 1.25rem;
	}
	.providers .btn {
		justify-content: center;
		text-decoration: none;
	}
	.sso-error {
		margin-top: 1rem;
		padding: 0.6rem 0.75rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
	}
	.or {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		margin-top: 1rem;
		color: var(--ink-2);
		font-size: 0.8125rem;
	}
	.or::before,
	.or::after {
		content: '';
		flex: 1;
		border-top: 1px solid var(--rule);
	}
	.link {
		margin-top: 1rem;
		padding: 0;
		border: 0;
		background: none;
		color: var(--ink-2);
		font: inherit;
		font-size: 0.875rem;
		text-decoration: underline;
		cursor: pointer;
	}
	.note {
		font-size: 0.875rem;
		padding: 0.6rem 0.75rem;
		background: var(--marker-soft);
		border-radius: var(--radius);
	}
</style>
