<script lang="ts">
	import { untrack } from 'svelte';
	import { api, ApiError } from '$lib/api';
	import { questionTypes, type DefField } from '$lib/catalogEditor';
	import ConditionInput from '$lib/components/ConditionInput.svelte';
	import FieldInput from '$lib/components/FieldInput.svelte';
	import FieldSettings from '$lib/components/FieldSettings.svelte';
	import FormGrid from '$lib/components/FormGrid.svelte';
	import { describe, holds, parseSimple } from '$lib/conditions';
	import { wide as wideField } from '$lib/fields';
	import { when } from '$lib/format';
	import {
		canMove,
		forSave,
		formSimpleFields,
		freshName,
		locate,
		moveBy,
		putIn,
		setColumns,
		sync,
		takeOut,
		type FormDefResponse,
		type LayoutKey
	} from '$lib/formEditor';
	import { lifecycle } from '$lib/lifecycle.svelte';
	import type { FieldValue, FormField, FormSection } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	/**
	 * Edits a record type's form: its fields and their conditions, laid out by dragging (or the arrow buttons) in the
	 * sections of the record page and of the New ticket dialog, each section flowing on its own or in two or three set
	 * columns, with a preview beside. Saving keeps only what changed in the app's studio layer; the server checks the
	 * form as an install would, and is asked as it is edited so problems show before saving.
	 */
	let { def, onsaved }: { def: FormDefResponse; onsaved: (d: FormDefResponse) => void } = $props();

	let form = $state(
		untrack(() => {
			const f = structuredClone($state.snapshot(def.form));
			f.validate ??= []; // left out when there are none
			// Somewhere to add fields to; an empty section is not saved.
			if (!f.detail.length) f.detail.push({ fields: [] });
			if (!f.create_sections.length) f.create_sections.push({ fields: [...f.create] });
			return f;
		})
	);
	/** Fields taken off the form: shown struck through until saved, and put back with one click. */
	let removed = $state(new Set<string>());
	let tab = $state<LayoutKey | 'checks'>('detail');
	let open = $state<string | null>(null);
	let busy = $state(false);
	let error = $state('');
	let problems = $state<string[]>([]);
	let confirmingReset = $state(false);
	let preview = $state<Record<string, FieldValue | null>>({});
	let dragging = $state<string | null>(null);
	let dropAt = $state<string | null>(null);
	/** On a phone the fields and the preview take turns. */
	let phoneView = $state<'fields' | 'preview'>('fields');

	const core = $derived(new Map(def.core_fields.map((c) => [c.name, c])));
	const coreNames = $derived(new Set(def.core_fields.map((c) => c.name)));
	const saved = $derived(new Set(def.form.fields.map((f) => f.name)));
	const statuses = $derived(lifecycle.byType[def.record_type]?.states ?? []);
	const layout = $derived(tab === 'checks' ? 'detail' : tab);
	const sections = $derived(form[layout]);
	const dirty = $derived(JSON.stringify(forSave(form, removed, coreNames)) !== JSON.stringify(forSave(def.form, new Set(), coreNames)));

	$effect(() => {
		void lifecycle.ensure();
	});

	const defOf = (name: string) => form.fields.find((f) => f.name === name);
	const labelOf = (name: string) => defOf(name)?.label || core.get(name)?.label || name;
	const typeOf = (name: string) => defOf(name)?.type ?? core.get(name)?.type ?? 'text';
	const isWide = (name: string) => typeOf(name) === 'textarea' || name === 'title';

	/** Problems the server reports about a field, by its name in "fields: name: ...". */
	function problemsOf(name: string) {
		return problems.filter((p) => p.startsWith(`fields: ${name}:`) || p.includes(` ${name} `) || p.endsWith(` ${name}`));
	}

	/** What the list says about a field's conditions, in words. */
	function tags(name: string) {
		const f = defOf(name);
		const out: string[] = [];
		if (!f) return out;
		const sf = formSimpleFields(form, name, statuses, removed);
		if (f.visible_when) out.push('shown when ' + describe(f.visible_when, sf));
		if (f.required_when) out.push('required when ' + describe(f.required_when, sf));
		if (f.read_only_when) out.push('read-only when ' + describe(f.read_only_when, sf));
		return out;
	}

	/** Fields that could be added to this layout: those not on it yet, the record's own where they are allowed. */
	const addable = $derived.by(() => {
		const on = new Set(form[layout].flatMap((s) => s.fields));
		const out: { name: string; label: string }[] = [];
		for (const f of form.fields) if (!coreNames.has(f.name) && !on.has(f.name) && !removed.has(f.name)) out.push({ name: f.name, label: f.label || f.name });
		for (const c of def.core_fields) if (!on.has(c.name) && (layout === 'create_sections' ? c.on_create : c.detail)) out.push({ name: c.name, label: labelOf(c.name) });
		return out;
	});

	function add(si: number, choice: string) {
		const s = form[layout][si];
		let name = choice;
		if (choice === '+new') {
			name = freshName('field', [...form.fields.map((f) => f.name), ...def.core_fields.map((c) => c.name)]);
			form.fields.push({ name, type: 'text', label: '' });
		}
		putIn(form[layout], name, si, 0, (s.columns?.[0] ?? s.fields).length);
		open = null;
		openField(name);
	}

	/** Opens a field's settings (a record's own field gets an entry to hold its label and conditions). */
	function openField(name: string) {
		if (open === name) {
			open = null;
			return;
		}
		if (!form.fields.some((f) => f.name === name)) form.fields.push({ name });
		open = name;
	}

	/** A new field's name followed its label: it keeps its places in both layouts. */
	function renamed(from: string, to: string) {
		for (const key of ['create_sections', 'detail'] as const) {
			for (const s of form[key]) {
				for (const l of s.columns ?? [s.fields]) {
					const i = l.indexOf(from);
					if (i >= 0) l[i] = to;
				}
				if (s.wide) s.wide = s.wide.map((n) => (n === from ? to : n));
				sync(s);
			}
		}
		if (open === from) open = to;
	}

	function removeField(name: string) {
		removed = new Set([...removed, name]);
	}
	function putBack(name: string) {
		const next = new Set(removed);
		next.delete(name);
		removed = next;
	}

	function toggleWide(s: FormSection, name: string) {
		s.wide = s.wide?.includes(name) ? s.wide.filter((n) => n !== name) : [...(s.wide ?? []), name];
	}

	function addSection() {
		form[layout].push({ label: '', fields: [] });
	}
	function removeSection(si: number) {
		const list = form[layout];
		const gone = list[si];
		const into = list[si - 1] ?? list[si + 1];
		if (into) {
			for (const n of gone.fields) (into.columns?.[0] ?? into.fields).push(n);
			sync(into);
		}
		list.splice(si, 1);
	}
	function moveSection(si: number, by: number) {
		const list = form[layout];
		const [s] = list.splice(si, 1);
		list.splice(si + by, 0, s);
	}

	// Drag and drop: a row onto a row (before it) or onto a list's empty end.
	function drop(e: DragEvent, si: number, ci: number, index: number) {
		e.preventDefault();
		const name = dragging ?? e.dataTransfer?.getData('text/plain');
		dragging = dropAt = null;
		if (name) putIn(form[layout], name, si, ci, index);
	}

	// Asking the server as the form changes: what saving would refuse, a moment after the last change.
	let timer: ReturnType<typeof setTimeout> | undefined;
	let asked = 0;
	$effect(() => {
		const body = forSave(form, removed, coreNames);
		if (!dirty) {
			problems = [];
			return;
		}
		clearTimeout(timer);
		timer = setTimeout(async () => {
			const n = ++asked;
			try {
				const r = await api<{ problems: string[] }>(`/api/v1/forms/${def.record_type}/definition/check`, { method: 'POST', body: { form: body } });
				if (n === asked) problems = r.problems;
			} catch {
				/* the save says it all */
			}
		}, 700);
		return () => clearTimeout(timer);
	});

	async function save() {
		busy = true;
		error = '';
		try {
			const out = await api<FormDefResponse>(`/api/v1/forms/${def.record_type}/definition`, { method: 'PUT', body: { form: forSave(form, removed, coreNames) } });
			ui.announce = `${def.label} form saved`;
			onsaved(out);
		} catch (err) {
			error = err instanceof Error ? err.message : 'The form was not saved';
			if (err instanceof ApiError) problems = ((err.body as { problems?: string[] })?.problems ?? []).slice(0, 30);
		} finally {
			busy = false;
		}
	}

	async function reset() {
		busy = true;
		error = '';
		try {
			const out = await api<FormDefResponse>(`/api/v1/forms/${def.record_type}/edits`, { method: 'DELETE' });
			ui.announce = 'Changes undone';
			onsaved(out);
		} catch (err) {
			error = err instanceof Error ? err.message : 'That did not work';
		} finally {
			busy = false;
			confirmingReset = false;
		}
	}

	// The preview: the form as people will see it, with the simple conditions worked out from what is filled in.
	function asFormField(name: string): FormField {
		const f = defOf(name);
		const c = core.get(name);
		return {
			name,
			type: (f?.type ?? c?.type ?? 'text') as FormField['type'],
			label: labelOf(name),
			help: f?.help,
			core: !!c,
			options: (f?.options ?? c?.options)?.filter((o) => o.value).map((o) => ({ value: o.value, label: o.label || o.value })),
			max_length: f?.max_length,
			min: f?.min,
			max: f?.max
		};
	}
	const previewValues = $derived({ status: statuses[0]?.id, priority: 3, ...preview });
	function previewState(name: string) {
		const f = defOf(name);
		const sf = formSimpleFields(form, name, statuses, removed);
		const test = (e?: string) => {
			if (!e) return undefined;
			const c = parseSimple(e, sf);
			return c ? holds(c, previewValues) : undefined; // an expression: the server works it out
		};
		const options = f?.options?.some((o) => o.when) ? f.options.filter((o) => !o.when || test(o.when) !== false).map((o) => o.value) : undefined;
		return {
			hidden: test(f?.visible_when) === false,
			required: !!f?.required || test(f?.required_when) === true,
			read_only: test(f?.read_only_when) === true,
			options
		};
	}
	const previewSections = $derived(form[layout].map((s) => ({ ...s, fields: s.fields.filter((n) => !removed.has(n)), columns: s.columns?.map((c) => c.filter((n) => !removed.has(n))) })));
</script>

{#snippet row(s: FormSection, si: number, ci: number, name: string, index: number)}
	{@const gone = removed.has(name)}
	{@const isCore = coreNames.has(name)}
	<li
		class="frow"
		class:sel={open === name}
		class:gone
		class:drop={dropAt === name}
		draggable="true"
		ondragstart={(e) => {
			dragging = name;
			e.dataTransfer?.setData('text/plain', name);
		}}
		ondragend={() => (dragging = dropAt = null)}
		ondragover={(e) => {
			e.preventDefault();
			dropAt = name;
		}}
		ondragleave={() => dropAt === name && (dropAt = null)}
		ondrop={(e) => drop(e, si, ci, index)}
	>
		<span class="grip" aria-hidden="true">⋮⋮</span>
		<button type="button" class="name" aria-expanded={open === name} onclick={() => openField(name)}>
			{labelOf(name)}
		</button>
		<span class="tag type">{isCore ? 'built-in' : (questionTypes.find((t) => t.type === typeOf(name))?.label ?? typeOf(name))}</span>
		<span class="space"></span>
		{#if gone}
			<span class="tag">removed</span>
		{:else}
			{#each tags(name) as t (t)}<span class="tag cond">{t}</span>{/each}
			{#if defOf(name)?.required}<span class="tag">required</span>{/if}
			{#if !s.columns?.length && !isWide(name)}
				<button type="button" class="tag toggle" aria-pressed={!!s.wide?.includes(name)} onclick={() => toggleWide(s, name)} title="Take a whole row">wide</button>
			{/if}
		{/if}
		{#if problemsOf(name).length}<span class="tag bad">problem</span>{/if}
		<span class="moves">
			<button type="button" class="btn quiet" disabled={!canMove(form[layout], name, -1)} onclick={() => moveBy(form[layout], name, -1)} aria-label="Move {labelOf(name)} up">↑</button>
			<button type="button" class="btn quiet" disabled={!canMove(form[layout], name, 1)} onclick={() => moveBy(form[layout], name, 1)} aria-label="Move {labelOf(name)} down">↓</button>
			{#if name !== 'title'}
				<button type="button" class="btn quiet" onclick={() => takeOut(form[layout], name)} aria-label="Take {labelOf(name)} off this layout" title="Take off this layout">×</button>
			{/if}
		</span>
	</li>
{/snippet}

{#snippet settings(name: string)}
	{@const fi = form.fields.findIndex((f) => f.name === name)}
	{@const c = core.get(name)}
	<div class="expand" role="region" aria-label="Settings for {labelOf(name)}">
		{#if removed.has(name)}
			<div class="note">
				<p>
					<strong>Taken off the form.</strong>
					{#if def.values[name]}
						{def.values[name]} record{def.values[name] === 1 ? ' has' : 's have'} a value for {labelOf(name)}; they keep it, and it stays in their
						history. It is no longer shown, asked for or checked. Adding a field named {name} again brings the values back.
					{:else}
						It goes when you save. No record has a value for it.
					{/if}
				</p>
				<button type="button" class="btn" onclick={() => putBack(name)}>Put it back</button>
			</div>
		{:else if fi >= 0}
			<FieldSettings
				bind:field={form.fields[fi]}
				core={c ? { label: c.label, type: c.type } : undefined}
				saved={saved.has(name)}
				values={def.values[name] ?? 0}
				others={form.fields.filter((f) => f.name !== name).map((f) => f.name)}
				fields={formSimpleFields(form, name, statuses, removed)}
				history
				problems={problemsOf(name)}
				onrename={renamed}
			/>
			{#if !c}
				<button type="button" class="danger" onclick={() => removeField(name)}>Remove {labelOf(name) || 'this field'} from the form</button>
			{/if}
		{/if}
	</div>
{/snippet}

<div class="segmented phone-switch" role="group" aria-label="Show">
	<button type="button" aria-pressed={phoneView === 'fields'} onclick={() => (phoneView = 'fields')}>Fields</button>
	<button type="button" aria-pressed={phoneView === 'preview'} onclick={() => (phoneView = 'preview')}>Preview</button>
</div>
<div class="editor" data-show={phoneView}>
	<div class="build">
		<div class="bar">
			<div class="segmented" role="group" aria-label="Layout">
				<button type="button" aria-pressed={tab === 'detail'} onclick={() => (tab = 'detail')}>Record page</button>
				<button type="button" aria-pressed={tab === 'create_sections'} onclick={() => (tab = 'create_sections')}>New ticket dialog</button>
				<button type="button" aria-pressed={tab === 'checks'} onclick={() => (tab = 'checks')}>Checks ({form.validate.length})</button>
			</div>
			<span class="space"></span>
			{#if dirty}<span class="pill">Unsaved changes</span>{/if}
			{#if def.edited}
				{#if confirmingReset}
					<span class="confirm" role="group" aria-label="Undo the changes made here?">
						<button type="button" class="btn danger-btn" disabled={busy} onclick={reset}>{def.shipped ? 'Undo changes' : 'Back to the built-in form'}</button>
						<button type="button" class="btn" onclick={() => (confirmingReset = false)}>Keep them</button>
					</span>
				{:else}
					<button type="button" class="btn" onclick={() => (confirmingReset = true)}>Undo the changes made here</button>
				{/if}
			{/if}
			<button type="button" class="btn primary" disabled={busy || !dirty} onclick={save}>{busy ? 'Saving…' : 'Save'}</button>
		</div>

		{#if error || (dirty && problems.length)}
			<div class="error" role="alert">
				<p>{error || 'Saving this would be refused:'}</p>
				{#if problems.length}<ul>{#each problems as p, i (i)}<li>{p}</li>{/each}</ul>{/if}
			</div>
		{/if}

		{#if tab === 'checks'}
			<section class="sec checks" aria-label="Checks">
				<p class="help">Rules the whole record must meet, each with what people are told when it does not.</p>
				{#each form.validate as v, i (i)}
					<div class="check">
						<ConditionInput
							legend="Check {i + 1}"
							bind:expression={v.when}
							empty="always"
							fields={formSimpleFields(form, '', statuses, removed)}
							placeholder={'!has(record.fields.due_date) || record.fields.due_date >= now'}
						/>
						<label class="field">
							<span>When it does not hold, people are told</span>
							<input bind:value={v.error} placeholder="The due date cannot be in the past" />
						</label>
						<button type="button" class="danger" onclick={() => form.validate.splice(i, 1)}>Remove check {i + 1}</button>
					</div>
				{/each}
				<button type="button" class="btn start" onclick={() => form.validate.push({ when: '', error: '' })}>Add a check</button>
			</section>
		{:else}
			{#each sections as s, si (si)}
				{@const opened = open && locate(sections, open)?.section === si ? open : null}
				<section class="sec" aria-label="Section {si + 1}{s.label ? `: ${s.label}` : ''}">
					<header>
						<input class="heading" aria-label="Heading of section {si + 1}" bind:value={s.label} placeholder="No heading" maxlength="100" />
						<div class="segmented cols-switch" role="group" aria-label="Columns of section {si + 1}">
							<button type="button" aria-pressed={!s.columns?.length} onclick={() => setColumns(s, 0)}>Automatic</button>
							<button type="button" aria-pressed={s.columns?.length === 2} onclick={() => setColumns(s, 2)}>2</button>
							<button type="button" aria-pressed={s.columns?.length === 3} onclick={() => setColumns(s, 3)}>3</button>
						</div>
						<button type="button" class="btn quiet" disabled={si === 0} onclick={() => moveSection(si, -1)} aria-label="Move section {si + 1} up">↑</button>
						<button type="button" class="btn quiet" disabled={si === sections.length - 1} onclick={() => moveSection(si, 1)} aria-label="Move section {si + 1} down">↓</button>
						{#if sections.length > 1}
							<button type="button" class="btn quiet" onclick={() => removeSection(si)} aria-label="Remove section {si + 1}" title="Its fields go to the section above">×</button>
						{/if}
					</header>
					{#if s.columns?.length}
						<div class="cols" style:--n={s.columns.length}>
							{#each s.columns as col, ci (ci)}
								<ol
									class="col"
									aria-label="Column {ci + 1}"
									ondragover={(e) => e.preventDefault()}
									ondrop={(e) => e.target === e.currentTarget && drop(e, si, ci, col.length)}
								>
									<li class="colhead" aria-hidden="true">Column {ci + 1}</li>
									{#each col as name, i (name)}{@render row(s, si, ci, name, i)}{/each}
								</ol>
							{/each}
						</div>
					{:else}
						<ol class="flow" ondragover={(e) => e.preventDefault()} ondrop={(e) => e.target === e.currentTarget && drop(e, si, 0, s.fields.length)}>
							{#each s.fields as name, i (name)}{@render row(s, si, 0, name, i)}{/each}
							{#if s.fields.length === 0}<li class="empty">Drag fields here, or add one.</li>{/if}
						</ol>
						<p class="help small">Automatic: as many columns as fit, up to three. Long text and wide fields take the whole row.</p>
					{/if}
					{#if opened}{@render settings(opened)}{/if}
					<select
						class="select add"
						aria-label="Add a field to section {si + 1}"
						value=""
						onchange={(e) => {
							const v = e.currentTarget.value;
							e.currentTarget.value = '';
							if (v) add(si, v);
						}}
					>
						<option value="">+ Add a field</option>
						<option value="+new">A new field</option>
						{#if addable.length}
							<optgroup label="Not on this layout yet">
								{#each addable as a (a.name)}<option value={a.name}>{a.label}</option>{/each}
							</optgroup>
						{/if}
					</select>
				</section>
			{/each}
			<button type="button" class="btn start" onclick={addSection}>Add a section</button>
		{/if}

		<p class="help">
			{#if def.builtin && !def.edited}
				{def.label} records have the built-in form. Saving keeps your form in {def.app}, the app that owns them.
			{:else}
				From {def.app}{def.edited ? `, changed here${def.edited_by ? ` by ${def.edited_by}` : ''}${def.edited_at ? ` on ${when(def.edited_at)}` : ''}` : ''}.
				{def.shipped ? 'Upgrades of the app keep these changes.' : ''}
			{/if}
		</p>
		<details>
			<summary>YAML</summary>
			{#if def.override}<p class="help">The change, as kept (what export-local writes):</p><pre>{def.override}</pre>{/if}
			<p class="help">The form:</p>
			<pre>{def.yaml}</pre>
		</details>
	</div>

	<aside class="preview" aria-label="Preview">
		<div class="pbar">
			<span class="help">Preview · {layout === 'detail' ? 'Record page' : 'New ticket dialog'}</span>
			{#if layout === 'detail' && statuses.length}
				<label class="as-if">
					as if
					<select class="select" bind:value={() => String(previewValues.status ?? ''), (v) => (preview.status = v)} aria-label="Preview as if the status is">
						{#each statuses as st (st.id)}<option value={st.id}>{st.label}</option>{/each}
					</select>
				</label>
			{/if}
		</div>
		<div class="sheet">
			{#if !previewSections.some((s) => s.fields.length)}<p class="help">Nothing on this layout yet.</p>{/if}
			{#each previewSections as s, si (si)}
				{#if s.fields.length}
					{#if s.label}<h3>{s.label}</h3>{/if}
					<FormGrid section={s} min="11rem" show={(n) => !previewState(n).hidden} isWide={(n) => isWide(n) || wideField(asFormField(n))}>
						{#snippet item(name)}
							{@const st = previewState(name)}
							<div class="field" class:hl={open === name}>
								<label for="pv-{name}">{labelOf(name)}{#if st.required}<span class="req" aria-hidden="true"> *</span>{/if}</label>
								<FieldInput field={asFormField(name)} value={preview[name] ?? null} fieldState={st} id="pv-{name}" disabled={st.read_only} live onchange={(v) => (preview[name] = v)} />
							</div>
						{/snippet}
					</FormGrid>
				{/if}
			{/each}
		</div>
		<p class="help small">Simple conditions are worked out here as you fill the preview in; expressions are worked out by the server.</p>
	</aside>
</div>

<style>
	.editor {
		display: grid;
		grid-template-columns: minmax(0, 1fr) minmax(20rem, 28rem);
		gap: 1.25rem;
		align-items: start;
	}
	.build {
		display: grid;
		/* One column as wide as the room: never as wide as the YAML's longest line. */
		grid-template-columns: minmax(0, 1fr);
		gap: 0.9rem;
		min-width: 0;
	}
	.bar {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		flex-wrap: wrap;
	}
	.space {
		flex: 1;
	}
	.pill {
		font-size: 0.75rem;
		padding: 0.15rem 0.55rem;
		border-radius: 99px;
		background: var(--marker-soft);
		color: var(--marker-ink);
		font-weight: 600;
	}
	.sec {
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--ground);
		display: grid;
		gap: 0;
	}
	.sec > header {
		display: flex;
		align-items: center;
		flex-wrap: wrap;
		gap: 0.4rem;
		padding: 0.5rem 0.6rem;
		border-bottom: 1px solid var(--rule);
	}
	.heading {
		flex: 1;
		min-width: 0;
		border: 1px solid transparent;
		border-radius: 6px;
		background: transparent;
		font: 600 0.9375rem var(--font-display);
		padding: 0.25rem 0.4rem;
		color: var(--ink);
	}
	.heading:hover,
	.heading:focus-visible {
		border-color: var(--rule);
		background: var(--raised);
	}
	.cols-switch {
		font-size: 0.8125rem;
	}
	ol {
		list-style: none;
		margin: 0;
		padding: 0;
	}
	.flow {
		min-height: 2.5rem;
	}
	.cols {
		display: grid;
		grid-template-columns: repeat(var(--n), minmax(0, 1fr));
		gap: 0.5rem;
		padding: 0.5rem;
	}
	.col {
		border: 1px dashed var(--rule);
		border-radius: var(--radius);
		background: var(--surface);
		min-height: 4rem;
		padding-bottom: 0.4rem;
	}
	.colhead {
		font-size: 0.75rem;
		color: var(--ink-2);
		padding: 0.3rem 0.6rem;
		border-bottom: 1px dashed var(--rule);
	}
	.frow {
		display: flex;
		align-items: center;
		gap: 0.4rem;
		flex-wrap: wrap;
		padding: 0.4rem 0.6rem;
		border-bottom: 1px solid var(--rule);
		background: var(--ground);
		cursor: grab;
	}
	.col .frow {
		margin: 0.4rem 0.4rem 0;
		border: 1px solid var(--rule);
		border-radius: 7px;
	}
	.flow .frow:last-child {
		border-bottom: 0;
	}
	.frow.sel {
		background: var(--blue-soft);
		box-shadow: inset 3px 0 0 var(--blue);
	}
	.frow.drop {
		box-shadow: inset 0 2px 0 var(--blue);
	}
	.frow.gone .name {
		text-decoration: line-through;
		color: var(--ink-2);
	}
	.grip {
		color: var(--ink-2);
		letter-spacing: -2px;
		font-size: 0.75rem;
	}
	.name {
		background: none;
		border: 0;
		padding: 0;
		font: inherit;
		color: var(--ink);
		cursor: pointer;
		text-align: left;
	}
	.name:hover {
		text-decoration: underline;
	}
	.tag {
		font-size: 0.75rem;
		color: var(--ink-2);
		white-space: nowrap;
	}
	.tag.type {
		font-family: var(--font-mono);
		background: var(--hover);
		padding: 0 0.35rem;
		border-radius: 5px;
	}
	.tag.cond {
		color: var(--marker-ink);
		background: var(--marker-soft);
		padding: 0.05rem 0.4rem;
		border-radius: 5px;
		white-space: normal;
	}
	.tag.bad {
		color: var(--red);
		font-weight: 600;
	}
	.tag.toggle {
		border: 1px solid var(--rule);
		border-radius: 5px;
		background: none;
		padding: 0 0.35rem;
		cursor: pointer;
	}
	.tag.toggle[aria-pressed='true'] {
		color: var(--ink);
		background: var(--hover);
		font-weight: 600;
	}
	.moves {
		display: inline-flex;
		gap: 0.1rem;
	}
	/* A row's own controls come up when it is pointed at, chosen or reached by keyboard. */
	.frow .moves,
	.frow .toggle[aria-pressed='false'] {
		opacity: 0;
		transition: opacity 0.12s;
	}
	.frow:hover .moves,
	.frow:focus-within .moves,
	.frow.sel .moves,
	.frow:hover .toggle,
	.frow:focus-within .toggle,
	.frow.sel .toggle {
		opacity: 1;
	}
	@media (hover: none) {
		.frow .moves,
		.frow .toggle[aria-pressed='false'] {
			opacity: 1;
		}
	}
	.start {
		justify-self: start;
	}
	.moves .btn {
		min-height: 1.75rem;
		padding: 0 0.4rem;
	}
	.empty {
		padding: 0.75rem;
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.expand {
		margin: 0.5rem;
		padding: 0.75rem;
		border: 1px solid var(--blue);
		border-radius: var(--radius);
		background: var(--surface);
		display: grid;
		gap: 0.6rem;
	}
	.note {
		display: grid;
		gap: 0.5rem;
		justify-items: start;
		padding: 0.6rem 0.75rem;
		border-left: 3px solid var(--marker);
		background: var(--marker-soft);
		border-radius: 0 6px 6px 0;
		font-size: 0.875rem;
	}
	.add {
		width: auto;
		margin: 0.5rem 0.6rem 0.6rem;
		justify-self: start;
		color: var(--blue);
		font-weight: 600;
	}
	.danger {
		justify-self: start;
		background: none;
		border: 0;
		padding: 0;
		color: var(--red);
		font-weight: 600;
		cursor: pointer;
	}
	.danger-btn {
		color: var(--red);
	}
	.checks {
		padding: 0.75rem;
		gap: 0.75rem;
	}
	.check {
		display: grid;
		gap: 0.5rem;
		padding-bottom: 0.75rem;
		border-bottom: 1px solid var(--rule);
	}
	.confirm {
		display: inline-flex;
		gap: 0.4rem;
	}
	.help {
		color: var(--ink-2);
		font-size: 0.875rem;
	}
	.small {
		font-size: 0.8125rem;
		padding: 0.3rem 0.6rem 0;
	}
	.error {
		padding: 0.6rem 0.75rem;
		border-left: 3px solid var(--red);
		background: var(--raised);
		font-size: 0.875rem;
	}
	.error p {
		font-weight: 600;
		color: var(--red);
	}
	.error ul {
		margin: 0.3rem 0 0;
		padding-left: 1.2rem;
	}
	pre {
		font: 0.75rem/1.45 var(--font-mono);
		background: var(--surface);
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		padding: 0.6rem;
		overflow-x: auto;
	}
	.preview {
		position: sticky;
		top: 0;
		display: grid;
		grid-template-columns: minmax(0, 1fr);
		gap: 0.6rem;
	}
	.pbar {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		flex-wrap: wrap;
	}
	.as-if {
		display: inline-flex;
		align-items: center;
		gap: 0.35rem;
		font-size: 0.8125rem;
		color: var(--ink-2);
	}
	.as-if .select {
		width: auto;
		min-height: 1.9rem;
		padding: 0.15rem 0.45rem;
	}
	.sheet {
		background: var(--ground);
		border: 1px solid var(--rule);
		border-radius: 10px;
		box-shadow: var(--shadow-pop);
		padding: 1rem;
		display: grid;
		gap: 0.9rem;
	}
	.sheet h3 {
		font: 650 1rem var(--font-display);
	}
	.hl {
		outline: 2px solid var(--blue);
		outline-offset: 4px;
		border-radius: 6px;
	}
	.req {
		color: var(--red);
	}
	@media (max-width: 64rem) {
		.editor {
			grid-template-columns: minmax(0, 1fr);
		}
		.preview {
			position: static;
		}
	}
	.phone-switch {
		display: none;
	}
	@media (max-width: 40rem) {
		.cols {
			grid-template-columns: minmax(0, 1fr);
		}
		.phone-switch {
			display: flex;
			margin-bottom: 0.75rem;
		}
		.heading {
			flex-basis: 100%;
		}
		.phone-switch button {
			flex: 1;
		}
		.editor[data-show='fields'] .preview,
		.editor[data-show='preview'] .build {
			display: none;
		}
	}
</style>
