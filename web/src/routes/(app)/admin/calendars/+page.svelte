<script lang="ts">
	import { onMount } from 'svelte';
	import { api } from '$lib/api';
	import CalendarForm from '$lib/components/CalendarForm.svelte';
	import { week } from '$lib/calendars';
	import type { BusinessCalendar } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	let list = $state<BusinessCalendar[]>([]);
	let loading = $state(true);
	let error = $state('');
	/** Which calendar is open for editing: an id, 'new', or nothing. */
	let editing = $state<string | null>(null);
	let removing = $state<string | null>(null);
	let removeError = $state('');

	async function load() {
		try {
			list = (await api<{ items: BusinessCalendar[] }>('/api/v1/business-calendars')).items;
			error = '';
		} catch (e) {
			error = e instanceof Error ? e.message : 'Could not load calendars';
		} finally {
			loading = false;
		}
	}
	onMount(load);

	function saved(c: BusinessCalendar) {
		ui.announce = `${c.label} saved`;
		editing = null;
		void load();
	}

	async function remove(c: BusinessCalendar) {
		removeError = '';
		try {
			await api(`/api/v1/business-calendars/${c.id}`, { method: 'DELETE' });
			ui.announce = `${c.label} removed`;
			removing = null;
			await load();
		} catch (e) {
			removeError = e instanceof Error ? e.message : 'Could not remove the calendar';
		}
	}
</script>

<svelte:head><title>Calendars – ServiceLater</title></svelte:head>

<div class="page">
	<h1>Calendars</h1>
	<p class="lede">
		The working hours SLA clocks count; hours outside them, and holidays, do not. Each SLA chooses its hours under
		<a href="/admin/slas">SLAs</a>, and a group can have its own on its page.
	</p>

	{#if error}
		<p class="msg" role="alert">{error}</p>
	{:else if !loading && list.length === 0}
		<p class="msg">No calendars yet. SLAs without one count every hour.</p>
	{/if}

	<ul class="calendars">
		{#each list as c (c.id)}
			<li class:editing={editing === c.id}>
				{#if editing === c.id}
					<CalendarForm calendar={c} onsaved={saved} oncancel={() => (editing = null)} />
				{:else}
					<div class="row">
						<div class="what">
							<span class="label">{c.label}</span>
							<code>{c.name}</code>
						</div>
						<div class="buttons">
							<button class="btn" onclick={() => ((editing = c.id), (removing = null))} aria-label="Edit {c.label}">Edit</button>
							<button class="btn" onclick={() => ((removing = c.id), (removeError = ''))} aria-label="Remove {c.label}">Remove</button>
						</div>
						<p class="week">{week(c)} <span class="zone">{c.time_zone}</span></p>
						<p class="holidays">{c.holidays.length === 1 ? '1 holiday' : `${c.holidays.length} holidays`}</p>
					</div>
					{#if removing === c.id}
						<div class="confirm" role="group" aria-label="Remove {c.label}">
							<p>SLAs that name <code>{c.name}</code> will count every hour. A calendar running clocks still count by cannot be removed.</p>
							{#if removeError}<p class="error" role="alert">{removeError}</p>{/if}
							<div class="buttons">
								<button class="btn danger" onclick={() => remove(c)}>Remove {c.label}</button>
								<button class="btn" onclick={() => (removing = null)}>Keep it</button>
							</div>
						</div>
					{/if}
				{/if}
			</li>
		{/each}
	</ul>

	{#if editing === 'new'}
		<CalendarForm onsaved={saved} oncancel={() => (editing = null)} />
	{:else}
		<p class="add"><button class="btn primary" onclick={() => (editing = 'new')}>Add calendar</button></p>
	{/if}
</div>

<style>
	.page {
		height: 100%;
		overflow-y: auto;
		padding: 1rem 1.25rem 2rem;
		max-width: 48rem;
	}
	h1 {
		font: 650 1.375rem/1.3 var(--font-display);
	}
	.lede {
		margin: 0.25rem 0 1rem;
		color: var(--ink-2);
	}
	.calendars {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.calendars li {
		border-bottom: 1px solid var(--rule);
	}
	.row {
		display: grid;
		grid-template-columns: minmax(0, 1fr) auto;
		align-items: center;
		column-gap: 1rem;
		row-gap: 0.2rem;
		padding: 0.75rem 0.25rem;
	}
	.what {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: 0.6rem;
	}
	.label {
		font-weight: 700;
		overflow-wrap: anywhere;
	}
	code {
		font-size: 0.8rem;
		color: var(--ink-2);
	}
	.buttons {
		display: flex;
		gap: 0.4rem;
	}
	.week,
	.holidays {
		grid-column: 1 / -1;
		font-size: 0.875rem;
		color: var(--ink-2);
		overflow-wrap: anywhere;
	}
	.zone {
		white-space: nowrap;
		margin-left: 0.4rem;
	}
	.confirm {
		display: grid;
		gap: 0.5rem;
		margin: 0 0 0.75rem;
		padding: 0.6rem 0.75rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
		font-size: 0.9rem;
	}
	.btn.danger {
		color: var(--red);
	}
	.add {
		margin-top: 1.25rem;
	}
	.msg {
		padding: 0.5rem 0;
		color: var(--ink-2);
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
</style>
