<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { api, ApiError } from '$lib/api';
	import FormEditor from '$lib/components/FormEditor.svelte';
	import type { FormDefResponse } from '$lib/formEditor';

	let def = $state<FormDefResponse | null>(null);
	let error = $state('');
	let version = $state(0);

	onMount(async () => {
		try {
			def = await api<FormDefResponse>(`/api/v1/forms/${page.params.type}/definition`);
		} catch (e) {
			error = e instanceof ApiError && e.status === 404 ? 'There is no such record type.' : e instanceof Error ? e.message : 'The form could not be loaded.';
		}
	});
</script>

<svelte:head><title>{def ? `${def.label} form` : 'Form'} – ServiceLater</title></svelte:head>

<div class="page">
	<nav class="crumbs" aria-label="Breadcrumb">
		<ol>
			<li><a href="/admin/forms">Forms</a></li>
			<li aria-current="page">{def?.label ?? page.params.type}</li>
		</ol>
	</nav>
	{#if error}
		<p class="error" role="alert">{error}</p>
	{:else if def}
		<h1>{def.label} form</h1>
		<!-- Keyed, so a save or an undo starts the editor again from what the server has. -->
		{#key version}
			<FormEditor
				{def}
				onsaved={(d) => {
					def = d;
					version++;
				}}
			/>
		{/key}
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 3rem;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
		margin-bottom: 0.75rem;
	}
	.crumbs ol {
		display: flex;
		flex-wrap: wrap;
		gap: 0.35rem;
		margin: 0 0 0.4rem;
		padding: 0;
		list-style: none;
		font-size: 0.8125rem;
		color: var(--ink-2);
	}
	.crumbs li + li::before {
		content: '/';
		margin-right: 0.35rem;
		color: color-mix(in srgb, var(--ink-2) 55%, transparent);
	}
	.crumbs a {
		color: var(--ink-2);
		font-weight: 550;
		text-decoration: none;
	}
	.crumbs a:hover {
		color: var(--ink);
		text-decoration: underline;
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
</style>
