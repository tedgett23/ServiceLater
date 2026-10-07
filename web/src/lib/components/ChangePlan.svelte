<script lang="ts">
	import { when } from '$lib/format';
	import type { ChangeFields } from '$lib/types';

	let {
		change,
		canEdit,
		save
	}: {
		change: ChangeFields;
		canEdit: boolean;
		/** Saves some of the fields; the parent applies them at once and reverts if the server says no. */
		save: (fields: Partial<ChangeFields>, done: string) => Promise<void>;
	} = $props();

	const kinds = [
		{ id: 'standard', label: 'Standard', hint: 'Pre-approved, low risk' },
		{ id: 'normal', label: 'Normal', hint: 'Goes to the change advisory board' },
		{ id: 'emergency', label: 'Emergency', hint: 'Fast, to a smaller board' }
	] as const;
	const levels = [
		{ id: '', label: 'Not assessed' },
		{ id: 'low', label: 'Low' },
		{ id: 'medium', label: 'Medium' },
		{ id: 'high', label: 'High' }
	] as const;
	const plans = [
		{ key: 'implementation_plan', label: 'Implementation plan', placeholder: 'What will be done, step by step?' },
		{ key: 'backout_plan', label: 'Back-out plan', placeholder: 'How is it undone if it goes wrong?' },
		{ key: 'test_plan', label: 'Test plan', placeholder: 'How will you know it worked?' }
	] as const;
	type PlanKey = (typeof plans)[number]['key'];

	const levelLabel = (v: string) => levels.find((l) => l.id === v)?.label ?? v;
	const kindLabel = (v: string) => kinds.find((k) => k.id === v)?.label ?? v;

	// Plans are typed in place and saved when focus leaves; while one is being typed in, a live update must not
	// overwrite it.
	let drafts = $state<Record<PlanKey, string>>({ implementation_plan: '', backout_plan: '', test_plan: '' });
	let editing = $state<PlanKey | null>(null);
	$effect(() => {
		for (const p of plans) if (editing !== p.key) drafts[p.key] = change[p.key];
	});

	async function savePlan(key: PlanKey, label: string) {
		editing = null;
		if (drafts[key] === change[key]) return;
		await save({ [key]: drafts[key] }, `${label} saved`);
	}

	/** An ISO time as a datetime-local input wants it, in the browser's time zone. */
	function local(iso: string | null) {
		if (!iso) return '';
		const d = new Date(iso);
		const pad = (n: number) => String(n).padStart(2, '0');
		return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
	}

	function saveWindow(key: 'planned_start' | 'planned_end', value: string) {
		const iso = value ? new Date(value).toISOString() : null;
		if (iso === change[key]) return;
		const what = key === 'planned_start' ? 'Planned start' : 'Planned end';
		void save({ [key]: iso }, iso ? `${what} set` : `${what} cleared`);
	}
</script>

<section class="change" aria-labelledby="change-h">
	<h2 id="change-h">Change</h2>
	<dl class="facts">
		<div>
			<dt id="kind-l">Type</dt>
			<dd>
				{#if canEdit}
					<select
						class="select"
						aria-labelledby="kind-l"
						value={change.change_type}
						onchange={(e) => {
							const v = e.currentTarget.value as ChangeFields['change_type'];
							void save({ change_type: v }, `Type set to ${kindLabel(v)}`);
						}}
					>
						{#each kinds as k (k.id)}<option value={k.id} title={k.hint}>{k.label}</option>{/each}
					</select>
				{:else}
					{kindLabel(change.change_type)}
				{/if}
			</dd>
		</div>
		{#each [{ key: 'risk', label: 'Risk' }, { key: 'impact', label: 'Impact' }] as const as f (f.key)}
			<div>
				<dt id="{f.key}-l">{f.label}</dt>
				<dd>
					{#if canEdit}
						<select
							class="select"
							aria-labelledby="{f.key}-l"
							value={change[f.key]}
							onchange={(e) => {
								const v = e.currentTarget.value as ChangeFields['risk'];
								void save({ [f.key]: v }, `${f.label} set to ${levelLabel(v)}`);
							}}
						>
							{#each levels as l (l.id)}<option value={l.id}>{l.label}</option>{/each}
						</select>
					{:else}
						<span class:unset={!change[f.key]}>{levelLabel(change[f.key])}</span>
					{/if}
				</dd>
			</div>
		{/each}
	</dl>
	<dl class="facts window">
		{#each [{ key: 'planned_start', label: 'Planned start' }, { key: 'planned_end', label: 'Planned end' }] as const as f (f.key)}
			<div>
				<dt id="{f.key}-l">{f.label}</dt>
				<dd>
					{#if canEdit}
						<input
							type="datetime-local"
							aria-labelledby="{f.key}-l"
							value={local(change[f.key])}
							onchange={(e) => saveWindow(f.key, e.currentTarget.value)}
						/>
					{:else if change[f.key]}
						<time datetime={change[f.key]}>{when(change[f.key]!)}</time>
					{:else}
						<span class="unset">Not planned</span>
					{/if}
				</dd>
			</div>
		{/each}
	</dl>

	{#each plans as p (p.key)}
		{#if canEdit}
			<label class="field plan">
				<span>{p.label}</span>
				<textarea
					bind:value={drafts[p.key]}
					onfocus={() => (editing = p.key)}
					onblur={() => savePlan(p.key, p.label)}
					maxlength="10000"
					placeholder={p.placeholder}
				></textarea>
			</label>
		{:else if change[p.key]}
			<div class="plan-read">
				<h3>{p.label}</h3>
				<p>{change[p.key]}</p>
			</div>
		{/if}
	{/each}
</section>

<style>
	.change {
		display: grid;
		gap: 1rem;
		padding-top: 1.25rem;
		border-top: 1px solid var(--rule);
	}
	h2 {
		font: 700 1.5rem/1 var(--font-display);
		letter-spacing: 0.01em;
	}
	.facts {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(9rem, 1fr));
		gap: 0.75rem 1rem;
		margin: 0;
	}
	/* Dates need the room: two to a row, or one each on a phone. */
	.window {
		grid-template-columns: repeat(auto-fill, minmax(14rem, 1fr));
	}
	dt {
		font-size: 0.85rem;
		font-weight: 600;
		margin-bottom: 0.35rem;
	}
	dd {
		margin: 0;
	}
	input[type='datetime-local'] {
		width: 100%;
		min-height: 2.5rem;
		padding: 0.4rem 0.5rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--surface);
		color: var(--ink);
		font: inherit;
		font-size: 0.9rem;
	}
	.unset {
		color: var(--ink-2);
	}
	.plan textarea {
		min-height: 4.5rem;
	}
	.plan-read h3 {
		font-size: 0.85rem;
		font-weight: 600;
		margin-bottom: 0.25rem;
	}
	.plan-read p {
		white-space: pre-wrap;
		overflow-wrap: anywhere;
	}
</style>
