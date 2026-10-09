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
}

export type StepType = 'set' | 'condition' | 'wait' | 'record.update' | 'record.transition' | 'comment.add' | 'approval' | 'task.create' | 'end';

/** Where a step can go next: the field that holds it, and what the connection is called in the diagram. */
export type Exit = { field: 'next' | 'then' | 'else' | 'on_timeout'; label: string };

export const stepTypes: { type: StepType; label: string; help: string; exits: Exit[] }[] = [
	{ type: 'set', label: 'Set variables', help: 'Work out values for later steps (vars.name).', exits: [{ field: 'next', label: 'next' }] },
	{
		type: 'condition',
		label: 'Condition',
		help: 'Go one way or the other.',
		exits: [
			{ field: 'then', label: 'then' },
			{ field: 'else', label: 'else' }
		]
	},
	{
		type: 'wait',
		label: 'Wait',
		help: 'For a time, or until something is true.',
		exits: [
			{ field: 'next', label: 'next' },
			{ field: 'on_timeout', label: 'on timeout' }
		]
	},
	{ type: 'record.update', label: 'Update the record', help: 'Change fields of the record.', exits: [{ field: 'next', label: 'next' }] },
	{ type: 'record.transition', label: 'Move the record', help: 'Move it to another status.', exits: [{ field: 'next', label: 'next' }] },
	{ type: 'comment.add', label: 'Add a comment', help: 'Write on the record.', exits: [{ field: 'next', label: 'next' }] },
	{
		type: 'approval',
		label: 'Ask for approval',
		help: 'Ask people to approve; the result is vars.<step>_decision.',
		exits: [
			{ field: 'next', label: 'next' },
			{ field: 'on_timeout', label: 'on timeout' }
		]
	},
	{
		type: 'task.create',
		label: 'File a task',
		help: 'A task under the record for a team; waits for it unless told not to. The result is vars.<step>_outcome.',
		exits: [
			{ field: 'next', label: 'next' },
			{ field: 'on_timeout', label: 'on timeout' }
		]
	},
	{ type: 'end', label: 'End', help: 'The workflow is done.', exits: [] }
];

export const typeInfo = (t: string) => stepTypes.find((s) => s.type === t);

/** The fields that hold a step id, which a rename or a delete must follow. */
export const linkFields = ['next', 'then', 'else', 'on_timeout'] as const;

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
		end: []
	};
	const out: Step = { id: s.id, type: s.type };
	for (const k of keep[s.type] ?? []) {
		const v = s[k];
		if (v === undefined || v === '' || v === null || (v === false && k !== 'wait') || (Array.isArray(v) && v.length === 0)) continue;
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
	if (s.type === 'wait' && !s.until) return all.filter((e) => e.field !== 'on_timeout');
	if (s.type === 'task.create' && s.wait === false) return all.filter((e) => e.field !== 'on_timeout');
	return all;
}
