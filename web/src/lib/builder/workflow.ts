/**
 * A workflow as the builder edits it: the same definition as its YAML file (GET /workflows/{app}/{name}). The server
 * checks everything when it is saved; this only knows enough to draw the graph and offer the right fields.
 */

/** A value as a definition holds it: an =expression, or a literal. */
export interface Value {
	expr?: string;
	literal?: string | number | boolean;
}

export interface NamedValue {
	name: string;
	value: Value | null;
}

export interface Step {
	id: string;
	type: StepType;
	next?: string;
	vars?: NamedValue[];
	if?: string;
	then?: string;
	else?: string;
	for?: string;
	until?: string;
	timeout?: string;
	on_timeout?: string;
	set?: NamedValue[];
	to?: string;
	body?: Value | null;
	internal?: boolean;
	approvers?: string;
	groups?: string;
	rule?: string;
	/** task.create: the task, the team (a group's name) it goes to, and whether to wait for it (true unless false). */
	title?: Value | null;
	description?: Value | null;
	group?: Value | null;
	wait?: boolean;
	/** script: the Starlark to run, and its named ways out besides next (name to step). */
	script?: string;
	branches?: Record<string, string>;
	/** subflow and foreach: the workflow to run, values for its variables, the record (subflow) or list (foreach). */
	workflow?: string;
	inputs?: NamedValue[];
	record?: Value | null;
	items?: string;
	each?: 'value' | 'record';
	parallel?: number;
	keep_going?: boolean;
	/** subflow, foreach and http.request: where to go when it fails. */
	on_error?: string;
	/** http.request */
	method?: string;
	url?: Value | null;
	headers?: NamedValue[];
	secret?: string;
	auth?: string;
	auth_header?: string;
	retries?: number;
	/** query */
	find?: RecordQuery;
	/** record.create */
	record_type?: string;
	fields?: NamedValue[];
	parent?: Value | null;
	/** notify */
	template?: string;
	recipients?: string[];
}

/** What a query step (or a schedule) finds: records of a type matching every filter (field → op → value). */
export interface RecordQuery {
	type: string;
	where?: Record<string, Record<string, Value>>;
	limit?: number;
}

/** When a workflow starts on its own: a cron line in a time zone, once, or once per record a query finds. */
export interface Schedule {
	cron: string;
	timezone?: string;
	for_each?: RecordQuery;
}

export interface WorkflowDef {
	app: string;
	name: string;
	record_type: string;
	start: string;
	steps: Step[];
	layout: Record<string, { x: number; y: number }>;
	edited: boolean;
	shipped: boolean;
	edited_at?: string;
	edited_by?: string | null;
	layers: string[];
	yaml: string;
	/** The edit as it is kept: only what it changes over the app's version (what export-local writes). */
	override?: string;
	path?: string;
	/** What the app may do: steps act as the app, so a step that reads or changes a record needs one of these. */
	app_permissions: string[];
	schedule?: Schedule | null;
	/** The app's workflows (what subflow and foreach run), its declared secrets and the hosts it may call. */
	app_workflows?: string[];
	app_secrets?: string[];
	app_hosts?: string[];
}

export type StepType = 'set' | 'condition' | 'wait' | 'record.update' | 'record.transition' | 'comment.add' | 'approval' | 'task.create' | 'script' | 'subflow' | 'foreach' | 'http.request' | 'query' | 'record.create' | 'notify' | 'end';

/**
 * Where a step can go next: its key (next, then, else, on_timeout, or branch:<name> for a script's branch) and what
 * the connection is called in the diagram.
 */
export type Exit = { key: string; label: string };

export const stepTypes: { type: StepType; label: string; help: string; exits: Exit[] }[] = [
	{ type: 'set', label: 'Set variables', help: 'Work out values for later steps (vars.name).', exits: [{ key: 'next', label: 'next' }] },
	{
		type: 'condition',
		label: 'Condition',
		help: 'Go one way or the other.',
		exits: [
			{ key: 'then', label: 'then' },
			{ key: 'else', label: 'else' }
		]
	},
	{
		type: 'wait',
		label: 'Wait',
		help: 'For a time, or until something is true.',
		exits: [
			{ key: 'next', label: 'next' },
			{ key: 'on_timeout', label: 'on timeout' }
		]
	},
	{ type: 'record.update', label: 'Update the record', help: 'Change fields of the record.', exits: [{ key: 'next', label: 'next' }] },
	{ type: 'record.transition', label: 'Move the record', help: 'Move it to another status.', exits: [{ key: 'next', label: 'next' }] },
	{ type: 'comment.add', label: 'Add a comment', help: 'Write on the record.', exits: [{ key: 'next', label: 'next' }] },
	{
		type: 'approval',
		label: 'Ask for approval',
		help: 'Ask people to approve; the result is vars.<step>_decision.',
		exits: [
			{ key: 'next', label: 'next' },
			{ key: 'on_timeout', label: 'on timeout' }
		]
	},
	{
		type: 'task.create',
		label: 'File a task',
		help: 'A task under the record for a team; waits for it unless told not to. The result is vars.<step>_outcome.',
		exits: [
			{ key: 'next', label: 'next' },
			{ key: 'on_timeout', label: 'on timeout' }
		]
	},
	{
		type: 'script',
		label: 'Run a script',
		help: 'Starlark that loops over records and changes them; it can set vars and choose one of its branches.',
		exits: [{ key: 'next', label: 'next' }]
	},
	{
		type: 'subflow',
		label: 'Run a subflow',
		help: 'Run another workflow of this app and wait for it; its variables come back as vars.<step>_vars.',
		exits: [
			{ key: 'next', label: 'next' },
			{ key: 'on_error', label: 'on error' },
			{ key: 'on_timeout', label: 'on timeout' }
		]
	},
	{
		type: 'foreach',
		label: 'For each',
		help: 'Run a workflow of this app for each item of a list (or each record), some at a time; results in vars.<step>_results.',
		exits: [
			{ key: 'next', label: 'next' },
			{ key: 'on_error', label: 'on error' },
			{ key: 'on_timeout', label: 'on timeout' }
		]
	},
	{
		type: 'http.request',
		label: 'Call an API',
		help: 'An HTTP request to a host the app may call; the answer is vars.<step>_status and vars.<step>_body.',
		exits: [
			{ key: 'next', label: 'next' },
			{ key: 'on_error', label: 'on error' }
		]
	},
	{ type: 'query', label: 'Find records', help: 'Records matching filters, as the app sees them: vars.<step>_ids and vars.<step>_count.', exits: [{ key: 'next', label: 'next' }] },
	{ type: 'record.create', label: 'File a record', help: 'A record of any type; its reference is vars.<step>_reference.', exits: [{ key: 'next', label: 'next' }] },
	{ type: 'notify', label: 'Notify', help: 'Send one of the app’s notification templates about the record.', exits: [{ key: 'next', label: 'next' }] },
	{ type: 'end', label: 'End', help: 'The workflow is done.', exits: [] }
];

export const typeInfo = (t: string) => stepTypes.find((s) => s.type === t);

/** The fields that hold a step id, which a rename or a delete must follow (besides a script's branches). */
export const linkFields = ['next', 'then', 'else', 'on_timeout', 'on_error'] as const;

const branchKey = 'branch:';

/** Where a step goes by an exit's key. */
export function linkOf(s: Step, key: string): string | undefined {
	if (key.startsWith(branchKey)) return s.branches?.[key.slice(branchKey.length)] || undefined;
	return (s as unknown as Record<string, string | undefined>)[key];
}

/** Points an exit at a step, or (to undefined) disconnects it; a branch keeps its name when disconnected. */
export function setLink(s: Step, key: string, to: string | undefined) {
	if (key.startsWith(branchKey)) {
		s.branches = { ...(s.branches ?? {}), [key.slice(branchKey.length)]: to ?? '' };
		return;
	}
	if (!(linkFields as readonly string[]).includes(key)) return;
	const rec = s as unknown as Record<string, string | undefined>;
	if (to) rec[key] = to;
	else delete rec[key];
}

/** Makes every exit that goes to from go to to instead, or (to null) disconnects it. */
export function retarget(s: Step, from: string, to: string | null) {
	for (const f of linkFields) if (s[f] === from) setLink(s, f, to ?? undefined);
	for (const [name, target] of Object.entries(s.branches ?? {})) if (target === from) setLink(s, branchKey + name, to ?? undefined);
}

/** A value as the person types it: =expression, or a literal. */
export function valueText(v: Value | null | undefined): string {
	if (!v) return '';
	if (v.expr) return `=${v.expr}`;
	return v.literal === undefined ? '' : String(v.literal);
}

/** What the person typed, as a value: =expression, true/false, a whole number, or text. */
export function parseValue(text: string): Value | null {
	if (text === '') return null;
	if (text.startsWith('=')) return { expr: text.slice(1) };
	if (text === 'true' || text === 'false') return { literal: text === 'true' };
	if (/^-?\d{1,15}$/.test(text)) return { literal: Number(text) };
	return { literal: text };
}

/** A new step id for a type that no step has yet: approval, approval_2... */
export function freshId(steps: Step[], type: StepType): string {
	const base = type.replace('.', '_').replace('record_', '');
	for (let n = 1; ; n++) {
		const id = n === 1 ? base : `${base}_${n}`;
		if (!steps.some((s) => s.id === id)) return id;
	}
}

/** The step without the fields its type does not use, so a changed type does not carry stray fields to the server. */
export function tidy(s: Step): Step {
	const keep: Record<StepType, (keyof Step)[]> = {
		set: ['next', 'vars'],
		condition: ['if', 'then', 'else'],
		wait: ['next', 'for', 'until', 'timeout', 'on_timeout'],
		'record.update': ['next', 'set'],
		'record.transition': ['next', 'to'],
		'comment.add': ['next', 'body', 'internal'],
		approval: ['next', 'approvers', 'groups', 'rule', 'timeout', 'on_timeout'],
		'task.create': ['next', 'title', 'description', 'group', 'wait', 'timeout', 'on_timeout'],
		script: ['next', 'script', 'branches'],
		subflow: ['next', 'workflow', 'inputs', 'record', 'timeout', 'on_timeout', 'on_error'],
		foreach: ['next', 'workflow', 'items', 'each', 'parallel', 'keep_going', 'inputs', 'timeout', 'on_timeout', 'on_error'],
		'http.request': ['next', 'method', 'url', 'headers', 'body', 'secret', 'auth', 'auth_header', 'retries', 'timeout', 'on_error'],
		query: ['next', 'find'],
		'record.create': ['next', 'record_type', 'title', 'description', 'group', 'fields', 'parent'],
		notify: ['next', 'template', 'recipients'],
		end: []
	};
	const out: Step = { id: s.id, type: s.type };
	for (const k of keep[s.type] ?? []) {
		const v = s[k];
		if (v === undefined || v === '' || v === null || (v === false && k !== 'wait') || (Array.isArray(v) && v.length === 0)) continue;
		if (k === 'branches' && Object.keys(v as object).length === 0) continue;
		if (k === 'wait' && v === true) continue; // waiting is the default
		(out as unknown as Record<string, unknown>)[k] = v;
	}
	return out;
}

/** What is happening at one step: runs there now, and the visits of runs started in the last week. */
export interface StepActivity {
	now: number;
	visits: number;
	failed: number;
	timeouts: number;
}

/** A run still going. */
export interface InFlightRun {
	id: string;
	status: 'queued' | 'running' | 'waiting';
	step: string;
	reference?: string;
	title?: string;
	started_at: string;
	wake_at?: string;
}

/** Where a workflow's runs are, from GET /workflows/{app}/{name}/activity. */
export interface WorkflowActivity {
	steps: Record<string, StepActivity>;
	in_flight: InFlightRun[];
	more: boolean;
	runs: Record<string, number>;
	since: string;
}

/** The ways out a step has as it is now: a wait for a fixed time cannot time out, so it has only next. */
export function exitsOf(s: Step): Exit[] {
	const all = typeInfo(s.type)?.exits ?? [];
	if (s.type === 'wait' && !s.until) return all.filter((e) => e.key !== 'on_timeout');
	if (s.type === 'task.create' && s.wait === false) return all.filter((e) => e.key !== 'on_timeout');
	if ((s.type === 'subflow' || s.type === 'foreach') && !s.timeout) return all.filter((e) => e.key !== 'on_timeout');
	if (s.type === 'script') return [...all, ...Object.keys(s.branches ?? {}).sort().map((name) => ({ key: branchKey + name, label: name }))];
	return all;
}

/** What a new script step starts with: an example of the shape, to change. */
export const scriptTemplate = `# The record this runs for is record; vars are the workflow's variables.
# For example: cancel the open tasks under a request, with a note.
for task in children(record, open=True):
    update(task, {"status": "cancelled"}, comment="The request was withdrawn.")
`;
