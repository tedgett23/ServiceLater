<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import type { RoleInfo } from '$lib/types';

	let roles = $state<RoleInfo[]>([]);
	let error = $state('');

	onMount(async () => {
		try {
			roles = (await api<{ items: RoleInfo[] }>('/api/v1/roles')).items;
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load roles';
		}
	});

	const base = $derived(roles.filter((r) => r.built_in));
	const extra = $derived(roles.filter((r) => !r.built_in));
	const many = (n: number, one: string, more: string) => (n === 1 ? `1 ${one}` : `${n} ${more}`);
</script>

<svelte:head><title>Roles – ServiceLater</title></svelte:head>

<div class="page">
	<h1>Roles</h1>
	<p class="lede">
		Everyone has one base role. Other roles add permissions on top, for a person or for everyone in a group. Apps ship
		them in <code>roles/</code>, and you can add your own in an app's <code>local/</code> folder.
	</p>

	{#if error}<p class="msg" role="alert">{error}</p>{/if}

	{#snippet list(items: RoleInfo[])}
		<ul class="roles">
			{#each items as r (r.name)}
				<li>
					<a href="/admin/roles/{r.name}">
						<span class="name">{r.label}</span>
						<span class="count">
							{many(r.permissions.length, 'permission', 'permissions')}{#if !r.built_in}, {many(r.grants, 'grant', 'grants')}{/if}
						</span>
						<span class="desc">{r.description || r.name}{#if r.app}{' '}· from {r.app}{/if}</span>
					</a>
				</li>
			{/each}
		</ul>
	{/snippet}

	<h2>Base roles</h2>
	{@render list(base)}
	<h2>Added roles</h2>
	{#if roles.length > 0 && extra.length === 0}<p class="msg">No app defines one yet.</p>{/if}
	{@render list(extra)}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 48rem;
	}
	h1 {
		font: 800 1.6rem/1.2 var(--font-display);
	}
	h2 {
		font: 700 1.25rem/1.2 var(--font-display);
		margin-top: 1.5rem;
	}
	.lede {
		margin: 0.25rem 0 0.5rem;
		color: var(--ink-2);
	}
	code {
		font-size: 0.85em;
	}
	.roles {
		list-style: none;
		margin: 0.25rem 0 0;
		padding: 0;
	}
	.roles li {
		border-bottom: 1px solid var(--rule);
	}
	.roles a {
		display: grid;
		grid-template-columns: minmax(0, 1fr) auto;
		column-gap: 1rem;
		padding: 0.65rem 0.25rem;
		color: var(--ink);
		text-decoration: none;
	}
	.roles a:hover .name {
		text-decoration: underline;
	}
	.name {
		font-weight: 700;
	}
	.count,
	.desc {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.count {
		white-space: nowrap;
	}
	.desc {
		grid-column: 1 / -1;
		overflow-wrap: anywhere;
	}
	.msg {
		padding: 0.5rem 0;
		color: var(--ink-2);
	}
</style>
