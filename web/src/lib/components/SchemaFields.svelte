<script lang="ts">
	import SchemaFields from './SchemaFields.svelte';
	import { inlineObject, referenced, typeName, unwrap, type Schema } from '$lib/openapi';

	/** The fields of an object schema. Inline objects nest; named ones link to their own section. */
	let { schema, request = false }: { schema: Schema; request?: boolean } = $props();

	const fields = $derived(Object.entries(schema.properties ?? {}).sort(([a], [b]) => a.localeCompare(b)));
	const required = $derived(new Set(schema.required ?? []));
</script>

{#if fields.length === 0}
	<p class="none">No fields.</p>
{:else}
	<dl>
		{#each fields as [name, field] (name)}
			{@const ref = referenced(field)}
			{@const inner = inlineObject(field)}
			<div>
				<dt>
					<code>{name}</code>
					<span class="type">
						{#if ref}<a href="#schema-{ref}">{typeName(field)}</a>{:else}{typeName(field)}{/if}
					</span>
					{#if !request && !required.has(name)}<span class="opt">may be absent</span>{/if}
				</dt>
				{#if field.description || unwrap(field).schema.description}
					<dd>{field.description ?? unwrap(field).schema.description}</dd>
				{/if}
				{#if inner && !ref}
					<dd class="nested"><SchemaFields schema={inner} {request} /></dd>
				{/if}
			</div>
		{/each}
	</dl>
{/if}

<style>
	dl {
		margin: 0;
		display: grid;
		gap: 0.5rem;
	}
	dt {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: 0.25rem 0.6rem;
	}
	dd {
		margin: 0.1rem 0 0;
		color: var(--ink-2);
		font-size: 0.875rem;
		overflow-wrap: anywhere;
	}
	.nested {
		margin: 0.4rem 0 0.2rem;
		padding-left: 0.9rem;
		border-left: 2px solid var(--rule);
	}
	code {
		font-weight: 700;
		overflow-wrap: anywhere;
	}
	.type {
		font-size: 0.85rem;
		color: var(--ink-2);
	}
	.opt {
		font-size: 0.8rem;
		color: var(--ink-2);
		font-style: italic;
	}
	.none {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
</style>
