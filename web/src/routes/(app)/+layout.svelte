<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { approvals } from '$lib/approvals.svelte';
	import { cmdb } from '$lib/cmdb.svelte';
	import { age } from '$lib/format';
	import { forms } from '$lib/forms.svelte';
	import { lifecycle } from '$lib/lifecycle.svelte';
	import { notifications } from '$lib/notifications.svelte';
	import { queue } from '$lib/queue.svelte';
	import { realtime } from '$lib/realtime.svelte';
	import { session } from '$lib/session.svelte';
	import { theme } from '$lib/theme.svelte';
	import { api } from '$lib/api';
	import type { Account } from '$lib/types';
	import { ui } from '$lib/ui.svelte';
	import NewTicket from '$lib/components/NewTicket.svelte';
	import { can, canAdmin, canRecord, canRecordAny, P } from '$lib/types';

	let { children } = $props();
	let bell = $state<HTMLDetailsElement>();

	function openNotification(id: string) {
		void notifications.markRead(id);
		if (bell) bell.open = false;
	}

	onMount(() => {
		// Anyone can be named an approver, not only staff, so this loads regardless of role; the Approvals link only
		// shows up when there is actually something for this person to do. Everyone has the knowledge base, so everyone
		// has the sections bar.
		void approvals.ensureLoaded();
		void notifications.ensureLoaded();
		// The theme is kept on the account; this device's copy may be stale (chosen elsewhere, or storage cleared).
		api<Account>('/api/v1/account').then(
			(a) => theme.apply(a.theme),
			() => {}
		);
		realtime.start();
		const off = realtime.on((e) => {
			if (e.type === 'ticket.changed' || e.type === 'resync') queue.scheduleRefresh();
			// An app was installed or removed somewhere: pick up new labels and flows without a reload.
			if (e.type === 'definitions.changed' || e.type === 'resync') {
				void lifecycle.reload();
				void forms.reload();
				void cmdb.reload();
			}
			if (e.type === 'definitions.changed') queue.scheduleRefresh();
			// Pending approvals and app notifications (an assignment, say) both arrive as this.
			if (e.type === 'notification.changed' || e.type === 'resync') {
				void notifications.load();
				void approvals.load();
			}
		});
		return () => {
			off();
			realtime.stop();
		};
	});
</script>

{#snippet sections()}
	{#if canRecordAny(session.user, 'read')}
		<a href="/tickets" aria-current={page.url.pathname.startsWith('/tickets') ? 'page' : undefined}>Tickets</a>
	{/if}
	{#if canRecord(session.user, 'read', 'change') && lifecycle.types.some((t) => t.type === 'change')}
		<a href="/calendar" aria-current={page.url.pathname.startsWith('/calendar') ? 'page' : undefined}>Calendar</a>
	{/if}
	<a href="/kb" aria-current={page.url.pathname.startsWith('/kb') ? 'page' : undefined}>Knowledge</a>
	{#if can(session.user, P.cmdbRead)}
		<a href="/cmdb" aria-current={page.url.pathname.startsWith('/cmdb') ? 'page' : undefined}>CMDB</a>
	{/if}
	{#if can(session.user, P.workflowsReadAll) || can(session.user, P.workflowsEdit)}
		<a href="/workflows" aria-current={page.url.pathname.startsWith('/workflows') ? 'page' : undefined}>Workflows</a>
	{/if}
	{#if can(session.user, P.workflowsReadAll)}
		<a href="/runs" aria-current={page.url.pathname.startsWith('/runs') ? 'page' : undefined}>Runs</a>
	{/if}
	{#if canAdmin(session.user)}
		<a href="/admin" aria-current={page.url.pathname.startsWith('/admin') ? 'page' : undefined}>Admin</a>
	{/if}
	{#if approvals.items.length > 0}
		<a href="/approvals" aria-current={page.url.pathname.startsWith('/approvals') ? 'page' : undefined}>
			Approvals ({approvals.items.length})
		</a>
	{/if}
{/snippet}

<div class="shell">
	<header>
		<a class="mark" href="/tickets" aria-label="ServiceLater, ticket queue">ServiceLater</a>
		<nav class="wide" aria-label="Sections">
			{@render sections()}
		</nav>
		<div class="right">
			{#if realtime.down}
				<span class="offline" role="status">Reconnecting…</span>
			{/if}
			<button class="btn primary" onclick={() => (ui.newTicketOpen = true)}>
				New ticket <kbd aria-hidden="true">c</kbd>
			</button>
			<details class="bell" bind:this={bell}>
				<summary aria-label="Notifications{notifications.unreadCount > 0 ? `, ${notifications.unreadCount} unread` : ''}">
					<svg viewBox="0 0 18 18" aria-hidden="true">
						<path
							d="M9 1.5c-1 0-1.8.8-1.8 1.8v.4C5 4.3 3.6 6 3.6 8v3l-1.2 1.8c-.2.3 0 .7.4.7h13.4c.4 0 .6-.4.4-.7L15.4 11V8c0-2-1.4-3.7-3.6-4.3v-.4c0-1-.8-1.8-1.8-1.8Z"
						/>
						<path d="M7 14.5a2 2 0 0 0 4 0" />
					</svg>
					{#if notifications.unreadCount > 0}<i class="badge" aria-hidden="true"></i>{/if}
				</summary>
				<div class="panel">
					{#if notifications.items.length === 0}
						<p class="empty">Nothing yet.</p>
					{:else}
						<ul>
							{#each notifications.items as n (n.id)}
								<li>
									<a class:unread={!n.read_at} href={n.link} onclick={() => openNotification(n.id)}>
										<span class="what">
											<span>{n.title}</span>
											{#if n.body}<span class="detail">{n.body}</span>{/if}
										</span>
										<time datetime={n.created_at}>{age(n.created_at)}</time>
									</a>
								</li>
							{/each}
						</ul>
					{/if}
					<!-- The header hides the name on a phone, so the way to the account page is here too. -->
					<a class="prefs" href="/account" onclick={() => bell && (bell.open = false)}>Notification settings</a>
				</div>
			</details>
			<a class="who" href="/account" aria-label="Your account, {session.user?.name}">{session.user?.name}</a>
			<button class="btn quiet" onclick={() => session.logout()}>Sign out</button>
		</div>
	</header>

	<!-- On a phone there is no room for these in the app bar, so they get a bar of their own. -->
	<nav class="narrow" aria-label="Sections">
		{@render sections()}
	</nav>

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
		position: relative;
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
		/* An admin has more sections than a phone is wide: they scroll sideways in the bar, never the page. */
		overflow-x: auto;
		scrollbar-width: none;
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
		color: inherit;
		text-decoration: none;
		font-size: 0.9rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.who:hover {
		text-decoration: underline;
	}
	.bell {
		position: relative;
	}
	.bell summary {
		display: flex;
		align-items: center;
		justify-content: center;
		width: 2rem;
		height: 2rem;
		border-radius: var(--radius);
		list-style: none;
		cursor: pointer;
	}
	.bell summary::-webkit-details-marker {
		display: none;
	}
	.bell summary:hover {
		background: color-mix(in srgb, var(--on-chrome) 15%, transparent);
	}
	.bell svg {
		width: 1.1rem;
		height: 1.1rem;
		fill: none;
		stroke: var(--on-chrome);
		stroke-width: 1.3;
		stroke-linecap: round;
		stroke-linejoin: round;
	}
	/* Yellow already means "look here" (unassigned tickets, internal notes), so the badge reuses it. */
	.bell .badge {
		position: absolute;
		top: 0.3rem;
		right: 0.3rem;
		width: 0.5rem;
		height: 0.5rem;
		border-radius: 50%;
		background: var(--marker);
	}
	.bell .panel {
		position: absolute;
		top: calc(100% + 0.4rem);
		right: 0;
		width: 18rem;
		max-width: calc(100vw - 1.5rem);
		max-height: 20rem;
		overflow-y: auto;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--surface);
		color: var(--ink);
		z-index: 20;
	}
	.bell .prefs {
		display: block;
		padding: 0.55rem 0.75rem;
		border-top: 1px solid var(--rule);
		font-size: 0.85rem;
		font-weight: 600;
	}
	.bell .empty {
		padding: 0.9rem;
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.bell ul {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.bell li + li a {
		border-top: 1px solid var(--rule);
	}
	.bell a {
		display: flex;
		justify-content: space-between;
		gap: 0.75rem;
		padding: 0.6rem 0.8rem;
		text-decoration: none;
		color: inherit;
		font-size: 0.875rem;
	}
	.bell a:hover {
		background: var(--blue-soft);
	}
	.bell a.unread {
		box-shadow: inset 3px 0 0 var(--marker);
		font-weight: 600;
	}
	.bell .what {
		display: grid;
		gap: 0.1rem;
		min-width: 0;
		overflow-wrap: anywhere;
	}
	.bell .detail {
		color: var(--ink-2);
		font-weight: 400;
		font-size: 0.8125rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.bell time {
		color: var(--ink-2);
		font-size: 0.8rem;
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
		/* No room for it in the bar (it pushed "Sign out" off the screen), so it hangs just below it instead. */
		.offline {
			position: absolute;
			top: calc(100% + 0.4rem);
			left: 50%;
			transform: translateX(-50%);
			z-index: 20;
		}
		/* The bell added a fourth item to .right; there is no room left at this width for full button padding too. */
		.right {
			gap: 0.4rem;
		}
		header .btn.primary {
			padding: 0 0.6rem;
		}
	}
</style>
