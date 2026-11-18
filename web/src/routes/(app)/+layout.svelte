<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { approvals } from '$lib/approvals.svelte';
	import { cmdb } from '$lib/cmdb.svelte';
	import { age } from '$lib/format';
	import { forms } from '$lib/forms.svelte';
	import { lifecycle } from '$lib/lifecycle.svelte';
	import { notifications } from '$lib/notifications.svelte';
	import { queue, queueViews } from '$lib/queue.svelte';
	import { adminGroups } from '$lib/adminNav';
	import { realtime } from '$lib/realtime.svelte';
	import { session } from '$lib/session.svelte';
	import { theme } from '$lib/theme.svelte';
	import { api } from '$lib/api';
	import type { Account } from '$lib/types';
	import { setNavCollapsed, ui } from '$lib/ui.svelte';
	import Avatar from '$lib/components/Avatar.svelte';
	import Icon, { type IconName } from '$lib/components/Icon.svelte';
	import GlobalSearch from '$lib/components/GlobalSearch.svelte';
	import NewTicket from '$lib/components/NewTicket.svelte';
	import { can, canAdmin, canRecord, canRecordAny, P } from '$lib/types';

	let { children } = $props();
	let bell = $state<HTMLDetailsElement>();

	const at = (prefix: string) => page.url.pathname.startsWith(prefix);
	// The sections this person has, in order. Approvals appears only while something waits on them.
	const sections = $derived(
		[
			{ href: '/tickets', label: 'Tickets', icon: 'tickets', show: canRecordAny(session.user, 'read') },
			{
				href: '/calendar',
				label: 'Calendar',
				icon: 'calendar',
				show: canRecord(session.user, 'read', 'change') && lifecycle.types.some((t) => t.type === 'change')
			},
			{
				href: '/problems',
				label: 'Problems',
				icon: 'problems',
				show: canRecord(session.user, 'read', 'problem') && lifecycle.types.some((t) => t.type === 'problem')
			},
			{ href: '/catalog', label: 'Request something', icon: 'catalog', show: true },
			{ href: '/kb', label: 'Knowledge', icon: 'knowledge', show: true },
			{ href: '/cmdb', label: 'CMDB', icon: 'cmdb', show: can(session.user, P.cmdbRead) },
			{ href: '/reports/slas', label: 'Reports', icon: 'reports', show: can(session.user, P.reportsRead) },
			{
				href: '/workflows',
				label: 'Workflows',
				icon: 'workflows',
				show: can(session.user, P.workflowsReadAll) || can(session.user, P.workflowsEdit)
			},
			{ href: '/runs', label: 'Runs', icon: 'runs', show: can(session.user, P.workflowsReadAll) },
			{ href: '/admin', label: 'Admin', icon: 'admin', show: canAdmin(session.user) },
			{
				href: '/approvals',
				label: `Approvals (${approvals.items.length})`,
				icon: 'approvals',
				show: approvals.items.length > 0
			}
		].filter((s) => s.show) as { href: string; label: string; icon: IconName }[]
	);
	// The shortcut as this person's keyboard shows it.
	const searchKey = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform) ? '⌘K' : 'Ctrl K';
	const views = $derived(queueViews(canRecordAny(session.user, 'read')));
	const admin = $derived(adminGroups(session.user));

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

{#snippet links(icons: boolean)}
	{#each sections as s (s.href)}
		<a href={s.href} aria-current={at(s.href) ? 'page' : undefined} title={icons && ui.navCollapsed ? s.label : undefined}>
			{#if icons}<Icon name={s.icon} />{/if}
			<span class="lbl">{s.label}</span>
		</a>
	{/each}
{/snippet}

<div class="shell" class:collapsed={ui.navCollapsed}>
	<!-- The navigation: a column at the left, or on a phone a bar across the top. -->
	<aside class="side">
		<div class="brand">
			<a class="mark" href="/" aria-label="ServiceLater, home">
				<span class="logo" aria-hidden="true">SL</span>
				<span class="word">ServiceLater</span>
			</a>
			<button
				class="collapse"
				onclick={() => setNavCollapsed(!ui.navCollapsed)}
				aria-label={ui.navCollapsed ? 'Expand navigation' : 'Collapse navigation'}
				aria-expanded={!ui.navCollapsed}
				title={ui.navCollapsed ? 'Expand navigation' : 'Collapse navigation'}
			>
				<Icon name="sidebar" />
			</button>
		</div>

		<button class="btn primary new" onclick={() => (ui.newTicketOpen = true)} title={ui.navCollapsed ? 'New ticket' : undefined}>
			<Icon name="plus" size={15} />
			<span class="lbl">New ticket</span>
			<kbd aria-hidden="true">c</kbd>
		</button>

		<button class="find" onclick={() => (ui.searchOpen = true)} aria-label="Search everything" title={ui.navCollapsed ? `Search (${searchKey})` : undefined}>
			<Icon name="search" size={15} />
			<span class="lbl">Search</span>
			<kbd aria-hidden="true">{searchKey}</kbd>
		</button>

		<nav class="wide" aria-label="Sections">
			{@render links(true)}
		</nav>

		{#if at('/tickets')}
			<div class="views" role="group" aria-label="Show">
				<h2>Views</h2>
				{#each views as v (v.id)}
					<!-- The count describes the view; it is not part of its name, which stays "Mine". -->
					<button
						aria-pressed={queue.filter === v.id}
						title={queue.counts[v.id] ? `${queue.counts[v.id]} records` : undefined}
						onclick={() => queue.setFilter(v.id)}
					>
						{v.label}
						{#if queue.counts[v.id]}<span class="count" aria-hidden="true">{queue.counts[v.id]}</span>{/if}
					</button>
				{/each}
			</div>
		{/if}

		<!-- In the admin section, its pages, under Admin like the queue's views under Tickets. -->
		{#if at('/admin')}
			<nav class="views" aria-label="Admin">
				{#each admin as g (g.label)}
					<h2>{g.label}</h2>
					{#each g.pages as p (p.href)}
						<a href={p.href} aria-current={at(p.href) ? 'page' : undefined}>{p.label}</a>
					{/each}
				{/each}
			</nav>
		{/if}

		<div class="foot">
			{#if realtime.down}
				<span class="offline" role="status">Reconnecting…</span>
			{/if}
			<a class="who" href="/account" aria-label="Your account, {session.user?.name}" title={ui.navCollapsed ? session.user?.name : undefined}>
				<Avatar name={session.user?.name ?? '?'} />
				<span class="lbl">{session.user?.name}</span>
			</a>
			<details class="bell" bind:this={bell}>
				<summary aria-label="Notifications{notifications.unreadCount > 0 ? `, ${notifications.unreadCount} unread` : ''}" title="Notifications">
					<Icon name="bell" size={17} />
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
					<!-- A phone hides the name, so the way to the account page is here too. -->
					<a class="prefs" href="/account" onclick={() => bell && (bell.open = false)}>Notification settings</a>
				</div>
			</details>
			<button class="signout" onclick={() => session.logout()} aria-label="Sign out" title="Sign out">
				<Icon name="signout" size={17} />
			</button>
		</div>
	</aside>

	<div class="main">
		<!-- On a phone the sections get a bar of their own under the app bar. -->
		<nav class="narrow" aria-label="Sections">
			{@render links(false)}
		</nav>

		<div class="content">
			{@render children()}
		</div>
	</div>

	<div class="sr-only" aria-live="polite">{ui.announce}</div>
</div>

<NewTicket />
<GlobalSearch />

<style>
	.shell {
		height: 100dvh;
		display: flex;
	}
	.side {
		flex: none;
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
		width: 14.5rem;
		padding: 0.75rem 0.625rem;
		background: var(--chrome);
		color: var(--on-chrome);
		border-right: 1px solid var(--rule);
		overflow-y: auto;
		overflow-x: hidden;
	}
	.main {
		flex: 1;
		min-width: 0;
		display: flex;
		flex-direction: column;
	}
	.content {
		flex: 1;
		min-height: 0;
	}

	.brand {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.5rem;
		padding: 0.125rem 0.25rem 0.125rem 0.4rem;
	}
	.mark {
		display: flex;
		align-items: center;
		gap: 0.55rem;
		min-width: 0;
		text-decoration: none;
		font-weight: 650;
		font-size: 0.9375rem;
		letter-spacing: -0.01em;
	}
	/* The yellow ticket: what is left of the stub, as the mark. */
	.logo {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		flex: none;
		width: 1.5rem;
		height: 1.5rem;
		border-radius: 7px;
		background: var(--marker);
		color: var(--on-marker);
		font-size: 0.6875rem;
		font-weight: 800;
		letter-spacing: -0.02em;
	}
	.collapse,
	.signout,
	.bell summary {
		display: flex;
		align-items: center;
		justify-content: center;
		flex: none;
		width: 1.875rem;
		height: 1.875rem;
		padding: 0;
		border: 0;
		border-radius: 7px;
		background: none;
		color: var(--ink-2);
		cursor: pointer;
	}
	.collapse:hover,
	.signout:hover,
	.bell summary:hover {
		background: var(--hover);
		color: var(--ink);
	}

	.new {
		justify-content: flex-start;
		gap: 0.45rem;
		padding: 0 0.6rem;
		white-space: nowrap;
	}
	.new kbd {
		margin-left: auto;
		border-color: transparent;
		background: color-mix(in srgb, var(--on-primary) 18%, transparent);
		color: inherit;
	}

	.find {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		height: 2rem;
		margin-top: -0.25rem;
		padding: 0 0.55rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--raised);
		color: var(--ink-2);
		font-size: 0.875rem;
		text-align: left;
		cursor: pointer;
	}
	.find:hover {
		color: var(--ink);
	}
	.find kbd {
		margin-left: auto;
		border: 0;
		background: none;
		color: var(--ink-2);
	}

	nav.wide {
		display: flex;
		flex-direction: column;
		gap: 1px;
	}
	nav.wide a,
	.views a,
	.views button {
		display: flex;
		align-items: center;
		gap: 0.6rem;
		height: 1.9rem;
		padding: 0 0.5rem;
		border: 0;
		border-radius: 7px;
		background: none;
		color: var(--ink-2);
		font-size: 0.875rem;
		font-weight: 500;
		text-align: left;
		text-decoration: none;
		white-space: nowrap;
		cursor: pointer;
	}
	nav.wide a:hover,
	.views a:hover,
	.views button:hover {
		background: var(--hover);
		color: var(--ink);
	}
	nav.wide a[aria-current='page'],
	.views a[aria-current='page'],
	.views button[aria-pressed='true'] {
		background: color-mix(in srgb, var(--ink) 8%, transparent);
		color: var(--ink);
		font-weight: 600;
	}
	nav.wide a[aria-current='page'] :global(.icon) {
		color: var(--primary);
	}
	.views {
		display: flex;
		flex-direction: column;
		gap: 1px;
	}
	.views .count {
		margin-left: auto;
		font-size: 0.75rem;
		font-weight: 500;
		color: var(--ink-2);
		font-variant-numeric: tabular-nums;
	}
	.views a + h2 {
		margin-top: 0.4rem;
	}
	.views h2 {
		padding: 0.25rem 0.5rem 0.3rem;
		font-size: 0.75rem;
		font-weight: 600;
		letter-spacing: 0;
		color: var(--ink-2);
	}

	.foot {
		display: flex;
		align-items: center;
		gap: 0.125rem;
		margin-top: auto;
		padding-top: 0.625rem;
		border-top: 1px solid var(--rule);
	}
	.who {
		display: flex;
		align-items: center;
		gap: 0.55rem;
		flex: 1;
		min-width: 0;
		padding: 0.2rem 0.25rem;
		border-radius: 7px;
		text-decoration: none;
		font-size: 0.875rem;
		font-weight: 550;
	}
	.who:hover {
		background: var(--hover);
	}
	.who .lbl {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.offline {
		position: absolute;
		left: 0.625rem;
		bottom: 3.5rem;
		padding: 0.1rem 0.5rem;
		border-radius: 5px;
		background: var(--marker);
		color: var(--on-marker);
		font-size: 0.8125rem;
		font-weight: 600;
		white-space: nowrap;
		z-index: 5;
	}
	.side {
		position: relative;
	}

	.bell {
		position: relative;
	}
	.bell summary {
		list-style: none;
		position: relative;
	}
	.bell summary::-webkit-details-marker {
		display: none;
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
		box-shadow: 0 0 0 2px var(--chrome);
	}
	.bell .panel {
		position: fixed;
		left: 0.625rem;
		bottom: 3.25rem;
		width: 20rem;
		max-width: calc(100vw - 1.5rem);
		max-height: 24rem;
		overflow-y: auto;
		border: 1px solid var(--rule);
		border-radius: 10px;
		background: var(--raised);
		color: var(--ink);
		box-shadow: var(--shadow-pop);
		z-index: 30;
	}
	.bell .prefs {
		display: block;
		padding: 0.55rem 0.75rem;
		border-top: 1px solid var(--rule);
		font-size: 0.8125rem;
		font-weight: 550;
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
	.bell li a {
		display: flex;
		justify-content: space-between;
		gap: 0.75rem;
		padding: 0.6rem 0.8rem;
		text-decoration: none;
		color: inherit;
		font-size: 0.8125rem;
	}
	.bell li a:hover {
		background: var(--hover);
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
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.bell time {
		color: var(--ink-2);
		font-size: 0.75rem;
		white-space: nowrap;
	}

	nav.narrow {
		display: none;
	}

	/* Collapsed: icons only. The labels stay for screen readers and show as tooltips. */
	@media (min-width: 40.01rem) {
		.collapsed .side {
			width: 3.5rem;
			align-items: center;
			padding-inline: 0.5rem;
		}
		.collapsed .lbl,
		.collapsed .word,
		.collapsed .new kbd,
		.collapsed .find kbd,
		.collapsed .views {
			position: absolute;
			width: 1px;
			height: 1px;
			overflow: hidden;
			clip-path: inset(50%);
			white-space: nowrap;
		}
		.collapsed .views {
			display: none;
		}
		.collapsed .brand {
			flex-direction: column;
			padding: 0;
			gap: 0.5rem;
		}
		.collapsed .new,
		.collapsed .find,
		.collapsed nav.wide a {
			width: 2.125rem;
			padding: 0;
			justify-content: center;
		}
		.collapsed .foot {
			flex-direction: column;
			gap: 0.25rem;
		}
		.collapsed .who {
			flex: none;
			padding: 0.15rem;
		}
		.collapsed .bell .panel {
			left: 3.9rem;
			bottom: 0.625rem;
		}
	}

	/* A phone: the column becomes a bar across the top, and the sections a second bar under it. */
	@media (max-width: 40rem) {
		.shell {
			flex-direction: column;
		}
		.side {
			flex-direction: row;
			align-items: center;
			width: auto;
			height: 3.25rem;
			padding: 0 0.75rem;
			gap: 0.5rem;
			border-right: 0;
			border-bottom: 1px solid var(--rule);
			overflow: visible;
		}
		.brand {
			padding: 0;
			margin-right: auto;
		}
		.collapse,
		nav.wide,
		.views,
		.new kbd,
		.find kbd,
		.find .lbl,
		.word,
		.who .lbl {
			display: none;
		}
		.find {
			order: 1; /* after New ticket, beside the bell */
			width: 1.875rem;
			height: 1.875rem;
			margin: 0;
			padding: 0;
			justify-content: center;
			border: 0;
			background: none;
		}
		.foot {
			order: 2;
		}
		.new {
			padding: 0 0.7rem;
		}
		.foot {
			margin: 0;
			padding: 0;
			border: 0;
			gap: 0.25rem;
		}
		.who {
			flex: none;
			padding: 0;
		}
		.offline {
			left: 50%;
			bottom: auto;
			top: calc(100% + 0.4rem);
			transform: translateX(-50%);
			z-index: 20;
		}
		.bell .panel {
			left: auto;
			right: 0.75rem;
			bottom: auto;
			top: 3.5rem;
		}
		nav.narrow {
			display: flex;
			flex: none;
			gap: 0.125rem;
			/* An admin has more sections than a phone is wide: they scroll sideways in the bar, never the page. */
			overflow-x: auto;
			scrollbar-width: none;
			padding: 0.25rem 0.5rem;
			background: var(--chrome);
			border-bottom: 1px solid var(--rule);
		}
		nav.narrow a {
			display: inline-flex;
			align-items: center;
			min-height: 2.25rem;
			padding: 0 0.8rem;
			border-radius: 7px;
			color: var(--ink-2);
			font-size: 0.875rem;
			font-weight: 550;
			text-decoration: none;
			white-space: nowrap;
		}
		nav.narrow a[aria-current='page'] {
			background: color-mix(in srgb, var(--ink) 8%, transparent);
			color: var(--ink);
		}
	}
</style>
