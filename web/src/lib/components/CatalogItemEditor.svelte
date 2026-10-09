<script lang="ts">
	import { onMount, untrack } from 'svelte';
	import { api, ApiError } from '$lib/api';
	import CatalogIcon from '$lib/components/CatalogIcon.svelte';
	import FieldInput from '$lib/components/FieldInput.svelte';
	import {
		isDescription,
		itemNameFrom,
		layoutFor,
		nameFrom,
		parseValue,
		previewField,
		questionTypes,
		valueText,
		type DefField,
		type ItemDef,
		type ItemDefResponse
	} from '$lib/catalogEditor';
	import { wide } from '$lib/fields';
	import { when } from '$lib/format';
	import type { FieldValue, Group } from '$lib/types';
	import { ui } from '$lib/ui.svelte';

	/**
	 * Edits one catalog item: what it is, who may order it, its questions and how it is fulfilled, with the order form
	 * previewed as people will see it. Saving keeps only what changed, in the app's studio layer (like the workflow
	 * builder); the server checks it as an install would and lists every problem.
	 */
	let {
		def,
		apps,
		categories,
		onsaved,
		onreset
	}: {
		/** The item as it is; null for a new one. */
		def: ItemDefResponse | null;
		/** Apps a new item can go in, with their request workflows. */
		apps: { name: string; workflows: string[] }[];
		categories: string[];
		onsaved: (saved: ItemDefResponse) => void;
		onreset: (after: ItemDefResponse | null) => void;
	} = $props();

	const icons = ['laptop', 'phone', 'monitor', 'printer', 'software', 'key', 'person', 'network', 'server', 'building', 'box', 'question'];

	const blank = (): ItemDef => ({ name: '', label: '', category: '', form: { fields: [], create: [], detail: [] } });
	// Working copies of what the editor started from: the page re-keys it to start again after a save or a reset.
	let item = $state<ItemDef>(untrack(() => structuredClone($state.snapshot(def?.item) ?? blank())));
	let app = $state(untrack(() => def?.app ?? apps.find((a) => a.name === 'service-catalog')?.name ?? apps[0]?.name ?? ''));
	let nameTouched = $state(untrack(() => !!def));
	let open = $state<number | null>(null);
	let busy = $state(false);
	let error = $state('');
	let problems = $state<string[]>([]);
	let confirmingReset = $state(false);
	let preview = $state<Record<string, FieldValue | null>>({});
	let allGroups = $state<Group[]>([]);

	const isNew = $derived(!def);
	const workflows = $derived(def?.workflows ?? apps.find((a) => a.name === app)?.workflows ?? []);
	const limited = $derived(!!item.available_to);
	const asksDescription = $derived(item.form.fields.some(isDescription));
	const titleText = $derived(valueText(item.title));

	onMount(() => {
		api<{ items: Group[] }>('/api/v1/groups')
			.then((r) => (allGroups = r.items))
			.catch(() => {});
	});

	function setLabel(v: string) {
		item.label = v;
		if (!nameTouched) item.name = itemNameFrom(v);
	}

	function addQuestion() {
		const taken = item.form.fields.map((f) => f.name);
		const q: DefField = { name: nameFrom('question', taken), type: 'text', label: '' };
		// Before the description, which reads best last.
		const at = item.form.fields.findIndex(isDescription);
		if (at >= 0) item.form.fields.splice(at, 0, q);
		else item.form.fields.push(q);
		open = at >= 0 ? at : item.form.fields.length - 1;
	}

	function toggleDescription(on: boolean) {
		if (on) item.form.fields.push({ name: 'description', label: 'Anything else we should know?' });
		else item.form.fields = item.form.fields.filter((f) => !isDescription(f));
	}

	function move(i: number, by: number) {
		const j = i + by;
		if (j < 0 || j >= item.form.fields.length) return;
		const [f] = item.form.fields.splice(i, 1);
		item.form.fields.splice(j, 0, f);
		if (open === i) open = j;
	}

	function remove(i: number) {
		item.form.fields.splice(i, 1);
		open = null;
	}

	function setLabelOf(f: DefField, v: string) {
		const auto = !f.label || f.name === nameFrom(f.label, item.form.fields.filter((x) => x !== f).map((x) => x.name));
		f.label = v;
		// A question's name follows its label until it is saved as part of the item (answers are stored by name).
		const saved = def?.item.form.fields.some((x) => x.name === f.name);
		if (auto && !saved && !isDescription(f) && v.trim()) {
			f.name = nameFrom(v, item.form.fields.filter((x) => x !== f).map((x) => x.name));
		}
	}

	function setType(f: DefField, t: DefField['type']) {
		f.type = t;
		if (t === 'choice' && !f.options?.length) f.options = [{ value: '', label: '' }];
		if (t !== 'choice') delete f.options;
		if (t !== 'text' && t !== 'textarea') delete f.max_length;
		if (t !== 'number') {
			delete f.min;
			delete f.max;
		}
		delete f.default;
	}

	function setAudience(on: boolean) {
		item.available_to = on ? { roles: [], groups: [] } : undefined;
	}

	function toggleGroup(name: string, on: boolean) {
		const a = (item.available_to ??= { roles: [], groups: [] });
		a.groups = on ? [...(a.groups ?? []), name] : (a.groups ?? []).filter((g) => g !== name);
	}

	/** The item as it is saved: questions tidied, layouts worked out from their order. */
	function forSave(): ItemDef {
		const out = $state.snapshot(item) as ItemDef;
		out.form.fields = out.form.fields.map((f) => {
			const c = { ...f };
			for (const k of Object.keys(c) as (keyof DefField)[]) {
				const v = c[k];
				if (v === '' || v === undefined || v === null || (v === false && k !== 'history')) delete c[k];
			}
			if (c.options) c.options = c.options.filter((o) => o.value.trim()).map((o) => ({ ...o, label: o.label || o.value, ...(o.when ? {} : { when: undefined }) }));
			return c;
		});
		Object.assign(out.form, layoutFor(out.form));
		out.form.validate = (out.form.validate ?? []).filter((v) => v.when.trim() || v.error.trim());
		if (out.available_to) {
			const roles = (out.available_to.roles ?? []).map((r) => r.trim()).filter(Boolean);
			const groups = out.available_to.groups ?? [];
			// Left empty, the server says so rather than this quietly meaning everyone.
			out.available_to = { ...(roles.length ? { roles } : {}), ...(groups.length ? { groups } : {}) };
		}
		for (const k of ['summary', 'description', 'icon', 'workflow'] as const) if (!out[k]) delete out[k];
		if (!out.order) delete out.order;
		if (!out.title || (!out.title.expr && !out.title.literal)) delete out.title;
		return out;
	}

	async function save(e: Event) {
		e.preventDefault();
		busy = true;
		error = '';
		problems = [];
		try {
			const body = forSave();
			const saved = await api<ItemDefResponse>(`/api/v1/catalog-items/${encodeURIComponent(body.name)}`, {
				method: 'PUT',
				body: { app: isNew ? app : undefined, item: body }
			});
			ui.announce = `${saved.item.label} saved`;
			onsaved(saved);
		} catch (err) {
			error = err instanceof Error ? err.message : 'The item was not saved';
			if (err instanceof ApiError) problems = ((err.body as { problems?: string[] })?.problems ?? []).slice(0, 30);
		} finally {
			busy = false;
		}
	}

	async function reset() {
		if (!def) return;
		busy = true;
		error = '';
		try {
			const after = await api<ItemDefResponse | undefined>(`/api/v1/catalog-items/${def.item.name}/edits`, { method: 'DELETE' });
			ui.announce = after ? 'Changes undone' : `${def.item.label} removed`;
			onreset(after ?? null);
		} catch (err) {
			error = err instanceof Error ? err.message : 'That did not work';
		} finally {
			busy = false;
			confirmingReset = false;
		}
	}
</script>

<div class="editor">
	<form class="settings" onsubmit={save} aria-label={isNew ? 'New catalog item' : `Edit ${def?.item.label}`}>
		<section aria-labelledby="item-h">
			<h2 id="item-h">The item</h2>
			<div class="grid">
				<label class="field">
					<span>Label</span>
					<input value={item.label} oninput={(e) => setLabel(e.currentTarget.value)} required maxlength="100" placeholder="New laptop" />
				</label>
				<label class="field">
					<span>Category</span>
					<input bind:value={item.category} required maxlength="60" list="catalog-categories" placeholder="Hardware" />
					<datalist id="catalog-categories">{#each categories as c (c)}<option value={c}></option>{/each}</datalist>
				</label>
				{#if isNew}
					<label class="field">
						<span>Name</span>
						<input
							value={item.name}
							oninput={(e) => {
								item.name = e.currentTarget.value;
								nameTouched = true;
							}}
							required
							pattern="[a-z][a-z0-9-]{'{'}1,62}"
							placeholder="new-laptop"
						/>
						<small class="help">How the item is named in links and files: lowercase letters, digits and hyphens. It cannot change later.</small>
					</label>
					<label class="field">
						<span>App</span>
						<select bind:value={app}>
							{#each apps as a (a.name)}<option value={a.name}>{a.name}</option>{/each}
						</select>
						<small class="help">The app it is kept in, with its workflows.</small>
					</label>
				{/if}
				<label class="field wide">
					<span>Summary</span>
					<input bind:value={item.summary} maxlength="200" placeholder="One line under the label in the catalog" />
				</label>
				<label class="field wide">
					<span>Description</span>
					<textarea bind:value={item.description} rows="4" placeholder="What it is, and what happens after ordering it. Markdown."></textarea>
				</label>
				<fieldset class="wide icons">
					<legend>Picture</legend>
					{#each icons as name (name)}
						<label class="icon" title={name}>
							<input type="radio" name="icon" value={name} checked={item.icon === name} onchange={() => (item.icon = name)} />
							<span><CatalogIcon {name} /></span>
							<span class="sr-only">{name}</span>
						</label>
					{/each}
				</fieldset>
				<label class="field">
					<span>Request title</span>
					<input value={titleText} oninput={(e) => (item.title = parseValue(e.currentTarget.value))} placeholder={item.label || 'The label'} />
					<small class="help">What its requests are called: text, or =an expression such as ="New laptop: " + record.fields.model</small>
				</label>
				<label class="field">
					<span>Position</span>
					<input type="number" value={item.order ?? ''} oninput={(e) => (item.order = Number(e.currentTarget.value) || undefined)} placeholder="Alphabetical" />
					<small class="help">Lower comes first within its category.</small>
				</label>
			</div>
		</section>

		<section aria-labelledby="who-h">
			<h2 id="who-h">Who may order it</h2>
			<label class="check"><input type="radio" name="who" checked={!limited} onchange={() => setAudience(false)} /> Everyone</label>
			<label class="check"><input type="radio" name="who" checked={limited} onchange={() => setAudience(true)} /> Only people in these groups or with these roles</label>
			{#if item.available_to}
				<div class="audience">
					<fieldset>
						<legend>Groups</legend>
						{#if allGroups.length === 0}<p class="help">There are no groups yet.</p>{/if}
						{#each allGroups as g (g.id)}
							<label class="check">
								<input type="checkbox" checked={item.available_to.groups?.includes(g.name)} onchange={(e) => toggleGroup(g.name, e.currentTarget.checked)} />
								{g.name}
							</label>
						{/each}
					</fieldset>
					<label class="field">
						<span>Roles</span>
						<input
							value={(item.available_to.roles ?? []).join(', ')}
							onchange={(e) => (item.available_to!.roles = e.currentTarget.value.split(',').map((r) => r.trim()).filter(Boolean))}
							placeholder="agent, admin"
						/>
						<small class="help">Role names, separated by commas: base roles (admin, agent, end_user) or app roles.</small>
					</label>
				</div>
			{/if}
		</section>

		<section aria-labelledby="questions-h">
			<h2 id="questions-h">Questions</h2>
			{#if item.form.fields.length === 0}<p class="help">No questions: ordering it is one click.</p>{/if}
			<ol class="questions">
				{#each item.form.fields as f, i (i)}
					<li class:open={open === i}>
						<div class="qhead">
							<button type="button" class="qtitle" aria-expanded={open === i} onclick={() => (open = open === i ? null : i)}>
								<strong>{f.label || (isDescription(f) ? 'Anything else' : 'New question')}</strong>
								<span class="help">{isDescription(f) ? 'their own words' : (questionTypes.find((t) => t.type === f.type)?.label ?? f.type)}{f.required ? ' · required' : ''}</span>
							</button>
							<button type="button" class="btn quiet" disabled={i === 0} onclick={() => move(i, -1)} aria-label="Move {f.label || f.name} up">↑</button>
							<button type="button" class="btn quiet" disabled={i === item.form.fields.length - 1} onclick={() => move(i, 1)} aria-label="Move {f.label || f.name} down">↓</button>
							<button type="button" class="btn quiet reject" onclick={() => remove(i)} aria-label="Remove {f.label || f.name}">Remove</button>
						</div>
						{#if open === i}
							<div class="qbody grid">
								<label class="field wide">
									<span>Question</span>
									<input value={f.label ?? ''} oninput={(e) => setLabelOf(f, e.currentTarget.value)} maxlength="100" placeholder="What do you need it for?" />
								</label>
								{#if !isDescription(f)}
									<label class="field">
										<span>Kind of answer</span>
										<select value={f.type} onchange={(e) => setType(f, e.currentTarget.value as DefField['type'])}>
											{#each questionTypes as t (t.type)}<option value={t.type}>{t.label}</option>{/each}
										</select>
									</label>
									<label class="field">
										<span>Name</span>
										<input bind:value={f.name} pattern="[a-z][a-z0-9_]*" maxlength="40" />
										<small class="help">Answers are kept, and workflows read them, as record.fields.{f.name}.</small>
									</label>
								{/if}
								<label class="field wide">
									<span>Help</span>
									<input bind:value={f.help} maxlength="300" placeholder="A hint under the question" />
								</label>
								{#if f.type === 'choice'}
									<fieldset class="wide options">
										<legend>Choices</legend>
										{#each f.options ?? [] as o, j (j)}
											<div class="option">
												<input
													aria-label="Choice {j + 1}"
													value={o.label}
													placeholder="What people see"
													oninput={(e) => {
														// The value kept follows the label until someone sets it apart.
														if (!o.value || o.value === o.label) o.value = e.currentTarget.value;
														o.label = e.currentTarget.value;
													}}
												/>
												<input aria-label="Choice {j + 1} value" bind:value={o.value} placeholder="Value kept" />
												<input aria-label="Choice {j + 1} offered when" bind:value={o.when} placeholder="Offered when (optional)" />
												<button type="button" class="btn quiet" onclick={() => f.options!.splice(j, 1)} aria-label="Remove choice {j + 1}">×</button>
											</div>
										{/each}
										<button type="button" class="btn" onclick={() => (f.options ??= []).push({ value: '', label: '' })}>Add a choice</button>
									</fieldset>
								{/if}
								<label class="check wide"><input type="checkbox" bind:checked={f.required} /> Always required</label>
								<label class="field">
									<span>Required when</span>
									<input bind:value={f.required_when} placeholder={'record.fields.replacing'} disabled={f.required} />
								</label>
								<label class="field">
									<span>Shown when</span>
									<input bind:value={f.visible_when} placeholder={'record.fields.access_level == "administrator"'} />
								</label>
								{#if !isDescription(f)}
									<label class="field">
										<span>Starts as</span>
										<input value={valueText(f.default)} oninput={(e) => (f.default = parseValue(e.currentTarget.value, f.type))} placeholder={f.type === 'boolean' ? 'true or false' : 'Empty'} />
									</label>
								{/if}
								{#if f.type === 'text' || f.type === 'textarea'}
									<label class="field">
										<span>Longest answer</span>
										<input type="number" min="1" value={f.max_length ?? ''} oninput={(e) => (f.max_length = Number(e.currentTarget.value) || undefined)} placeholder="Characters" />
									</label>
								{/if}
								{#if f.type === 'number'}
									<label class="field">
										<span>At least</span>
										<input type="number" value={f.min ?? ''} oninput={(e) => (f.min = e.currentTarget.value === '' ? undefined : Number(e.currentTarget.value))} />
									</label>
									<label class="field">
										<span>At most</span>
										<input type="number" value={f.max ?? ''} oninput={(e) => (f.max = e.currentTarget.value === '' ? undefined : Number(e.currentTarget.value))} />
									</label>
								{/if}
								<p class="help wide">Conditions are expressions over the answers so far, such as record.fields.replacing or has(record.fields.model).</p>
							</div>
						{/if}
					</li>
				{/each}
			</ol>
			<div class="row">
				<button type="button" class="btn" onclick={addQuestion}>Add a question</button>
				<label class="check"><input type="checkbox" checked={asksDescription} onchange={(e) => toggleDescription(e.currentTarget.checked)} /> Ask “Anything else we should know?”</label>
			</div>
		</section>

		<section aria-labelledby="checks-h">
			<h2 id="checks-h">Checks</h2>
			<p class="help">Rules the answers must meet together, each with what people are told when they do not.</p>
			{#each item.form.validate ?? [] as v, i (i)}
				<div class="check-row">
					<input aria-label="Check {i + 1}" bind:value={v.when} placeholder={'!has(record.fields.needed_by) || record.fields.needed_by > now'} />
					<input aria-label="Check {i + 1} message" bind:value={v.error} placeholder="What people are told" />
					<button type="button" class="btn quiet" onclick={() => item.form.validate!.splice(i, 1)} aria-label="Remove check {i + 1}">×</button>
				</div>
			{/each}
			<button type="button" class="btn" onclick={() => (item.form.validate ??= []).push({ when: '', error: '' })}>Add a check</button>
		</section>

		<section aria-labelledby="fulfil-h">
			<h2 id="fulfil-h">Fulfilment</h2>
			<label class="field">
				<span>Workflow</span>
				<select bind:value={item.workflow}>
					<option value={undefined}>None: staff work the request as it is</option>
					{#each workflows as w (w)}<option value={w}>{w}</option>{/each}
				</select>
			</label>
			{#if item.workflow}
				<p><a href="/workflows/{def?.app ?? app}/{item.workflow}">Open {item.workflow} in the workflow builder</a></p>
			{/if}
		</section>

		{#if error}
			<div class="error" role="alert">
				<p>{error}</p>
				{#if problems.length}<ul>{#each problems as p, i (i)}<li>{p}</li>{/each}</ul>{/if}
			</div>
		{/if}
		<div class="actions">
			<button class="btn primary" disabled={busy}>{busy ? 'Saving…' : 'Save'}</button>
			{#if def?.edited}
				{#if confirmingReset}
					<span class="confirm" role="group" aria-label="Undo the changes made here?">
						<button type="button" class="btn reject" disabled={busy} onclick={reset}>{def.shipped ? 'Undo changes' : 'Remove item'}</button>
						<button type="button" class="btn" onclick={() => (confirmingReset = false)}>Keep it</button>
					</span>
				{:else}
					<button type="button" class="btn" onclick={() => (confirmingReset = true)}>
						{def.shipped ? 'Undo the changes made here' : 'Remove this item'}
					</button>
				{/if}
			{/if}
		</div>
		{#if def}
			<p class="help">
				From {def.app}{def.edited ? `, changed here${def.edited_by ? ` by ${def.edited_by}` : ''}${def.edited_at ? ` on ${when(def.edited_at)}` : ''}` : ''}.
				{def.shipped ? 'Upgrades of the app keep these changes.' : ''}
			</p>
			<details>
				<summary>YAML</summary>
				{#if def.override}<p class="help">The change, as kept (what export-local writes):</p><pre>{def.override}</pre>{/if}
				<p class="help">The item:</p>
				<pre>{def.yaml}</pre>
			</details>
		{/if}
	</form>

	<aside class="preview" aria-labelledby="preview-h">
		<h2 id="preview-h">What people see</h2>
		<div class="card-head">
			<span class="pic"><CatalogIcon name={item.icon} size={26} /></span>
			<div>
				<p class="help">{item.category || 'Category'}</p>
				<strong>{item.label || 'Label'}</strong>
				{#if item.summary}<p class="help">{item.summary}</p>{/if}
			</div>
		</div>
		<div class="pfields">
			{#each item.form.fields as f, i (i)}
				{@const pf = previewField(f)}
				<div class="field" class:wide={wide(pf)}>
					<label for="pv-{i}">{pf.label}{#if f.required}<span class="req" aria-hidden="true"> *</span>{/if}</label>
					<FieldInput field={pf} value={preview[f.name] ?? pf.default ?? null} fieldState={{}} id="pv-{i}" onchange={(v) => (preview[f.name] = v)} />
					{#if f.visible_when}<small class="help">Shown when {f.visible_when}</small>{/if}
				</div>
			{/each}
		</div>
		<p class="help">Conditions are worked out by the server as people answer; here every question shows.</p>
	</aside>
</div>

<style>
	.editor {
		display: grid;
		grid-template-columns: minmax(0, 1fr) 22rem;
		gap: 2rem;
		align-items: start;
	}
	.settings {
		display: grid;
		gap: 0.5rem;
	}
	section {
		display: grid;
		gap: 0.6rem;
		padding: 1.1rem 0;
		border-top: 1px solid var(--rule);
	}
	h2 {
		font: 700 1.25rem/1.2 var(--font-display);
	}
	.grid {
		display: grid;
		grid-template-columns: repeat(2, minmax(0, 1fr));
		gap: 0.8rem 1rem;
		align-items: start;
	}
	.wide {
		grid-column: 1 / -1;
	}
	.help {
		color: var(--ink-2);
		font-size: 0.85rem;
	}
	fieldset {
		margin: 0;
		padding: 0;
		border: 0;
	}
	legend {
		font-weight: 600;
		font-size: 0.85rem;
		margin-bottom: 0.35rem;
	}
	.icons {
		display: flex;
		flex-wrap: wrap;
		gap: 0.4rem;
	}
	.icons legend {
		width: 100%;
	}
	.icon input {
		position: absolute;
		opacity: 0;
	}
	.icon span:first-of-type {
		display: grid;
		place-items: center;
		width: 2.6rem;
		height: 2.6rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		color: var(--ink-2);
		cursor: pointer;
	}
	.icon input:checked + span {
		border-color: var(--blue);
		box-shadow: inset 0 0 0 1px var(--blue);
		background: var(--blue-soft);
		color: var(--blue);
	}
	.icon input:focus-visible + span {
		outline: 2px solid var(--blue);
		outline-offset: 2px;
	}
	.check {
		display: flex;
		align-items: center;
		gap: 0.5rem;
	}
	.audience {
		display: grid;
		grid-template-columns: repeat(2, minmax(0, 1fr));
		gap: 1rem;
		padding-left: 1.5rem;
	}
	.questions {
		list-style: none;
		margin: 0;
		padding: 0;
		display: grid;
		gap: 0.4rem;
	}
	.questions li {
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--surface);
	}
	.questions li.open {
		border-color: var(--blue);
	}
	.qhead {
		display: flex;
		align-items: center;
		gap: 0.3rem;
		padding: 0.3rem 0.4rem;
	}
	.qtitle {
		flex: 1;
		min-width: 0;
		display: grid;
		gap: 0.05rem;
		padding: 0.25rem 0.35rem;
		border: 0;
		background: none;
		color: var(--ink);
		font: inherit;
		text-align: left;
		cursor: pointer;
		overflow-wrap: anywhere;
	}
	.qbody {
		padding: 0.6rem 0.8rem 0.9rem;
		border-top: 1px solid var(--rule);
	}
	.quiet {
		min-height: 2rem;
		font-weight: 500;
	}
	.btn.reject {
		color: var(--red);
	}
	.options {
		display: grid;
		gap: 0.4rem;
		justify-items: start;
	}
	.option,
	.check-row {
		display: grid;
		grid-template-columns: 1fr 1fr 1fr auto;
		gap: 0.4rem;
		width: 100%;
	}
	.check-row {
		grid-template-columns: 2fr 1.4fr auto;
	}
	.option input,
	.check-row input {
		min-width: 0;
		min-height: 2.5rem;
		padding: 0.5rem 0.65rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--raised);
		color: var(--ink);
		font: inherit;
	}
	.option input:focus-visible,
	.check-row input:focus-visible {
		outline-offset: 0;
		border-color: var(--focus);
	}
	.row,
	.actions,
	.confirm {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.75rem;
	}
	.actions {
		padding-top: 1rem;
		border-top: 1px solid var(--rule);
	}
	.error {
		color: var(--red);
		font-weight: 600;
	}
	.error ul {
		margin: 0.3rem 0 0;
		padding-left: 1.2rem;
		font-weight: 400;
	}
	pre {
		padding: 0.6rem 0.75rem;
		background: var(--raised);
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		font-size: 0.8rem;
		overflow-x: auto;
	}
	.preview {
		position: sticky;
		top: 0;
		display: grid;
		gap: 0.8rem;
		padding: 1rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--surface);
	}
	.card-head {
		display: flex;
		gap: 0.7rem;
		align-items: flex-start;
	}
	.pic {
		flex: none;
		display: grid;
		place-items: center;
		width: 2.8rem;
		height: 2.8rem;
		border-radius: var(--radius);
		background: var(--blue-soft);
		color: var(--blue);
	}
	.pfields {
		display: grid;
		gap: 0.8rem;
	}
	.pfields .field {
		display: grid;
		gap: 0.3rem;
	}
	.req {
		color: var(--red);
	}
	@media (max-width: 60rem) {
		.editor {
			grid-template-columns: minmax(0, 1fr);
		}
		.preview {
			position: static;
		}
	}
	@media (max-width: 40rem) {
		.grid,
		.audience {
			grid-template-columns: minmax(0, 1fr);
		}
		.option {
			grid-template-columns: 1fr 1fr auto;
		}
		.option input:nth-child(3) {
			grid-column: 1 / 3;
		}
		.check-row {
			grid-template-columns: 1fr auto;
		}
	}
</style>
