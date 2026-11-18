<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import type { SimpleField } from '$lib/conditions';
	import SlaHoursEditor from '$lib/components/SlaHoursEditor.svelte';
	import { priorityLabel } from '$lib/format';
	import { forms } from '$lib/forms.svelte';
	import { lifecycle } from '$lib/lifecycle.svelte';
	import type { BusinessCalendar, Group, SlaDef } from '$lib/types';

	let slas = $state<SlaDef[]>([]);
	let calendars = $state<BusinessCalendar[]>([]);
	let groups = $state<Group[]>([]);
	let loading = $state(true);
	let error = $state('');

	onMount(async () => {
		try {
			const [s, c, g] = await Promise.all([
				api<{ items: SlaDef[] }>('/api/v1/slas'),
				api<{ items: BusinessCalendar[] }>('/api/v1/business-calendars'),
				api<{ items: Group[] }>('/api/v1/groups'),
				lifecycle.ensure()
			]);
			slas = s.items;
			calendars = c.items;
			groups = g.items;
			await Promise.all([...new Set(slas.map((x) => x.record_type))].map((t) => forms.ensure(t)));
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load the SLAs';
		} finally {
			loading = false;
		}
	});

	/** What a condition can test on a type's records: priority, status, assignment group, and its form's fields. */
	function fieldsFor(type: string): SimpleField[] {
		const out: SimpleField[] = [
			{ name: 'priority', label: 'Priority', type: 'priority', options: Object.entries(priorityLabel).map(([value, label]) => ({ value, label })) },
			{ name: 'assignment_group', label: 'Assignment group', type: 'group', options: groups.map((g) => ({ value: g.name, label: g.name })) }
		];
		const states = lifecycle.byType[type]?.states ?? [];
		if (states.length) out.push({ name: 'status', label: 'Status', type: 'status', options: states.map((s) => ({ value: s.id, label: s.label })) });
		for (const f of forms.of(type)?.fields ?? []) {
			if (f.core || !['choice', 'boolean', 'text', 'number'].includes(f.type)) continue;
			out.push({ name: f.name, label: f.label, type: f.type as SimpleField['type'], options: f.type === 'choice' ? f.options : undefined });
		}
		return out;
	}
</script>

<svelte:head><title>SLAs – ServiceLater</title></svelte:head>

<div class="page">
	<h1>SLAs</h1>
	<p class="lede">
		Which hours each SLA counts. The first rule that holds for a record decides, and it is checked again on every save: a
		record handed to another team counts from then on in that team's hours, keeping the time already used. Clocks that
		are running keep their rules until their record next changes.
	</p>
	<p class="hint">
		Hours come from <a href="/admin/calendars">Calendars</a>; a group's own hours are set on its page under
		<a href="/admin/groups">Groups</a>.
	</p>

	{#if error}
		<p class="msg" role="alert">{error}</p>
	{:else if !loading && slas.length === 0}
		<p class="msg">No installed app defines an SLA.</p>
	{/if}

	{#if !loading}
		<div class="list">
			{#each slas as s, i (s.app + '/' + s.name)}
				<SlaHoursEditor sla={s} {calendars} fields={fieldsFor(s.record_type)} onsaved={(x) => (slas[i] = x)} />
			{/each}
		</div>
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 60rem;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
	}
	.lede {
		margin: 0.25rem 0 0.35rem;
		color: var(--ink-2);
	}
	.hint {
		margin-bottom: 1rem;
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.msg {
		color: var(--ink-2);
	}
	.list {
		display: grid;
		gap: 0.9rem;
	}
</style>
