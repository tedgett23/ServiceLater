/**
 * Simple conditions: "Category is Hardware and Status is not Closed", for people who have never written CEL. They are
 * written as the CEL expression the server checks, and read back from it when it has one of the shapes written here;
 * anything else stays an expression, never rewritten. An extra field may be unset, and reading an unset field is an
 * error in CEL, so each test of one is guarded with has().
 */

export type Op = 'is' | 'is_not' | 'set' | 'not_set';

export interface Clause {
	field: string;
	op: Op;
	value?: string | number | boolean;
}

/** A field a simple condition can test: an extra field, or the record's own status or priority. */
export interface SimpleField {
	name: string;
	label: string;
	type: 'choice' | 'boolean' | 'text' | 'number' | 'status' | 'priority';
	options?: { value: string; label: string }[];
}

export const opLabels: Record<Op, string> = { is: 'is', is_not: 'is not', set: 'is filled in', not_set: 'is empty' };

/** The ops a field offers: the record's own fields always have a value. */
export const opsFor = (f: SimpleField): Op[] => (f.type === 'status' || f.type === 'priority' ? ['is', 'is_not'] : ['is', 'is_not', 'set', 'not_set']);

const core = (f: SimpleField) => f.type === 'status' || f.type === 'priority';
const ref = (f: SimpleField) => (core(f) ? `record.${f.name}` : `record.fields.${f.name}`);

function literal(f: SimpleField, v: Clause['value']): string {
	switch (f.type) {
		case 'number':
		case 'priority':
			return String(Number(v) || 0);
		case 'boolean':
			return v === true || v === 'true' ? 'true' : 'false';
		default:
			return JSON.stringify(String(v ?? ''));
	}
}

/** The CEL for clauses, all of which must hold. */
export function toExpression(clauses: Clause[], fields: SimpleField[]): string {
	const parts: string[] = [];
	for (const c of clauses) {
		const f = fields.find((x) => x.name === c.field);
		if (!f) continue;
		const r = ref(f);
		const v = literal(f, c.value);
		if (core(f)) {
			parts.push(`${r} ${c.op === 'is_not' ? '!=' : '=='} ${v}`);
			continue;
		}
		switch (c.op) {
			case 'is':
				parts.push(`has(${r}) && ${r} == ${v}`);
				break;
			case 'is_not':
				parts.push(`(!has(${r}) || ${r} != ${v})`);
				break;
			case 'set':
				parts.push(`has(${r})`);
				break;
			case 'not_set':
				parts.push(`!has(${r})`);
		}
	}
	return parts.join(' && ');
}

const VALUE = String.raw`("(?:[^"\\]|\\.)*"|true|false|-?\d+(?:\.\d+)?)`;
const shapes: { re: RegExp; read: (m: RegExpExecArray) => Clause | null }[] = [
	{
		re: new RegExp(String.raw`has\(record\.fields\.(\w+)\) && record\.fields\.(\w+) == ${VALUE}`, 'y'),
		read: (m) => (m[1] === m[2] ? { field: m[1], op: 'is', value: value(m[3]) } : null)
	},
	{
		re: new RegExp(String.raw`\(!has\(record\.fields\.(\w+)\) \|\| record\.fields\.(\w+) != ${VALUE}\)`, 'y'),
		read: (m) => (m[1] === m[2] ? { field: m[1], op: 'is_not', value: value(m[3]) } : null)
	},
	{ re: /!has\(record\.fields\.(\w+)\)/y, read: (m) => ({ field: m[1], op: 'not_set' }) },
	{ re: /has\(record\.fields\.(\w+)\)/y, read: (m) => ({ field: m[1], op: 'set' }) },
	{
		re: new RegExp(String.raw`record\.(status|priority) (==|!=) ${VALUE}`, 'y'),
		read: (m) => ({ field: m[1], op: m[2] === '==' ? 'is' : 'is_not', value: value(m[3]) })
	}
];

function value(raw: string): string | number | boolean {
	if (raw === 'true' || raw === 'false') return raw === 'true';
	if (raw.startsWith('"')) return JSON.parse(raw) as string;
	return Number(raw);
}

/** The clauses an expression is, or null when it is not one this can show simply. */
export function parseSimple(expression: string, fields: SimpleField[]): Clause[] | null {
	const s = expression.trim();
	if (!s) return null;
	const out: Clause[] = [];
	let at = 0;
	while (at < s.length) {
		if (out.length) {
			const and = /\s*&&\s*/y;
			and.lastIndex = at;
			if (!and.exec(s)) return null;
			at = and.lastIndex;
		}
		let found: Clause | null = null;
		for (const shape of shapes) {
			shape.re.lastIndex = at;
			const m = shape.re.exec(s);
			if (!m) continue;
			found = shape.read(m);
			if (found) {
				at = shape.re.lastIndex;
				break;
			}
		}
		if (!found || !fields.some((f) => f.name === found.field)) return null;
		out.push(found);
	}
	return out;
}

/**
 * Whether simple clauses hold for values (by field name; status and priority among them), for the editor's preview.
 * Only simple conditions can be worked out here: the server works out the rest.
 */
export function holds(clauses: Clause[], values: Record<string, unknown>): boolean {
	return clauses.every((c) => {
		const v = values[c.field];
		const set = v !== undefined && v !== null && v !== '';
		switch (c.op) {
			case 'set':
				return set;
			case 'not_set':
				return !set;
			case 'is':
				return set && String(v) === String(c.value);
			case 'is_not':
				return !set || String(v) !== String(c.value);
		}
	});
}

/** A condition in words, for the field list: "shown when Category is Hardware". */
export function describe(expression: string, fields: SimpleField[]): string {
	const clauses = parseSimple(expression, fields);
	if (!clauses) return 'an expression';
	return clauses
		.map((c) => {
			const f = fields.find((x) => x.name === c.field)!;
			const v = c.op === 'is' || c.op === 'is_not' ? ' ' + (f.options?.find((o) => o.value === String(c.value))?.label ?? String(c.value)) : '';
			return `${f.label} ${opLabels[c.op]}${v}`;
		})
		.join(' and ');
}
