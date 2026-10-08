import { priorityLabel, when } from './format';
import type { ChangeFields, FieldValue, FormField, Ticket } from './types';

/** A change's own fields, which requests send under `change`. */
const changeKeys = new Set([
	'change_type',
	'risk',
	'impact',
	'planned_start',
	'planned_end',
	'implementation_plan',
	'backout_plan',
	'test_plan'
]);

/** A field's value on a ticket, wherever it lives: a column, the change's own row, or the extra fields. */
export function valueOf(t: Ticket, f: FormField): FieldValue | null {
	if (!f.core) return t.fields?.[f.name] ?? null;
	if (changeKeys.has(f.name)) return (t.change?.[f.name as keyof ChangeFields] as FieldValue | null | undefined) ?? null;
	return (t[f.name as keyof Ticket] as FieldValue | null | undefined) ?? null;
}

/** The request body that sets a field, and the same change applied to the ticket for showing at once. */
export function patchFor(t: Ticket, f: FormField, v: FieldValue | null): { body: Record<string, unknown>; optimistic: Partial<Ticket> } {
	if (!f.core) {
		const fields = { ...(t.fields ?? {}) };
		if (v === null || v === '') delete fields[f.name];
		else fields[f.name] = v;
		return { body: { fields: { [f.name]: v === '' ? null : v } }, optimistic: { fields } };
	}
	if (changeKeys.has(f.name)) {
		return { body: { change: { [f.name]: v } }, optimistic: { change: { ...t.change!, [f.name]: v } } };
	}
	return { body: { [f.name]: v }, optimistic: { [f.name]: v } as Partial<Ticket> };
}

/** A value as text to read, or '' when there is none to show. */
export function display(f: FormField, v: FieldValue | null): string {
	if (v === null || v === '') return f.options?.find((o) => o.value === '')?.label ?? '';
	switch (f.type) {
		case 'priority':
			return priorityLabel[v as 1 | 2 | 3 | 4] ?? String(v);
		case 'choice':
			return f.options?.find((o) => o.value === v)?.label ?? String(v);
		case 'boolean':
			return v ? 'Yes' : 'No';
		case 'datetime':
			return when(String(v));
		case 'date':
			return new Date(`${v}T12:00:00Z`).toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: 'numeric', timeZone: 'UTC' });
	}
	return String(v);
}

/** Long text takes a row of its own. */
export const wide = (f: FormField) => f.type === 'textarea';
