<script lang="ts">
	import FieldInput from '$lib/components/FieldInput.svelte';
	import { display, valueOf, wide } from '$lib/fields';
	import type { FieldValue, FormDef, FormField, Ticket } from '$lib/types';

	/**
	 * The record's sections, as its type's form lays them out. Those who may edit it change a field in place (it saves
	 * when they are done with it); everyone else reads the values, and empty fields are left out. What is shown,
	 * required or read-only is the ticket's field_states, from the server.
	 */
	let {
		ticket,
		form,
		canEdit,
		save
	}: {
		ticket: Ticket;
		form: FormDef;
		canEdit: boolean;
		save: (field: FormField, value: FieldValue | null) => void;
	} = $props();

	const byName = $derived(new Map(form.fields.map((f) => [f.name, f])));
	const states = $derived(ticket.field_states ?? {});

	/** The section's fields to show: not hidden, and for readers only those with a value. */
	function shown(names: string[]) {
		const out: FormField[] = [];
		for (const n of names) {
			const f = byName.get(n);
			if (!f || states[n]?.hidden) continue;
			if (!canEdit && display(f, valueOf(ticket, f)) === '') continue;
			out.push(f);
		}
		return out;
	}

	const slug = (s: string) => s.toLowerCase().replace(/[^a-z0-9]+/g, '-');
</script>

{#each form.detail as section, i (i)}
	{@const fields = shown(section.fields)}
	{#if fields.length}
		<section class="record-section" aria-labelledby={section.label ? `section-${slug(section.label)}` : undefined}>
			{#if section.label}<h2 id="section-{slug(section.label)}">{section.label}</h2>{/if}
			<div class="grid">
				{#each fields as f (f.name)}
					{@const st = states[f.name] ?? {}}
					{#if canEdit}
						<div class="field" class:wide={wide(f)}>
							<label for="field-{f.name}">{f.label}{#if st.required}<span class="req" aria-hidden="true"> *</span>{/if}</label>
							<FieldInput
								field={f}
								value={valueOf(ticket, f)}
								fieldState={st}
								id="field-{f.name}"
								disabled={!!st.read_only}
								onchange={(v) => save(f, v)}
							/>
						</div>
					{:else}
						<div class="read" class:wide={wide(f)}>
							<h3>{f.label}</h3>
							<p>{display(f, valueOf(ticket, f))}</p>
						</div>
					{/if}
				{/each}
			</div>
		</section>
	{/if}
{/each}

<style>
	.record-section {
		display: grid;
		gap: 1rem;
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
	}
	h2 {
		font: 700 1.5rem/1 var(--font-display);
		letter-spacing: 0.01em;
	}
	.grid {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(15rem, 1fr));
		gap: 0.9rem 1rem;
		align-items: start;
	}
	.wide {
		grid-column: 1 / -1;
	}
	.req {
		color: var(--red);
	}
	.read h3 {
		font-size: 0.85rem;
		font-weight: 600;
		margin-bottom: 0.25rem;
	}
	.read p {
		white-space: pre-wrap;
		overflow-wrap: anywhere;
	}
</style>
