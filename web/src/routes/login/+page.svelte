<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { ApiError } from '$lib/api';
	import { session } from '$lib/session.svelte';

	let email = $state('');
	let password = $state('');
	let busy = $state(false);
	let error = $state('');

	const expired = $derived(page.url.searchParams.has('expired'));

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
	<div class="slot" aria-hidden="true"></div>

	<div class="feed">
		<div class="stub">
			<h1>ServiceLater</h1>
		</div>

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
	</div>
</main>

<style>
	main {
		min-height: 100dvh;
		display: grid;
		justify-items: center;
		align-content: start;
		padding: 0 1rem 3rem;
	}

	/* The slot the stub prints from. */
	.slot {
		width: min(32rem, 100%);
		height: 0.5rem;
		margin-top: clamp(2rem, 16vh, 8rem);
		border-radius: 0 0 var(--radius) var(--radius);
		background: var(--ink);
	}

	.feed {
		width: min(24rem, 100%);
		animation: print 0.7s cubic-bezier(0.2, 0.8, 0.2, 1) both;
	}

	.stub {
		position: relative;
		padding: 2rem 1.5rem 1.5rem;
		background: var(--marker);
		color: var(--on-marker);
		--notch: 0.7rem;
		-webkit-mask:
			radial-gradient(circle var(--notch) at 0 100%, #0000 98%, #000) left / 51% 100% no-repeat,
			radial-gradient(circle var(--notch) at 100% 100%, #0000 98%, #000) right / 51% 100% no-repeat;
		mask:
			radial-gradient(circle var(--notch) at 0 100%, #0000 98%, #000) left / 51% 100% no-repeat,
			radial-gradient(circle var(--notch) at 100% 100%, #0000 98%, #000) right / 51% 100% no-repeat;
	}
	/* Perforation along the tear edge. */
	.stub::after {
		content: '';
		position: absolute;
		left: 1rem;
		right: 1rem;
		bottom: 0;
		border-bottom: 2px dashed color-mix(in srgb, var(--on-marker) 45%, transparent);
	}
	h1 {
		font: 900 clamp(2.4rem, 12vw, 3.5rem) / 0.9 var(--font-display);
		letter-spacing: 0.005em;
		white-space: nowrap;
		text-align: center;
	}

	form {
		display: grid;
		gap: 1rem;
		padding: 1.75rem 0.25rem 0;
	}
	.btn {
		min-height: 2.75rem;
		margin-top: 0.25rem;
	}
	.error {
		color: var(--red);
		font-weight: 600;
		font-size: 0.9rem;
	}
	.note {
		font-size: 0.9rem;
		padding: 0.6rem 0.75rem;
		background: var(--marker-soft);
		border-radius: var(--radius);
	}

	@keyframes print {
		from {
			transform: translateY(-1.2rem);
			clip-path: inset(0 0 100% 0);
			opacity: 0.4;
		}
		to {
			transform: none;
			clip-path: inset(0 0 -1rem 0);
			opacity: 1;
		}
	}
</style>
