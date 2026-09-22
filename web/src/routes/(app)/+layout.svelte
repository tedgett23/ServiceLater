<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { lifecycle } from '$lib/lifecycle.svelte';
	import { queue } from '$lib/queue.svelte';
	import { realtime } from '$lib/realtime.svelte';
	import { session } from '$lib/session.svelte';
	import { ui } from '$lib/ui.svelte';
	import NewTicket from '$lib/components/NewTicket.svelte';
	import { isStaff } from '$lib/types';

	let { children } = $props();

	onMount(() => {
		realtime.start();
		const off = realtime.on((e) => {
			if (e.type === 'ticket.changed' || e.type === 'resync') queue.scheduleRefresh();
			// An app was installed or removed somewhere: pick up new labels and flows without a reload.
			if (e.type === 'definitions.changed' || e.type === 'resync') void lifecycle.reload();
			if (e.type === 'definitions.changed') queue.scheduleRefresh();
		});
		return () => {
			off();
			realtime.stop();
		};
	});
</script>

{#snippet sections()}
	<a href="/tickets" aria-current={page.url.pathname.startsWith('/tickets') ? 'page' : undefined}>Tickets</a>
	<a href="/runs" aria-current={page.url.pathname.startsWith('/runs') ? 'page' : undefined}>Runs</a>
{/snippet}

<div class="shell">
	<header>
		<a class="mark" href="/tickets" aria-label="ServiceLater, ticket queue">ServiceLater</a>
		{#if isStaff(session.user)}
			<nav class="wide" aria-label="Sections">
				{@render sections()}
			</nav>
		{/if}
		<div class="right">
			{#if realtime.down}
				<span class="offline" role="status">Reconnecting…</span>
			{/if}
			<button class="btn primary" onclick={() => (ui.newTicketOpen = true)}>
				New ticket <kbd aria-hidden="true">c</kbd>
			</button>
			<span class="who">{session.user?.name}</span>
			<button class="btn quiet" onclick={() => session.logout()}>Sign out</button>
		</div>
	</header>

	{#if isStaff(session.user)}
		<!-- On a phone there is no room for these in the app bar, so they get a bar of their own. -->
		<nav class="narrow" aria-label="Sections">
			{@render sections()}
		</nav>
	{/if}

	<div class="content">
		{@render children()}
	</div>

	<div class="sr-only" aria-live="polite">{ui.announce}</div>
</div>

<NewTicket />

<style>
	.shell {
		height: 100dvh;
		display: flex;
		flex-direction: column;
	}
	header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 1rem;
		height: 3.25rem;
		padding: 0 1rem;
		background: var(--chrome);
		color: var(--on-chrome);
		flex: none;
	}
	.mark {
		font: 900 1.6rem/1 var(--font-display);
		letter-spacing: 0.01em;
		text-decoration: none;
		color: var(--marker);
	}
	nav {
		display: flex;
		gap: 0.25rem;
	}
	nav.wide {
		margin-right: auto;
		margin-left: 0.5rem;
	}
	nav.narrow {
		display: none;
		flex: none;
		padding: 0.25rem 0.75rem;
		background: var(--chrome);
		border-top: 1px solid color-mix(in srgb, var(--on-chrome) 18%, transparent);
	}
	nav a {
		white-space: nowrap;
		padding: 0.2rem 0.7rem;
		border-radius: var(--radius);
		font-size: 0.9rem;
		font-weight: 600;
		text-decoration: none;
		color: color-mix(in srgb, var(--on-chrome) 75%, transparent);
	}
	nav a:hover {
		color: var(--on-chrome);
	}
	nav a[aria-current='page'] {
		background: color-mix(in srgb, var(--on-chrome) 15%, transparent);
		color: var(--on-chrome);
	}
	.right {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		min-width: 0;
	}
	.offline {
		padding: 0.1rem 0.5rem;
		border-radius: 2px;
		background: var(--marker);
		color: var(--on-marker);
		font-size: 0.8125rem;
		font-weight: 600;
		white-space: nowrap;
	}
	.who {
		font-size: 0.9rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	header .btn {
		min-height: 2rem;
		white-space: nowrap;
	}
	header .btn.primary kbd {
		background: color-mix(in srgb, var(--on-blue) 18%, transparent);
		border-color: color-mix(in srgb, var(--on-blue) 35%, transparent);
		color: inherit;
	}
	header .btn.quiet {
		background: transparent;
		border-color: color-mix(in srgb, var(--on-chrome) 35%, transparent);
		color: var(--on-chrome);
	}
	header .btn.quiet:hover {
		background: color-mix(in srgb, var(--on-chrome) 15%, transparent);
	}
	header :focus-visible {
		outline-color: var(--marker);
	}
	.content {
		flex: 1;
		min-height: 0;
	}
	@media (max-width: 40rem) {
		.who,
		header kbd {
			display: none;
		}
		nav.wide {
			display: none;
		}
		nav.narrow {
			display: flex;
		}
		nav.narrow a {
			min-height: 2.25rem;
			display: inline-flex;
			align-items: center;
			padding: 0 0.9rem;
		}
		header {
			padding: 0 0.75rem;
		}
	}
</style>
