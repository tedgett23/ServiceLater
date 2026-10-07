/** Just enough of OpenAPI 3.1 to show the API's own generated document (backend/internal/api/openapi.json). */

export interface Schema {
	type?: string | string[];
	format?: string;
	description?: string;
	properties?: Record<string, Schema>;
	required?: string[];
	items?: Schema;
	additionalProperties?: Schema;
	$ref?: string;
	anyOf?: Schema[];
	allOf?: Schema[];
}

export interface Parameter {
	name: string;
	in: 'path' | 'query';
	required?: boolean;
	schema: Schema;
}

export interface Operation {
	operationId: string;
	summary: string;
	description?: string;
	tags: string[];
	parameters?: Parameter[];
	requestBody?: { content: { 'application/json': { schema: Schema } } };
	responses: Record<string, { description: string; content?: { 'application/json': { schema: Schema } } }>;
	security: Record<string, string[]>[];
	'x-permissions'?: string[];
}

export interface Document {
	info: { title: string; version: string; description: string };
	paths: Record<string, Record<string, Operation>>;
	components: { schemas: Record<string, Schema> };
}

export interface Endpoint {
	method: string;
	path: string;
	op: Operation;
}

const order = ['get', 'post', 'put', 'patch', 'delete'];

/** Endpoints grouped by their tag, in path order. */
export function byTag(doc: Document): [string, Endpoint[]][] {
	const groups = new Map<string, Endpoint[]>();
	for (const [path, ops] of Object.entries(doc.paths).sort(([a], [b]) => a.localeCompare(b))) {
		for (const method of order) {
			const op = ops[method];
			if (!op) continue;
			const tag = op.tags[0] ?? 'other';
			if (!groups.has(tag)) groups.set(tag, []);
			groups.get(tag)!.push({ method: method.toUpperCase(), path, op });
		}
	}
	return [...groups.entries()].sort(([a], [b]) => a.localeCompare(b));
}

/** The component a $ref points at. */
export const refName = (ref: string) => ref.split('/').pop() ?? ref;

/** The schema behind nullability or a description wrapper, and whether it may be null. */
export function unwrap(s: Schema): { schema: Schema; nullable: boolean } {
	if (s.anyOf?.length === 2 && s.anyOf.some((x) => x.type === 'null')) {
		return { schema: s.anyOf.find((x) => x.type !== 'null')!, nullable: true };
	}
	if (s.allOf?.length === 1) return { schema: s.allOf[0], nullable: false };
	if (Array.isArray(s.type) && s.type.includes('null')) {
		const rest = s.type.filter((t) => t !== 'null');
		return { schema: { ...s, type: rest.length === 1 ? rest[0] : rest }, nullable: true };
	}
	return { schema: s, nullable: false };
}

/** A short name for a schema's type, such as "string (uuid)", "Ticket" or "array of Group". */
export function typeName(s: Schema): string {
	const { schema, nullable } = unwrap(s);
	let name: string;
	if (schema.$ref) name = refName(schema.$ref);
	else if (schema.type === 'array') name = `array of ${schema.items ? typeName(schema.items) : 'anything'}`;
	else if (schema.type === 'object' && schema.additionalProperties && !schema.properties) name = `map of ${typeName(schema.additionalProperties)}`;
	else if (typeof schema.type === 'string') name = schema.format ? `${schema.type} (${schema.format})` : schema.type;
	else name = 'any';
	return nullable ? `${name} or null` : name;
}

/** The inline object a schema describes (directly, or as the items of an array), if it has properties. */
export function inlineObject(s: Schema): Schema | null {
	const { schema } = unwrap(s);
	if (schema.properties) return schema;
	if (schema.type === 'array' && schema.items) {
		const inner = unwrap(schema.items).schema;
		if (inner.properties) return inner;
	}
	return null;
}

/** The component a schema refers to, directly or through an array, so it can be linked. */
export function referenced(s: Schema): string | null {
	const { schema } = unwrap(s);
	if (schema.$ref) return refName(schema.$ref);
	if (schema.type === 'array' && schema.items) return referenced(schema.items);
	return null;
}
