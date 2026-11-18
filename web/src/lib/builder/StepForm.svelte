<script lang="ts">
	import { api } from '$lib/api';
	import { lifecycle } from '$lib/lifecycle.svelte';
	import QueryEditor from './QueryEditor.svelte';
	import ScriptEditor from './ScriptEditor.svelte';
	import { exitsOf, linkOf, parseValue, scriptTemplate, setLink, stepTypes, typeInfo, valueText, type NamedValue, type Step, type StepType } from './workflow';

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
		app = '',
		workflowName = '',
		appWorkflows = [],
		appSecrets = [],
		appHosts = [],
		canScript = false,
		onchange,
		onstart,
		ondelete
	}: {
		step: Step;
		steps: Step[];
		recordType: string;
		isStart: boolean;
		editable: boolean;
		/** The workflow's app: its script files are what a script step may load. */
		app?: string;
		/** The workflow being edited, and its app's workflows, secrets and allowed hosts. */
		workflowName?: string;
		appWorkflows?: string[];
		appSecrets?: string[];
		appHosts?: string[];
		canScript?: boolean;
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
		if (step.next && typeInfo(t)?.exits.some((e) => e.key === 'next')) next.next = step.next;
		if (t === 'script') next.script = scriptTemplate;
		if (t === 'http.request') next.method = 'GET';
		if (t === 'query') next.find = { type: recordType };
		if (t === 'record.create') next.title = { literal: '' };
		if (t === 'notify') next.recipients = ['assignee'];
		if (t === 'comment.add') next.body = { literal: '' };
		if (t === 'task.create') next.title = { literal: '' };
		onchange(next);
	}

	type PairKey = 'vars' | 'set' | 'inputs' | 'headers' | 'fields';
	function setList(key: PairKey, list: NamedValue[]) {
		set(key, list);
	}
	const list = (key: PairKey) => (step[key] ?? []) as NamedValue[];

	function setExit(key: string, to: string) {
		const next = $state.snapshot(step) as Step;
		setLink(next, key, to || undefined);
		onchange(next);
	}

	// A script's branches: names it may return as next, each to a step.
	let newBranch = $state('');
	function addBranch() {
		const name = newBranch.trim().toLowerCase().replace(/[^a-z0-9_]+/g, '_');
		if (!name || step.branches?.[name] !== undefined) return;
		set('branches', { ...(step.branches ?? {}), [name]: '' });
		newBranch = '';
	}
	function removeBranch(name: string) {
		const b = { ...(step.branches ?? {}) };
		delete b[name];
		set('branches', b);
	}

	// Trying the script against a record, as you: what it would change. Nothing is changed.
	interface TestResult {
		ok: boolean;
		problem?: { line: number; message: string };
		aborted?: string;
		changes: { reference: string; title: string; fields?: Record<string, unknown>; status?: string; comment?: string }[];
		comments: { reference: string; comment?: string; internal?: boolean }[];
		vars?: Record<string, unknown>;
		next?: string;
		printed: string[];
		millis: number;
	}
	let testRecord = $state('');
	let testing = $state(false);
	let test = $state<TestResult | null>(null);
	let testError = $state('');
	async function runTest() {
		testing = true;
		testError = '';
		try {
			test = await api<TestResult>('/api/v1/scripts/test', { method: 'POST', body: { script: step.script ?? '', record: testRecord.trim(), where: 'workflow', app } });
		} catch (e) {
			test = null;
			testError = e instanceof Error ? e.message : 'The test did not run';
		} finally {
			testing = false;
		}
	}
	const fieldText = (f: Record<string, unknown> | undefined) =>
		Object.entries(f ?? {})
			.map(([k, v]) => `${k} = ${JSON.stringify(v)}`)
			.join(', ');
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

	{#if step.type === 'approval' || (step.type === 'wait' && step.until) || (step.type === 'task.create' && step.wait !== false) || step.type === 'subflow' || step.type === 'foreach' || step.type === 'http.request'}
		<div class="field">
			<label for="step-timeout">Time limit</label>
			<input id="step-timeout" value={step.timeout ?? ''} disabled={!editable} placeholder={step.type === 'http.request' ? '10s (at most 30s)' : '5d'} onchange={(e) => set('timeout', e.currentTarget.value.trim())} />
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

	{#snippet pairs(key: PairKey, legend: string, add: string, namePh: string)}
		<fieldset>
			<legend>{legend}</legend>
			{#each list(key) as nv, i (i)}
				<div class="pair">
					<input
						aria-label="{legend}: name {i + 1}"
						value={nv.name}
						disabled={!editable}
						placeholder={namePh}
						onchange={(e) => setList(key, list(key).map((x, j) => (j === i ? { ...x, name: e.currentTarget.value.trim() } : x)))}
					/>
					<input
						aria-label="{legend}: value {i + 1}"
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
				<button type="button" class="btn" onclick={() => setList(key, [...list(key), { name: '', value: null }])}>{add}</button>
			{/if}
		</fieldset>
	{/snippet}

	{#if step.type === 'set'}{@render pairs('vars', 'Variables', 'Add a variable', 'name')}{/if}
	{#if step.type === 'record.update'}{@render pairs('set', 'Fields to change', 'Add a field', 'priority')}{/if}

	{#if step.type === 'subflow' || step.type === 'foreach'}
		<div class="field">
			<label for="step-workflow">Workflow to run</label>
			<select id="step-workflow" value={step.workflow ?? ''} disabled={!editable} onchange={(e) => set('workflow', e.currentTarget.value)}>
				<option value="">Choose one of {app || 'the app'}’s workflows</option>
				{#each appWorkflows.filter((n) => n !== workflowName) as n (n)}<option value={n}>{n}</option>{/each}
			</select>
		</div>
		{#if step.type === 'foreach'}
			<div class="field">
				<label for="step-items">For each item of</label>
				<textarea id="step-items" rows="2" value={step.items ?? ''} disabled={!editable} placeholder="vars.find_ids" onchange={(e) => set('items', e.currentTarget.value)}></textarea>
				<small class="hint">An expression giving a list. Each run gets vars.item and vars.index.</small>
			</div>
			<div class="field">
				<label for="step-each">The items are</label>
				<select id="step-each" value={step.each ?? 'value'} disabled={!editable} onchange={(e) => set('each', e.currentTarget.value === 'value' ? undefined : 'record')}>
					<option value="value">Values</option>
					<option value="record">Records (ids or references): each run is for its record</option>
				</select>
			</div>
			<div class="field">
				<label for="step-parallel">At once</label>
				<input id="step-parallel" type="number" min="1" max="10" value={step.parallel ?? 1} disabled={!editable} onchange={(e) => set('parallel', Number(e.currentTarget.value) > 1 ? Number(e.currentTarget.value) : undefined)} />
			</div>
			<label class="check"><input type="checkbox" checked={!!step.keep_going} disabled={!editable} onchange={(e) => set('keep_going', e.currentTarget.checked || undefined)} /> Carry on when one fails</label>
		{:else}
			<div class="field">
				<label for="step-record">For the record</label>
				<input id="step-record" value={valueText(step.record)} disabled={!editable} placeholder="This run’s record, or =vars.find_ids[0]" onchange={(e) => set('record', parseValue(e.currentTarget.value))} />
			</div>
		{/if}
		{@render pairs('inputs', 'Its variables', 'Add a variable', 'name')}
	{/if}

	{#if step.type === 'http.request'}
		<div class="field">
			<label for="step-method">Method</label>
			<select id="step-method" value={step.method ?? 'GET'} disabled={!editable} onchange={(e) => set('method', e.currentTarget.value)}>
				{#each ['GET', 'POST', 'PUT', 'PATCH', 'DELETE'] as m (m)}<option value={m}>{m}</option>{/each}
			</select>
		</div>
		<div class="field">
			<label for="step-url">URL</label>
			<input id="step-url" value={valueText(step.url)} disabled={!editable} placeholder="https://api.example.com/v1/items" onchange={(e) => set('url', parseValue(e.currentTarget.value))} />
			<small class="hint">{appHosts.length ? `The app may call: ${appHosts.join(', ')}.` : 'The app may call no hosts: its manifest needs http: permissions.'}</small>
		</div>
		{@render pairs('headers', 'Headers', 'Add a header', 'X-Source')}
		{#if step.method && step.method !== 'GET' && step.method !== 'DELETE'}
			<div class="field">
				<label for="step-reqbody">Body</label>
				<textarea id="step-reqbody" rows="3" value={valueText(step.body)} disabled={!editable} placeholder={'={"title": record.title, "priority": record.priority}'} onchange={(e) => set('body', parseValue(e.currentTarget.value))}></textarea>
				<small class="hint">Text, or =an expression: a map or list is sent as JSON.</small>
			</div>
		{/if}
		<div class="field">
			<label for="step-secret">Sign in with</label>
			<select id="step-secret" value={step.secret ?? ''} disabled={!editable} onchange={(e) => (e.currentTarget.value ? onchange({ ...$state.snapshot(step), secret: e.currentTarget.value, auth: step.auth ?? 'bearer' } as Step) : onchange({ ...$state.snapshot(step), secret: undefined, auth: undefined, auth_header: undefined } as Step))}>
				<option value="">Nothing</option>
				{#each appSecrets as s (s)}<option value={s}>The {s} secret</option>{/each}
			</select>
		</div>
		{#if step.secret}
			<div class="field">
				<label for="step-auth">Sent as</label>
				<select id="step-auth" value={step.auth ?? 'bearer'} disabled={!editable} onchange={(e) => set('auth', e.currentTarget.value)}>
					<option value="bearer">Bearer token</option>
					<option value="basic">Basic (user:password)</option>
					<option value="header">A header of its own</option>
				</select>
			</div>
			{#if step.auth === 'header'}
				<div class="field">
					<label for="step-auth-header">Header</label>
					<input id="step-auth-header" value={step.auth_header ?? ''} disabled={!editable} placeholder="X-Api-Key" onchange={(e) => set('auth_header', e.currentTarget.value.trim())} />
				</div>
			{/if}
		{/if}
		<div class="field">
			<label for="step-retries">Tries again</label>
			<input id="step-retries" type="number" min="0" max="5" value={step.retries ?? 0} disabled={!editable} onchange={(e) => set('retries', Number(e.currentTarget.value) || undefined)} />
			<small class="hint">On a network fault, a 429 or a 5xx.</small>
		</div>
	{/if}

	{#if step.type === 'query'}
		<QueryEditor query={step.find ?? { type: recordType }} {editable} idPrefix="step-find" onchange={(q) => set('find', q)} />
	{/if}

	{#if step.type === 'record.create'}
		<div class="field">
			<label for="step-rtype">Type</label>
			<select id="step-rtype" value={step.record_type ?? ''} disabled={!editable} onchange={(e) => set('record_type', e.currentTarget.value)}>
				<option value="">Choose a type</option>
				{#each lifecycle.types as t (t.type)}<option value={t.type}>{t.label}</option>{/each}
			</select>
		</div>
		<div class="field">
			<label for="step-rtitle">Title</label>
			<input id="step-rtitle" value={valueText(step.title)} disabled={!editable} placeholder={'="Follow up on " + record.reference'} onchange={(e) => set('title', parseValue(e.currentTarget.value))} />
		</div>
		<div class="field">
			<label for="step-rdesc">Details</label>
			<textarea id="step-rdesc" rows="2" value={valueText(step.description)} disabled={!editable} onchange={(e) => set('description', parseValue(e.currentTarget.value))}></textarea>
		</div>
		<div class="field">
			<label for="step-rgroup">Team</label>
			<input id="step-rgroup" value={valueText(step.group)} disabled={!editable} placeholder="Service Desk" onchange={(e) => set('group', parseValue(e.currentTarget.value))} />
		</div>
		<div class="field">
			<label for="step-rparent">Under</label>
			<input id="step-rparent" value={valueText(step.parent)} disabled={!editable} placeholder="=record.id (optional)" onchange={(e) => set('parent', parseValue(e.currentTarget.value))} />
		</div>
		{@render pairs('fields', 'Its fields', 'Add a field', 'category')}
	{/if}

	{#if step.type === 'notify'}
		<div class="field">
			<label for="step-template">Template</label>
			<input id="step-template" value={step.template ?? ''} disabled={!editable} placeholder="assigned" onchange={(e) => set('template', e.currentTarget.value.trim())} />
			<small class="hint">One of the app’s notification templates (notifications/).</small>
		</div>
		<fieldset>
			<legend>To</legend>
			{#each ['assignee', 'requester', 'assignment_group'] as who (who)}
				<label class="check">
					<input type="checkbox" checked={step.recipients?.includes(who)} disabled={!editable}
						onchange={(e) => set('recipients', e.currentTarget.checked ? [...(step.recipients ?? []), who] : (step.recipients ?? []).filter((r) => r !== who))} />
					{who === 'assignment_group' ? 'The assignment group' : `The ${who}`}
				</label>
			{/each}
			<input aria-label="More people (an expression)" value={(step.recipients ?? []).find((r) => r.startsWith('=')) ?? ''} disabled={!editable} placeholder="=group('CAB') (optional)"
				onchange={(e) => set('recipients', [...(step.recipients ?? []).filter((r) => !r.startsWith('=')), ...(e.currentTarget.value.trim() ? [e.currentTarget.value.trim().startsWith('=') ? e.currentTarget.value.trim() : '=' + e.currentTarget.value.trim()] : [])])} />
		</fieldset>
	{/if}

	{#if step.type === 'script'}
		<div class="field">
			<span class="label">Script</span>
			<ScriptEditor value={step.script ?? ''} readonly={!canScript} label="Script of {step.id}" {app} onchange={(text) => set('script', text)} />
			{#if editable && !canScript}<small class="hint">Changing a script needs the scripts:edit permission.</small>{/if}
			<small class="hint">
				Starlark (like Python). It reads record, vars and user, finds records with children() and get(), and asks for changes with
				update() and comment(): they are made when it ends, as the workflow’s app. Return {'{'}"vars": {'{…}'}, "next": "a_branch"{'}'} to
				set variables or leave by a branch.
			</small>
		</div>
		<fieldset>
			<legend>Branches</legend>
			{#each Object.keys(step.branches ?? {}).sort() as name (name)}
				<div class="pair branch">
					<code>{name}</code>
					{#if editable}<button type="button" class="btn" aria-label="Remove branch {name}" onclick={() => removeBranch(name)}>×</button>{/if}
				</div>
			{/each}
			{#if editable}
				<div class="pair">
					<input aria-label="New branch name" bind:value={newBranch} placeholder="nothing_open" onkeydown={(e) => e.key === 'Enter' && (e.preventDefault(), addBranch())} />
					<button type="button" class="btn" onclick={addBranch}>Add a branch</button>
				</div>
			{/if}
			<small class="hint">Ways out besides next, chosen by returning {'{'}"next": "name"{'}'}. Connect each below or in the diagram.</small>
		</fieldset>
		<fieldset class="test" aria-label="Test run">
			<legend>Test run</legend>
			<div class="pair">
				<input aria-label="Record to test against" bind:value={testRecord} placeholder="A reference, such as REQ12" />
				<button type="button" class="btn" disabled={testing || !canScript} onclick={runTest}>{testing ? 'Running…' : 'Run'}</button>
			</div>
			<small class="hint">Runs it as you, for that record, and says what it would do. Nothing is changed.</small>
			{#if testError}<p class="bad" role="alert">{testError}</p>{/if}
			{#if test}
				<div class="result" role="status">
					{#if test.problem}
						<p class="bad">{test.problem.line ? `Line ${test.problem.line}: ` : ''}{test.problem.message}</p>
					{:else if test.aborted}
						<p class="bad">It stops with: {test.aborted}</p>
					{:else}
						<p class="good">It would succeed{test.next ? `, leaving by ${test.next}` : ''} ({test.millis} ms).</p>
					{/if}
					{#each test.changes as c, i (i)}
						<p><code>{c.reference}</code> {c.status ? `→ ${c.status}` : ''} {fieldText(c.fields)}{c.comment ? ` · comment “${c.comment}”` : ''}</p>
					{/each}
					{#each test.comments as c, i (i)}
						<p><code>{c.reference}</code> {c.internal ? 'internal note' : 'comment'} “{c.comment}”</p>
					{/each}
					{#if test.vars && Object.keys(test.vars).length}<p>vars: <code>{JSON.stringify(test.vars)}</code></p>{/if}
					{#if test.printed.length}<pre>{test.printed.join('\n')}</pre>{/if}
					{#if test.ok && !test.changes.length && !test.comments.length}<p class="hint">It would change nothing.</p>{/if}
				</div>
			{/if}
		</fieldset>
	{/if}

	{#if exitsOf(step).length}
		<fieldset>
			<legend>Then</legend>
			{#each exitsOf(step) as ex (ex.key)}
				{@const fid = `step-${ex.key.replace(':', '-')}`}
				<div class="field">
					<label for={fid}>{ex.label === 'next' ? 'Next step' : ex.key.startsWith('branch:') ? `Branch ${ex.label}` : ex.label}</label>
					<select id={fid} value={linkOf(step, ex.key) ?? ''} disabled={!editable} onchange={(e) => setExit(ex.key, e.currentTarget.value)}>
						<option value="">{ex.key === 'on_timeout' ? 'Carry on to the next step' : 'Not connected'}</option>
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
		font: 650 1.0625rem/1.3 var(--font-display);
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
	.label {
		font-size: 0.8125rem;
		font-weight: 550;
		color: var(--ink-2);
	}
	.pair.branch,
	.test .pair {
		grid-template-columns: minmax(0, 1fr) auto;
		align-items: center;
	}
	.pair.branch code {
		font: 0.8125rem var(--font-mono);
	}
	.result {
		display: grid;
		gap: 0.3rem;
		padding: 0.5rem 0.6rem;
		border: 1px solid var(--rule);
		border-radius: var(--radius);
		background: var(--surface);
		font-size: 0.8125rem;
		overflow-wrap: anywhere;
	}
	.result code,
	.result pre {
		font: 0.75rem/1.5 var(--font-mono);
	}
	.result pre {
		margin: 0;
		white-space: pre-wrap;
		color: var(--ink-2);
	}
	.good {
		color: var(--green);
		font-weight: 600;
	}
	.bad {
		color: var(--red);
		font-weight: 600;
	}
</style>
