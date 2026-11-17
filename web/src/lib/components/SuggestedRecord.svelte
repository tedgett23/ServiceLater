<script lang="ts">
	import type { Snippet } from 'svelte';
	import { forms } from '$lib/forms.svelte';
	import type { Suggestion } from '$lib/types';

	/**
	 * One record suggested as related: its reference and title, why it looks related, and, for a problem that is a known
	 * error, the workaround. The labels of the known-error and workaround fields come from the problem form, and they
	 * show only while that form has them. `action` is the button beside it (Link).
	 */
	let { s, newTab = false, action }: { s: Suggestion; newTab?: boolean; action?: Snippet } = $props();

	const form = $derived(s.type === 'problem' ? forms.of('problem') : undefined);
	const known = $derived(form?.fields.find((f) => f.name === 'known_error'));
	const workaround = $derived(form?.fields.find((f) => f.name === 'workaround'));
	$effect(() => {
		if (s.type === 'problem') void forms.ensure('problem');
	});
</script>

<li class="suggested">
	<div class="main">
		<a href="/tickets/{s.reference}" target={newTab ? '_blank' : undefined} rel={newTab ? 'noopener' : undefined}
			><span class="ref">{s.reference}</span> {s.title}</a
		>
		<span class="meta">
			{#if known && s.fields?.known_error === true}<span class="known">{known.label}</span>{/if}
			<span>{s.why}</span>
		</span>
		{#if workaround && s.fields?.known_error === true && typeof s.fields?.workaround === 'string'}
			<p class="workaround"><strong>{workaround.label}:</strong> {s.fields.workaround}</p>
		{/if}
	</div>
	{#if action}<div class="action">{@render action()}</div>{/if}
</li>

<style>
	.suggested {
		display: flex;
		align-items: flex-start;
		gap: 0.75rem;
		padding: 0.5rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.suggested:last-child {
		border-bottom: 0;
	}
	.main {
		flex: 1;
		min-width: 0;
		display: grid;
		gap: 0.15rem;
	}
	a {
		color: var(--ink);
		overflow-wrap: anywhere;
	}
	.ref {
		font-weight: 700;
	}
	.meta {
		display: flex;
		flex-wrap: wrap;
		gap: 0 0.75rem;
		color: var(--ink-2);
		font-size: 0.8125rem;
	}
	.known {
		padding: 0 0.35rem;
		border-radius: 4px;
		background: var(--marker-soft);
		color: var(--marker-ink);
		font-weight: 600;
	}
	.workaround {
		font-size: 0.875rem;
		color: var(--ink-2);
	}
	.workaround strong {
		color: var(--ink);
		font-weight: 600;
	}
	.action {
		flex: none;
	}
</style>
