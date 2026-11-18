/**
 * Record types' forms as the form editor changes them (GET /forms/{type}/definition): the same definition as a
 * forms/<type>.yaml file. The server checks everything when a form is saved; these only move fields about and tidy
 * what is sent.
 */
import type { DefField } from './catalogEditor';
import type { SimpleField } from './conditions';
import { priorityLabel } from './format';
import type { FormSection } from './types';

/** One of a type's own fields: a form may relabel, explain, lay out and set conditions on it. */
export interface CoreFieldInfo {
	name: string;
	label: string;
	type: string;
	options?: { value: string; label: string }[];
	/** May be asked when filing one. */
	on_create: boolean;
	/** May be laid out on the record. */
	detail: boolean;
}

export interface EditForm {
	record_type: string;
	fields: DefField[];
	create: string[];
	create_sections: FormSection[];
	detail: FormSection[];
	/** Absent when the form has no checks. */
	validate: { when: string; error: string }[];
}

export interface FormDefResponse {
	record_type: string;
	label: string;
	form: EditForm;
	app: string;
	builtin: boolean;
	shipped: boolean;
	edited: boolean;
	edited_at?: string;
	edited_by?: string | null;
	layers: string[];
	core_fields: CoreFieldInfo[];
	/** Records holding a value, by extra field name. */
	values: Record<string, number>;
	yaml: string;
	override?: string;
	path: string;
}

export interface FormEntry {
	record_type: string;
	label: string;
	prefix: string;
	app: string;
	builtin: boolean;
	catalog: boolean;
	edited: boolean;
	extra_fields: number;
}

/** The two layouts: the form for filing one, and the record. */
export type LayoutKey = 'create_sections' | 'detail';

/** The list a field sits in: a section's flow, or one of its columns. */
const listsOf = (s: FormSection): string[][] => (s.columns?.length ? s.columns : [s.fields]);

/** Keeps a section's fields in step with its columns. */
export function sync(s: FormSection) {
	if (s.columns?.length) s.fields = s.columns.flat();
	if (s.wide) s.wide = s.wide.filter((n) => s.fields.includes(n));
}

/** Where a field is in a layout. */
export function locate(sections: FormSection[], name: string): { section: number; column: number; index: number } | null {
	for (let si = 0; si < sections.length; si++) {
		const lists = listsOf(sections[si]);
		for (let ci = 0; ci < lists.length; ci++) {
			const i = lists[ci].indexOf(name);
			if (i >= 0) return { section: si, column: ci, index: i };
		}
	}
	return null;
}

/** Takes a field off a layout. */
export function takeOut(sections: FormSection[], name: string) {
	for (const s of sections) {
		for (const l of listsOf(s)) {
			const i = l.indexOf(name);
			if (i >= 0) l.splice(i, 1);
		}
		sync(s);
	}
}

/** Puts a field at a place in a layout (taking it from where it was). */
export function putIn(sections: FormSection[], name: string, section: number, column: number, index: number) {
	const from = locate(sections, name);
	takeOut(sections, name);
	// Taken from earlier in the same list, the place moves up one.
	if (from && from.section === section && from.column === column && from.index < index) index--;
	const s = sections[section];
	const list = listsOf(s)[column] ?? listsOf(s)[0];
	list.splice(Math.max(0, Math.min(index, list.length)), 0, name);
	sync(s);
}

/** Moves a field one place up or down, into the end of the previous list or the start of the next at its edges. */
export function moveBy(sections: FormSection[], name: string, by: -1 | 1) {
	const at = locate(sections, name);
	if (!at) return;
	const lists: { section: number; column: number; list: string[] }[] = [];
	sections.forEach((s, si) => listsOf(s).forEach((l, ci) => lists.push({ section: si, column: ci, list: l })));
	const li = lists.findIndex((l) => l.section === at.section && l.column === at.column);
	const here = lists[li].list;
	const to = at.index + by;
	if (to >= 0 && to < here.length) {
		[here[at.index], here[to]] = [here[to], here[at.index]];
		sync(sections[at.section]);
		return;
	}
	const next = lists[li + by];
	if (!next) return;
	putIn(sections, name, next.section, next.column, by < 0 ? next.list.length : 0);
}

/** Whether a field can move up or down at all. */
export function canMove(sections: FormSection[], name: string, by: -1 | 1): boolean {
	const at = locate(sections, name);
	if (!at) return false;
	const lists = sections.flatMap(listsOf);
	let li = 0;
	for (let si = 0; si < at.section; si++) li += listsOf(sections[si]).length;
	li += at.column;
	return by < 0 ? at.index > 0 || li > 0 : at.index < lists[li].length - 1 || li < lists.length - 1;
}

/** Sets how many columns a section has (0: automatic). Fields are dealt out in order, or gathered back. */
export function setColumns(s: FormSection, n: number) {
	const all = s.columns?.length ? s.columns.flat() : s.fields;
	if (!n) {
		s.fields = [...all];
		delete s.columns;
		return;
	}
	const per = Math.ceil(all.length / n) || 0;
	s.columns = Array.from({ length: n }, (_, i) => all.slice(i * per, (i + 1) * per));
	delete s.wide;
	sync(s);
}

/** A name for a new field, from its label, that no other field has. */
export function freshName(base: string, taken: string[]): string {
	let name = base;
	for (let i = 2; taken.includes(name); i++) name = `${base}_${i}`;
	return name;
}

/**
 * The form as it is saved: fields taken off removed (and from every layout), empty settings dropped, the plain
 * list for filing worked out from its sections, and blank checks left out.
 */
export function forSave(form: EditForm, removed: Set<string>, core: Set<string>): EditForm {
	// A JSON copy: the editor passes its reactive state, which structuredClone cannot copy.
	const out = JSON.parse(JSON.stringify(form)) as EditForm;
	out.fields = out.fields
		.filter((f) => !removed.has(f.name))
		.map((f) => {
			const c = { ...f };
			for (const k of Object.keys(c) as (keyof DefField)[]) {
				const v = c[k];
				if (v === '' || v === undefined || v === null || (v === false && k !== 'history')) delete c[k];
			}
			if (c.options) c.options = c.options.filter((o) => o.value.trim()).map((o) => ({ value: o.value, label: o.label || o.value, ...(o.when ? { when: o.when } : {}) }));
			return c;
		})
		// A record's own field that changes nothing need not be listed.
		.filter((f) => !core.has(f.name) || Object.keys(f).length > 1);
	for (const key of ['create_sections', 'detail'] as const) {
		for (const name of removed) takeOut(out[key], name);
		out[key] = out[key].filter((s, i) => s.fields.length > 0 || (key === 'create_sections' && i === 0));
		for (const s of out[key]) {
			if (!s.label) delete s.label;
			if (!s.wide?.length) delete s.wide;
			if (!s.columns?.length) delete s.columns;
		}
	}
	out.create = out.create_sections.flatMap((s) => s.fields);
	out.validate = (out.validate ?? []).filter((v) => v.when.trim() || v.error.trim());
	return out;
}

/** The fields a form's simple conditions can test: its extra fields, and the record's status and priority. */
export function formSimpleFields(form: EditForm, except: string, statuses: { id: string; label: string }[], removed: Set<string>): SimpleField[] {
	const out: SimpleField[] = [];
	for (const f of form.fields) {
		if (f.name === except || removed.has(f.name) || !f.type || !['choice', 'boolean', 'text', 'number'].includes(f.type)) continue;
		out.push({
			name: f.name,
			label: f.label || f.name,
			type: f.type as SimpleField['type'],
			options: f.type === 'choice' ? (f.options ?? []).filter((o) => o.value).map((o) => ({ value: o.value, label: o.label || o.value })) : undefined
		});
	}
	if (statuses.length) out.push({ name: 'status', label: 'Status', type: 'status', options: statuses.map((s) => ({ value: s.id, label: s.label })) });
	out.push({
		name: 'priority',
		label: 'Priority',
		type: 'priority',
		options: Object.entries(priorityLabel).map(([p, label]) => ({ value: p, label }))
	});
	return out;
}
