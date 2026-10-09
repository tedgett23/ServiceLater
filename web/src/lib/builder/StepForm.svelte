<script lang="ts">
	import { lifecycle } from '$lib/lifecycle.svelte';
	import { exitsOf, parseValue, stepTypes, typeInfo, valueText, type NamedValue, type Step, type StepType } from './workflow';

	/**
	 * The selected step's fields, by its type. Each change is reported when the person is done with a field (on
	 * change), so renaming a step does not rename it at every keystroke. Expressions are CEL, as in the YAML; the server
	 * checks them when the workflow is saved.
	 */
	let {
		step,
		steps,
		recordType,
		isStart,
		editable,
		onchange,
		onstart,
		ondelete
	}: {
		step: Step;
		steps: Step[];
		recordType: string;
		isStart: boolean;
		editable: boolean;
		onchange: (s: Step) => void;
		onstart: () => void;
		ondelete: () => void;
	} = $props();

	const info = $derived(typeInfo(step.type));
	const others = $derived(steps.filter((s) => s.id !== step.id));
	const statuses = $derived(lifecycle.byType[recordType]?.states ?? []);

	function set<K extends keyof Step>(key: K, value: Step[K]) {
		const next = { ...$state.snapshot(step), [key]: value } as Step;
		if (value === '' || value === undefined) delete next[key];
		onchange(next);
	}

	function setType(t: StepType) {
		const next: Step = { id: step.id, type: t };
		// Keep where it goes next when the new type goes somewhere.
		if (step.next && typeInfo(t)?.exits.some((e) => e.field === 'next')) next.next = step.next;
		if (t === 'comment.add') next.body = { literal: '' };
		if (t === 'task.create') next.title = { literal: '' };
		onchange(next);
	}

	function setList(key: 'vars' | 'set', list: NamedValue[]) {
		set(key, list);
	}
	const list = (key: 'vars' | 'set') => (step[key] ?? []) as NamedValue[];
</script>

<div class="form">
	<div class="head">
		<h2>{step.id}</h2>
		{#if isStart}<span class="badge">Start</span>{/if}
	</div>
	<p class="hint">{info?.help}</p>

	<div class="field">
		<label for="step-id">Step id</label>
		<input id="step-id" value={step.id} disabled={!editable} onchange={(e) => e.currentTarget.value.trim() && set('id', e.currentTarget.value.trim())} />
	</div>
	<div class="field">
		<label for="step-type">Kind of step</label>
		<select id="step-type" value={step.type} disabled={!editable} onchange={(e) => setType(e.currentTarget.value as StepType)}>
			{#each stepTypes as t (t.type)}<option value={t.type}>{t.label}</option>{/each}
		</select>
	</div>

	{#if step.type === 'condition'}
		<div class="field">
			<label for="step-if">If</label>
			<textarea id="step-if" rows="2" value={step.if ?? ''} disabled={!editable} placeholder={'record.priority <= 2'} onchange={(e) => set('if', e.currentTarget.value)}></textarea>
		</div>
	{/if}

	{#if step.type === 'wait'}
		<div class="field">
			<label for="step-for">For</label>
			<input id="step-for" value={step.for ?? ''} disabled={!editable} placeholder="2h, 3d" onchange={(e) => set('for', e.currentTarget.value.trim())} />
		</div>
		<div class="field">
			<label for="step-until">Or until</label>
			<textarea id="step-until" rows="2" value={step.until ?? ''} disabled={!editable} placeholder={'record.state_category == "done"'} onchange={(e) => set('until', e.currentTarget.value)}></textarea>
		</div>
	{/if}

	{#if step.type === 'approval'}
		<div class="field">
			<label for="step-approvers">Approvers</label>
			<textarea id="step-approvers" rows="2" value={step.approvers ?? ''} disabled={!editable} placeholder={"group('CAB')"} onchange={(e) => set('approvers', e.currentTarget.value)}></textarea>
			<small class="hint">An expression giving user ids, each with a vote: group('name'), role('name'), [record.assignee_id]…</small>
		</div>
		<div class="field">
			<label for="step-groups">Groups</label>
			<textarea id="step-groups" rows="2" value={step.groups ?? ''} disabled={!editable} placeholder={'record.cis.filter(c, has(c.owner_group)).map(c, c.owner_group)'} onchange={(e) => set('groups', e.currentTarget.value)}></textarea>
			<small class="hint">An expression giving group names. Each group has one vote, cast by whichever member answers first.</small>
		</div>
		<div class="field">
			<label for="step-rule">Decided by</label>
			<select id="step-rule" value={step.rule ?? 'any'} disabled={!editable} onchange={(e) => set('rule', e.currentTarget.value === 'any' ? '' : e.currentTarget.value)}>
				<option value="any">Any one vote</option>
				<option value="majority">A majority of the votes</option>
				<option value="all">Every vote</option>
			</select>
		</div>
	{/if}

	{#if step.type === 'task.create'}
		<div class="field">
			<label for="step-title">Task</label>
			<input id="step-title" value={valueText(step.title)} disabled={!editable} placeholder={'="Order a laptop for " + record.requester_name'} onchange={(e) => set('title', parseValue(e.currentTarget.value))} />
			<small class="hint">Text, or =an expression.</small>
		</div>
		<div class="field">
			<label for="step-description">Details</label>
			<textarea id="step-description" rows="2" value={valueText(step.description)} disabled={!editable} onchange={(e) => set('description', parseValue(e.currentTarget.value))}></textarea>
		</div>
		<div class="field">
			<label for="step-group">Team</label>
			<input id="step-group" value={valueText(step.group)} disabled={!editable} placeholder="Service Desk" onchange={(e) => set('group', parseValue(e.currentTarget.value))} />
			<small class="hint">A group’s name. A group that does not exist leaves the task unrouted.</small>
		</div>
		<label class="check"><input type="checkbox" checked={step.wait !== false} disabled={!editable} onchange={(e) => set('wait', e.currentTarget.checked ? undefined : false)} /> Wait until it is done</label>
	{/if}

	{#if step.type === 'approval' || (step.type === 'wait' && step.until) || (step.type === 'task.create' && step.wait !== false)}
		<div class="field">
			<label for="step-timeout">Time limit</label>
			<input id="step-timeout" value={step.timeout ?? ''} disabled={!editable} placeholder="5d" onchange={(e) => set('timeout', e.currentTarget.value.trim())} />
		</div>
	{/if}

	{#if step.type === 'record.transition'}
		<div class="field">
			<label for="step-to">Move to</label>
			<select id="step-to" value={step.to ?? ''} disabled={!editable} onchange={(e) => set('to', e.currentTarget.value)}>
				<option value="">Choose a status</option>
				{#each statuses as st (st.id)}<option value={st.id}>{st.label}</option>{/each}
			</select>
		</div>
	{/if}

	{#if step.type === 'comment.add'}
		<div class="field">
			<label for="step-body">Comment</label>
			<textarea id="step-body" rows="3" value={valueText(step.body)} disabled={!editable} onchange={(e) => set('body', parseValue(e.currentTarget.value))}></textarea>
			<small class="hint">Text, or =an expression.</small>
		</div>
		<label class="check"><input type="checkbox" checked={!!step.internal} disabled={!editable} onchange={(e) => set('internal', e.currentTarget.checked)} /> Internal note</label>
	{/if}

	{#if step.type === 'set' || step.type === 'record.update'}
		{@const key = step.type === 'set' ? 'vars' : 'set'}
		<fieldset>
			<legend>{step.type === 'set' ? 'Variables' : 'Fields to change'}</legend>
			{#each list(key) as nv, i (i)}
				<div class="pair">
					<input
						aria-label="Name {i + 1}"
						value={nv.name}
						disabled={!editable}
						placeholder={step.type === 'set' ? 'name' : 'priority'}
						onchange={(e) => setList(key, list(key).map((x, j) => (j === i ? { ...x, name: e.currentTarget.value.trim() } : x)))}
					/>
					<input
						aria-label="Value {i + 1}"
						value={valueText(nv.value)}
						disabled={!editable}
						placeholder="text, 2, true or =expression"
						onchange={(e) => setList(key, list(key).map((x, j) => (j === i ? { ...x, value: parseValue(e.currentTarget.value) } : x)))}
					/>
					{#if editable}
						<button type="button" class="btn" aria-label="Remove {nv.name || `row ${i + 1}`}" onclick={() => setList(key, list(key).filter((_, j) => j !== i))}>×</button>
					{/if}
				</div>
			{/each}
			{#if editable}
				<button type="button" class="btn" onclick={() => setList(key, [...list(key), { name: '', value: null }])}>Add {step.type === 'set' ? 'a variable' : 'a field'}</button>
			{/if}
		</fieldset>
	{/if}

	{#if exitsOf(step).length}
		<fieldset>
			<legend>Then</legend>
			{#each exitsOf(step) as ex (ex.field)}
				<div class="field">
					<label for="step-{ex.field}">{ex.label === 'next' ? 'Next step' : ex.label}</label>
					<select id="step-{ex.field}" value={step[ex.field] ?? ''} disabled={!editable} onchange={(e) => set(ex.field, e.currentTarget.value)}>
						<option value="">{ex.field === 'on_timeout' ? 'Carry on to the next step' : 'Not connected'}</option>
						{#each others as o (o.id)}<option value={o.id}>{o.id}</option>{/each}
					</select>
				</div>
			{/each}
		</fieldset>
	{/if}

	{#if editable}
		<div class="actions">
			{#if !isStart}<button type="button" class="btn" onclick={onstart}>Start here</button>{/if}
			<button type="button" class="btn reject" onclick={ondelete}>Delete step</button>
		</div>
	{/if}
</div>

<style>
	.form {
		display: grid;
		gap: 0.7rem;
	}
	.head {
		display: flex;
		align-items: center;
		gap: 0.5rem;
	}
	h2 {
		font: 700 1.2rem/1.2 var(--font-display);
		overflow-wrap: anywhere;
	}
	.badge {
		padding: 0.05rem 0.45rem;
		border-radius: var(--radius);
		background: var(--blue-soft);
		font-size: 0.8rem;
		font-weight: 600;
	}
	.hint {
		color: var(--ink-2);
		font-size: 0.8rem;
	}
	textarea {
		font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
		font-size: 0.85rem;
		min-height: 0;
	}
	fieldset {
		display: grid;
		gap: 0.5rem;
		margin: 0;
		padding: 0.6rem 0 0;
		border: 0;
		border-top: 1px solid var(--rule);
		min-width: 0;
	}
	legend {
		padding: 0;
		font-size: 0.85rem;
		font-weight: 700;
	}
	.pair {
		display: grid;
		grid-template-columns: minmax(0, 1fr) minmax(0, 1.4fr) auto;
		gap: 0.35rem;
	}
	.pair input {
		min-width: 0;
		min-height: 2.25rem;
		padding: 0.35rem 0.5rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--raised);
		font-size: 0.85rem;
	}
	.check {
		display: flex;
		align-items: center;
		gap: 0.45rem;
	}
	.actions {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
		padding-top: 0.6rem;
		border-top: 1px solid var(--rule);
	}
	.btn.reject {
		color: var(--red);
	}
</style>
