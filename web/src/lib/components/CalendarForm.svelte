<script lang="ts">
	import { api } from '$lib/api';
	import type { BusinessCalendar } from '$lib/types';

	/** Adds a business calendar, or changes one when `calendar` is given. */
	let { calendar = null, onsaved, oncancel }: { calendar?: BusinessCalendar | null; onsaved: (c: BusinessCalendar) => void; oncancel: () => void } =
		$props();

	// Monday first, as a working week is read; the API counts from Sunday (0).
	const days = [
		{ day: 1, label: 'Monday' },
		{ day: 2, label: 'Tuesday' },
		{ day: 3, label: 'Wednesday' },
		{ day: 4, label: 'Thursday' },
		{ day: 5, label: 'Friday' },
		{ day: 6, label: 'Saturday' },
		{ day: 0, label: 'Sunday' }
	];
	const zones = (() => {
		try {
			return Intl.supportedValuesOf('timeZone');
		} catch {
			return ['UTC'];
		}
	})();

	type Stretch = { day: number; start: string; end: string };
	function initial() {
		if (calendar)
			return {
				name: calendar.name,
				label: calendar.label,
				zone: calendar.time_zone,
				hours: calendar.hours.map((h) => ({ ...h })),
				holidays: [...calendar.holidays]
			};
		const weekday = [1, 2, 3, 4, 5].map((day) => ({ day, start: '09:00', end: '17:00' }));
		return { name: '', label: '', zone: Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC', hours: weekday as Stretch[], holidays: [] as string[] };
	}
	const draft = $state(initial());
	let holiday = $state('');
	let saving = $state(false);
	let error = $state('');

	const of = (day: number) => draft.hours.filter((h) => h.day === day);
	const add = (day: number) => draft.hours.push({ day, start: '09:00', end: '17:00' });
	const remove = (h: Stretch) => draft.hours.splice(draft.hours.indexOf(h), 1);
	function addHoliday() {
		if (holiday && !draft.holidays.includes(holiday)) draft.holidays = [...draft.holidays, holiday].sort();
		holiday = '';
	}
	const dayName = (iso: string) =>
		new Date(`${iso}T12:00:00Z`).toLocaleDateString(undefined, { weekday: 'short', day: 'numeric', month: 'short', year: 'numeric', timeZone: 'UTC' });

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		error = '';
		saving = true;
		try {
			const body = { name: draft.name.trim(), label: draft.label.trim(), time_zone: draft.zone, hours: draft.hours, holidays: draft.holidays };
			const saved = calendar
				? await api<BusinessCalendar>(`/api/v1/business-calendars/${calendar.id}`, { method: 'PATCH', body })
				: await api<BusinessCalendar>('/api/v1/business-calendars', { method: 'POST', body });
			onsaved(saved);
		} catch (err) {
			error = err instanceof Error ? err.message : 'Could not save the calendar';
		} finally {
			saving = false;
		}
	}
</script>

<form class="calendar-form" onsubmit={submit} aria-label={calendar ? `Change ${calendar.label}` : 'Add a calendar'}>
	<label class="field">
		<span>Label</span>
		<input bind:value={draft.label} required maxlength="200" placeholder="UK office" />
	</label>
	<label class="field">
		<span>Name <em>(what SLA definitions write after calendar:)</em></span>
		<input bind:value={draft.name} required maxlength="63" placeholder="uk-office" />
	</label>
	<label class="field wide">
		<span>Time zone</span>
		<input bind:value={draft.zone} required list="calendar-zones" />
		<datalist id="calendar-zones">
			{#each zones as z (z)}<option value={z}></option>{/each}
		</datalist>
	</label>

	<fieldset class="wide">
		<legend>Working hours</legend>
		{#each days as d (d.day)}
			<div class="day">
				<span class="dayname">{d.label}</span>
				<div class="stretches">
					{#each of(d.day) as h, i (i)}
						<span class="stretch">
							<input type="time" bind:value={h.start} required aria-label="{d.label} from" />
							<span aria-hidden="true">to</span>
							<input type="time" bind:value={h.end} required aria-label="{d.label} until" />
							<button type="button" class="btn quiet" onclick={() => remove(h)} aria-label="Remove {d.label} {h.start} to {h.end}">Remove</button>
						</span>
					{:else}
						<span class="closed">Closed</span>
					{/each}
				</div>
				<button type="button" class="btn quiet" onclick={() => add(d.day)} aria-label="Add hours on {d.label}">Add hours</button>
			</div>
		{/each}
	</fieldset>

	<fieldset class="wide">
		<legend>Holidays</legend>
		{#if draft.holidays.length}
			<ul class="holidays">
				{#each draft.holidays as h (h)}
					<li>
						<time datetime={h}>{dayName(h)}</time>
						<button type="button" class="btn quiet" onclick={() => (draft.holidays = draft.holidays.filter((x) => x !== h))} aria-label="Remove {dayName(h)}"
							>Remove</button
						>
					</li>
				{/each}
			</ul>
		{:else}
			<p class="closed">No holidays.</p>
		{/if}
		<div class="add-holiday">
			<input type="date" bind:value={holiday} aria-label="Holiday date" />
			<button type="button" class="btn" disabled={!holiday} onclick={addHoliday}>Add holiday</button>
		</div>
	</fieldset>

	{#if error}<p class="error wide" role="alert">{error}</p>{/if}
	<div class="actions wide">
		<button class="btn primary" disabled={saving}>{calendar ? 'Save calendar' : 'Add calendar'}</button>
		<button type="button" class="btn" onclick={oncancel}>Cancel</button>
	</div>
</form>

<style>
	.calendar-form {
		display: grid;
		grid-template-columns: repeat(2, minmax(0, 1fr));
		gap: 0.75rem 1rem;
		padding: 1rem 0;
	}
	.wide {
		grid-column: 1 / -1;
	}
	em {
		font-style: normal;
		font-weight: 400;
		color: var(--ink-2);
	}
	fieldset {
		margin: 0;
		padding: 0;
		border: 0;
		display: grid;
		gap: 0.4rem;
	}
	legend {
		margin-bottom: 0.4rem;
		font-weight: 600;
		font-size: 0.875rem;
	}
	.day {
		display: grid;
		grid-template-columns: 6.5rem minmax(0, 1fr) auto;
		align-items: center;
		gap: 0.5rem;
		padding: 0.3rem 0;
		border-bottom: 1px solid var(--rule);
	}
	.dayname {
		font-weight: 600;
		font-size: 0.9rem;
	}
	.stretches {
		display: grid;
		gap: 0.3rem;
	}
	.stretch {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.4rem;
		font-size: 0.875rem;
	}
	.stretch input {
		width: 7.5rem;
	}
	.closed {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.btn.quiet {
		min-height: 2rem;
		padding: 0 0.5rem;
		font-weight: 500;
		border-color: transparent;
		background: transparent;
		color: var(--blue);
	}
	.holidays {
		list-style: none;
		margin: 0;
		padding: 0;
		display: grid;
		gap: 0.15rem;
	}
	.holidays li {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.5rem;
		font-size: 0.9rem;
	}
	.add-holiday {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
	.actions {
		display: flex;
		gap: 0.5rem;
	}
	@media (max-width: 40rem) {
		.calendar-form {
			grid-template-columns: minmax(0, 1fr);
		}
		.day {
			grid-template-columns: minmax(0, 1fr) auto;
		}
		.stretches {
			grid-column: 1 / -1;
			grid-row: 2;
		}
	}
</style>
