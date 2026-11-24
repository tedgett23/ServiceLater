/**
 * Turning a query's result into a chart (the workbench's Chart tab; the server keeps the same shape with a saved view,
 * query.Chart): which chart fits the columns, and the result's rows as the series a chart draws.
 */
import type { QueryColumn } from '$lib/queries';

export type ChartType = 'line' | 'bars' | 'stacked' | 'figure';

export interface ChartSpec {
	type: ChartType;
	/** The column along the axis; empty for a figure. */
	x?: string;
	/** The numeric columns drawn, one series each, at most four. */
	y: string[];
}

export const MAX_SERIES = 4;
/** Bars beyond this many are folded into one "Other". */
export const MAX_BARS = 50;

const NUMERIC = ['int2', 'int4', 'int8', 'float4', 'float8', 'numeric'];
const TIME = ['date', 'timestamp', 'timestamptz'];
export const isNumeric = (c: QueryColumn) => NUMERIC.includes(c.type);
export const isTime = (c: QueryColumn) => TIME.includes(c.type);

/** The chart that fits a result best, or null when there is no number to draw. */
export function guessChart(columns: QueryColumn[], rowCount: number): ChartSpec | null {
	const nums = columns.filter(isNumeric);
	if (!nums.length) return null;
	const time = columns.find(isTime);
	const other = columns.find((c) => !isNumeric(c) && !isTime(c));
	if (rowCount === 1 && (columns.length === 1 || (!time && !other && nums.length === 1))) return { type: 'figure', y: [nums[0].name] };
	if (time) return { type: 'line', x: time.name, y: nums.slice(0, MAX_SERIES).map((c) => c.name) };
	if (other) return { type: 'bars', x: other.name, y: nums.slice(0, MAX_SERIES).map((c) => c.name) };
	if (nums.length >= 2) return { type: 'line', x: nums[0].name, y: nums.slice(1, MAX_SERIES + 1).map((c) => c.name) };
	return { type: 'figure', y: [nums[0].name] };
}

/** Whether a spec still fits a result's columns (after the query changed). */
export function fits(spec: ChartSpec, columns: QueryColumn[]): boolean {
	const byName = new Map(columns.map((c) => [c.name, c]));
	if (spec.type !== 'figure' && (!spec.x || !byName.has(spec.x))) return false;
	return spec.y.length > 0 && spec.y.every((y) => byName.get(y) && isNumeric(byName.get(y)!));
}

/** A cell as a number for a chart: numbers as they are, numeric text parsed, anything else none. */
export function num(v: unknown): number | null {
	if (typeof v === 'number') return Number.isFinite(v) ? v : null;
	if (typeof v === 'string' && v.trim() !== '' && Number.isFinite(Number(v))) return Number(v);
	return null;
}

export interface Series {
	key: string;
	color: string;
}

/** Fixed order: the first column drawn is always series-1. Never cycled: at most four are drawn. */
export const COLORS = ['var(--series-1)', 'var(--series-2)', 'var(--series-3)', 'var(--series-4)'];

export interface Category {
	label: string;
	values: (number | null)[];
	/** How many rows "Other" stands for. */
	folded?: number;
}

/** Bars: one category per row in the query's own order, the rest past MAX_BARS summed into "Other". */
export function categories(columns: QueryColumn[], rows: unknown[][], spec: ChartSpec): Category[] {
	const xi = columns.findIndex((c) => c.name === spec.x);
	const yi = spec.y.map((y) => columns.findIndex((c) => c.name === y));
	const all = rows.map((r) => ({
		label: r[xi] === null || r[xi] === undefined ? '(none)' : typeof r[xi] === 'object' ? JSON.stringify(r[xi]) : String(r[xi]),
		values: yi.map((i) => num(r[i]))
	}));
	if (all.length <= MAX_BARS) return all;
	const kept = all.slice(0, MAX_BARS - 1);
	const rest = all.slice(MAX_BARS - 1);
	const other = spec.y.map((_, s) => rest.reduce((sum, c) => sum + (c.values[s] ?? 0), 0));
	return [...kept, { label: `Other (${rest.length.toLocaleString()} more)`, values: other, folded: rest.length }];
}

export interface Point {
	x: number | string;
	/** What the tooltip says for this x. */
	label: string;
	values: (number | null)[];
}

/** A line: rows by x (times and numbers in order, text as the query gave it). */
export function points(columns: QueryColumn[], rows: unknown[][], spec: ChartSpec): { points: Point[]; kind: 'time' | 'number' | 'text'; utc: boolean } {
	const xc = columns.find((c) => c.name === spec.x);
	const xi = columns.findIndex((c) => c.name === spec.x);
	const yi = spec.y.map((y) => columns.findIndex((c) => c.name === y));
	const kind = xc && isTime(xc) ? 'time' : xc && isNumeric(xc) ? 'number' : 'text';
	const out: Point[] = [];
	for (const r of rows) {
		const raw = r[xi];
		let x: number | string;
		let label: string;
		if (kind === 'time') {
			const t = typeof raw === 'string' ? Date.parse(raw) : NaN;
			if (Number.isNaN(t)) continue;
			x = t;
			label = xc?.type === 'date' ? new Date(t).toLocaleDateString(undefined, { timeZone: 'UTC', dateStyle: 'medium' }) : new Date(t).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
		} else if (kind === 'number') {
			const n = num(raw);
			if (n === null) continue;
			x = n;
			label = n.toLocaleString();
		} else {
			x = raw === null || raw === undefined ? '(none)' : String(raw);
			label = x;
		}
		out.push({ x, label, values: yi.map((i) => num(r[i])) });
	}
	if (kind !== 'text') out.sort((a, b) => (a.x as number) - (b.x as number));
	// A date is a day, not an instant: it is drawn and labelled in UTC so it never shifts by the reader's time zone.
	return { points: out, kind, utc: xc?.type === 'date' };
}

const compact = new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 });
const full = new Intl.NumberFormat(undefined, { maximumFractionDigits: 2 });
/** A value on an axis or a bar's end: short (12.9K). */
export const short = (n: number) => (Math.abs(n) >= 10000 ? compact.format(n) : full.format(n));
/** A value in a tooltip or a figure: whole. */
export const whole = (n: number | null) => (n === null ? 'none' : full.format(n));
