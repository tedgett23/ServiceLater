/** The SQL workbench's API (docs/query.md): queries are sent with the HTTP QUERY method (RFC 10008). */
import { api, ApiError } from './api';

export interface QueryColumn {
	name: string;
	type: string;
}

export interface QueryResult {
	columns: QueryColumn[];
	rows: unknown[][];
	truncated: boolean;
	duration_ms: number;
}

/** Why a query was not run or did not work, and where (line and column from 1; 0 for the query as a whole). */
export interface QueryProblem {
	error: string;
	line: number;
	column: number;
	timeout?: boolean;
}

export interface BuiltinView {
	name: string;
	description: string;
	columns: { name: string; type: string; description: string }[];
}

import type { ChartSpec } from './charts/resultChart';

export interface SavedView {
	name: string;
	description: string;
	sql: string;
	shared: boolean;
	columns: QueryColumn[];
	uses: string[];
	/** How the workbench draws it; null: a table. */
	chart: ChartSpec | null;
	mine: boolean;
	owner_name: string;
	created_at: string;
	updated_at: string;
}

export interface QueryRun {
	id: string;
	sql: string;
	params: string[];
	outcome: 'ok' | 'refused' | 'failed' | 'timeout';
	error: string;
	row_count: number;
	truncated: boolean;
	duration_ms: number;
	started_at: string;
}

/** A failure as the workbench shows it: the server's problem when it gave one, else its message. */
export function problemOf(e: unknown): QueryProblem {
	if (e instanceof ApiError) {
		const b = e.body as Partial<QueryProblem> | undefined;
		return { error: b?.error ?? e.message, line: b?.line ?? 0, column: b?.column ?? 0, timeout: b?.timeout };
	}
	return { error: e instanceof Error ? e.message : 'Something went wrong', line: 0, column: 0 };
}

export const runQuery = (sql: string, params: string[]) =>
	api<QueryResult>('/api/v1/query', { method: 'QUERY', body: { sql, params } });

export const loadSchema = () => api<{ views: BuiltinView[]; saved_views: SavedView[] }>('/api/v1/query/schema');

export const loadRuns = () => api<{ items: QueryRun[] }>('/api/v1/query/runs').then((r) => r.items);

export const saveView = (name: string, body: { sql: string; description: string; shared: boolean; chart: ChartSpec | null }) =>
	api<SavedView>(`/api/v1/views/${encodeURIComponent(name)}`, { method: 'PUT', body });

export const removeView = (name: string) => api(`/api/v1/views/${encodeURIComponent(name)}`, { method: 'DELETE' });

/** A cell as text: null stays visible, JSON values are written out. */
export function cellText(v: unknown): string {
	if (v === null || v === undefined) return 'null';
	if (typeof v === 'object') return JSON.stringify(v);
	return String(v);
}

/** The result as CSV for a spreadsheet: quoted where needed, and cells that would run as formulas kept as text. */
export function toCSV(r: QueryResult): string {
	const cell = (v: unknown) => {
		let s = v === null || v === undefined ? '' : cellText(v);
		if (/^[=+\-@\t\r]/.test(s)) s = `'${s}`;
		return /[",\n\r]/.test(s) ? `"${s.replaceAll('"', '""')}"` : s;
	};
	return [r.columns.map((c) => cell(c.name)).join(','), ...r.rows.map((row) => row.map(cell).join(','))].join('\r\n');
}
