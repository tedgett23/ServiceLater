<script lang="ts">
	import { onMount } from 'svelte';
	import { queue } from '$lib/queue.svelte';
	import { realtime } from '$lib/realtime.svelte';
	import { session } from '$lib/session.svelte';
	import { ui } from '$lib/ui.svelte';
	import NewTicket from '$lib/components/NewTicket.svelte';

	let { children } = $props();

	onMount(() => {
		realtime.start();
		const off = realtime.on((e) => {
			if (e.type === 'ticket.changed' || e.type === 'resync') queue.scheduleRefresh();
		});
		return () => {
			off();
			realtime.stop();
		};
	});
</script>

<div class="shell">
	<header>
		<a class="mark" href="/tickets" aria-label="ServiceLater, ticket queue">ServiceLater</a>
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
		header {
			padding: 0 0.75rem;
		}
	}
</style>
