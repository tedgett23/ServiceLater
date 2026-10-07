<script lang="ts">
	import { page } from '$app/state';

	let { children } = $props();

	const here = (path: string) => (page.url.pathname.startsWith(path) ? 'page' : undefined);
</script>

<div class="admin">
	<nav aria-label="Admin">
		<a href="/admin/users" aria-current={here('/admin/users')}>Users</a>
		<a href="/admin/groups" aria-current={here('/admin/groups')}>Groups</a>
		<!-- The API reference lives outside the app shell (it needs no sign-in), so this leaves the app. -->
		<a href="/docs/api">API reference</a>
	</nav>
	<div class="body">
		{@render children()}
	</div>
</div>

<style>
	.admin {
		height: 100%;
		display: grid;
		grid-template-columns: 12rem minmax(0, 1fr);
	}
	nav {
		display: flex;
		flex-direction: column;
		gap: 0.15rem;
		padding: 1rem 0.75rem;
		border-right: 1px solid var(--rule);
		background: var(--ground);
	}
	nav a {
		padding: 0.4rem 0.6rem;
		border-radius: var(--radius);
		font-weight: 600;
		font-size: 0.925rem;
		color: var(--ink-2);
		text-decoration: none;
		white-space: nowrap;
	}
	nav a:hover {
		color: var(--ink);
		background: var(--blue-soft);
	}
	nav a[aria-current='page'] {
		color: var(--ink);
		background: var(--surface);
		box-shadow: inset 3px 0 0 var(--blue);
	}
	.body {
		min-height: 0;
		background: var(--surface);
	}

	/* Narrow: the section links become a row of tabs above the page. */
	@media (max-width: 40rem) {
		.admin {
			display: flex;
			flex-direction: column;
		}
		nav {
			flex: none;
			flex-direction: row;
			overflow-x: auto;
			padding: 0.4rem 0.75rem;
			border-right: 0;
			border-bottom: 1px solid var(--rule);
		}
		nav a[aria-current='page'] {
			box-shadow: inset 0 -3px 0 var(--blue);
		}
		.body {
			flex: 1;
		}
	}
</style>
