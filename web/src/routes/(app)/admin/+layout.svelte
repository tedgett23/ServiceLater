<script lang="ts">
	import { page } from '$app/state';
	import { adminGroups } from '$lib/adminNav';
	import { ui } from '$lib/ui.svelte';

	let { children, data } = $props();

	const here = (path: string) => (page.url.pathname.startsWith(path) ? 'page' : undefined);
	const groups = $derived(adminGroups(data.user));
</script>

<div class="admin">
	<!-- The admin pages are listed under Admin in the main navigation. On a phone, or with the navigation folded to
	     icons, they are this bar above the page instead. -->
	<nav class="bar" class:shown={ui.navCollapsed} aria-label="Admin">
		{#each groups as g (g.label)}
			{#each g.pages as p (p.href)}
				<a href={p.href} aria-current={here(p.href)}>{p.label}</a>
			{/each}
		{/each}
	</nav>
	<div class="body">
		{@render children()}
	</div>
</div>

<style>
	.admin {
		height: 100%;
		display: flex;
		flex-direction: column;
	}
	.bar {
		display: none;
		flex: none;
		gap: 0.15rem;
		overflow-x: auto;
		padding: 0.4rem 0.75rem;
		border-bottom: 1px solid var(--rule);
		background: var(--ground);
		scrollbar-width: none;
	}
	.bar.shown {
		display: flex;
	}
	.bar a {
		padding: 0.35rem 0.6rem;
		border-radius: 7px;
		font-weight: 500;
		font-size: 0.875rem;
		color: var(--ink-2);
		text-decoration: none;
		white-space: nowrap;
	}
	.bar a:hover {
		color: var(--ink);
		background: var(--hover);
	}
	.bar a[aria-current='page'] {
		color: var(--ink);
		font-weight: 600;
		background: color-mix(in srgb, var(--ink) 8%, transparent);
	}
	.body {
		flex: 1;
		min-height: 0;
		background: var(--surface);
	}
	@media (max-width: 40rem) {
		.bar {
			display: flex;
		}
	}
</style>
