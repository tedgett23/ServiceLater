/**
 * Catalog items as the editor changes them: the same definition as their YAML file (GET /catalog-items/{name}). The
 * server checks everything when an item is saved; this only shapes what the editor shows and sends.
 */
import type { SimpleField } from './conditions';
import type { FieldType, FieldValue, FormField } from './types';

/** A value as a definition holds it: an =expression, or a literal. */
export interface DefValue {
	expr?: string;
	literal?: string | number | boolean;
}

/** A question: an extra field (it has a type), or the request's own description (it has none). */
export interface DefField {
	name: string;
	type?: Exclude<FieldType, 'priority'>;
	label?: string;
	help?: string;
	options?: { value: string; label: string; when?: string }[];
	required?: boolean;
	required_when?: string;
	visible_when?: string;
	read_only_when?: string;
	default?: DefValue;
	max_length?: number;
	min?: number;
	max?: number;
	history?: boolean;
	/** Personal data: a clone that masks form fields leaves its values out. */
	mask?: boolean;
}

export interface DefForm {
	record_type?: string;
	fields: DefField[];
	create: string[];
	detail: { label?: string; fields: string[] }[];
	validate?: { when: string; error: string }[];
	title_given?: boolean;
}

export interface ItemDef {
	name: string;
	label: string;
	category: string;
	summary?: string;
	description?: string;
	icon?: string;
	order?: number;
	available_to?: { roles?: string[]; groups?: string[] };
	title?: DefValue;
	workflow?: string;
	form: DefForm;
}

/** An item as GET /catalog-items/{name} answers. */
export interface ItemDefResponse {
	item: ItemDef;
	app: string;
	edited: boolean;
	shipped: boolean;
	edited_at?: string;
	edited_by?: string | null;
	layers: string[];
	yaml: string;
	override?: string;
	path: string;
	workflows: string[];
}

/** An item in the editor's list. */
export interface ItemEntry {
	name: string;
	label: string;
	category: string;
	summary?: string;
	icon?: string;
	app: string;
	available_to?: { roles?: string[]; groups?: string[] };
	edited: boolean;
	shipped: boolean;
}

export const questionTypes: { type: Exclude<FieldType, 'priority'>; label: string }[] = [
	{ type: 'text', label: 'Short text' },
	{ type: 'textarea', label: 'Long text' },
	{ type: 'choice', label: 'Choice' },
	{ type: 'boolean', label: 'Yes or no' },
	{ type: 'number', label: 'Number' },
	{ type: 'date', label: 'Date' },
	{ type: 'datetime', label: 'Date and time' }
];

/** The request's own description, which an item may ask for as "anything else". */
export const isDescription = (f: DefField) => f.name === 'description' && !f.type;

/** A value as the person types it: =expression, or a literal. */
export function valueText(v: DefValue | null | undefined): string {
	if (!v) return '';
	if (v.expr) return `=${v.expr}`;
	return v.literal === undefined ? '' : String(v.literal);
}

/** What the person typed, as a value for a question of this type. */
export function parseValue(text: string, type?: string): DefValue | undefined {
	if (text === '') return undefined;
	if (text.startsWith('=')) return { expr: text.slice(1) };
	if (type === 'boolean') return { literal: text === 'true' };
	if (type === 'number' && /^-?\d{1,15}$/.test(text)) return { literal: Number(text) };
	return { literal: text };
}

/** A name for a question from its label: lowercase words joined by underscores. */
export function nameFrom(label: string, taken: string[]): string {
	const base =
		label
			.toLowerCase()
			.replace(/[^a-z0-9]+/g, '_')
			.replace(/^_+|_+$/g, '')
			.replace(/^(\d)/, 'q_$1')
			.slice(0, 40) || 'question';
	let name = base;
	for (let n = 2; taken.includes(name); n++) name = `${base}_${n}`;
	return name;
}

/** An item name from its label: lowercase words joined by hyphens. */
export const itemNameFrom = (label: string) =>
	label
		.toLowerCase()
		.replace(/[^a-z0-9]+/g, '-')
		.replace(/^-+|-+$/g, '')
		.replace(/^(\d)/, 'item-$1')
		.slice(0, 60);

/**
 * The layouts for the questions as listed: all are asked in order; the record shows them in its sections, keeping
 * the sections the item had (and their names) and putting new questions in the last one.
 */
export function layoutFor(form: DefForm): Pick<DefForm, 'create' | 'detail'> {
	const names = form.fields.map((f) => f.name);
	const shown = form.fields.filter((f) => !isDescription(f)).map((f) => f.name);
	const kept = form.detail.map((s) => ({ ...s, fields: s.fields.filter((n) => shown.includes(n)) })).filter((s) => s.fields.length);
	const placed = new Set(kept.flatMap((s) => s.fields));
	const rest = shown.filter((n) => !placed.has(n));
	if (rest.length) {
		if (kept.length) kept[kept.length - 1].fields.push(...rest);
		else kept.push({ label: 'Details', fields: rest });
	}
	return { create: names, detail: kept };
}

/** A question as the order form renders it, for the preview. */
export function previewField(f: DefField): FormField {
	const def = f.default?.literal as FieldValue | undefined;
	return {
		name: f.name,
		type: f.type ?? 'textarea',
		label: f.label || (isDescription(f) ? 'Details' : f.name),
		help: f.help,
		core: !f.type,
		options: f.options?.map((o) => ({ value: o.value, label: o.label })),
		required: f.required,
		default: def,
		max_length: f.max_length,
		min: f.min,
		max: f.max
	};
}

/** The fields a simple condition can test, other than the one being edited: choices, yes-or-no, text and numbers. */
export function simpleFields(fields: DefField[], except: string): SimpleField[] {
	const out: SimpleField[] = [];
	for (const f of fields) {
		if (f.name === except || !f.type || !['choice', 'boolean', 'text', 'number'].includes(f.type)) continue;
		out.push({
			name: f.name,
			label: f.label || f.name,
			type: f.type as SimpleField['type'],
			options: f.type === 'choice' ? (f.options ?? []).filter((o) => o.value).map((o) => ({ value: o.value, label: o.label || o.value })) : undefined
		});
	}
	return out;
}
