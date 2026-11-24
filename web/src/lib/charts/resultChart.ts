/**
 * Turning a query's result into a chart (the workbench's Chart tab; the server keeps the same shape with a saved view,
 * query.Chart): which chart fits the columns, and the result's rows as the series a chart draws.
 */
import type { QueryColumn } from '$lib/queries';

export type ChartType = 'line' | 'area' | 'scatter' | 'bars' | 'stacked' | 'pie' | 'doughnut' | 'figure';

export const radial = (type: ChartType) => type === 'pie' || type === 'doughnut';
export const singleMeasure = (type: ChartType) => type !== 'line' && type !== 'bars' && type !== 'stacked';

/** Axes that leave a usable measure. Scatter compares two different numeric columns. */
export function axes(type: ChartType, columns: QueryColumn[]) {
	if (type === 'scatter') return columns.filter(isNumeric).length >= 2 ? columns.filter(isNumeric) : [];
	if (radial(type)) return columns.filter(c => columns.some(y => y.name !== c.name && isNumeric(y)));
	return columns;
}

export function selectChart(type: ChartType, columns: QueryColumn[], previous: ChartSpec, axis = previous.x): ChartSpec {
	const options = axes(type, columns);
	const x = type === 'figure' ? undefined : options.find(c => c.name === axis)?.name ?? (options.find(c => !isNumeric(c)) ?? options[0])?.name;
	const measures = columns.filter(c => isNumeric(c) && (!(radial(type) || type === 'scatter') || c.name !== x));
	const keep = previous.y.filter(name => measures.some(c => c.name === name));
	return { type, x, y: (keep.length ? keep : measures.slice(0, 1).map(c => c.name)).slice(0, singleMeasure(type) ? 1 : MAX_SERIES) };
}

export interface ChartSpec {
	type: ChartType;
	/** The column along the axis; empty for a figure. */
	x?: string;
	/** The numeric columns drawn, one series each, at most four. */
	y: string[];
}

export const MAX_SERIES = 4;
/** Draw the first rows in query order; arbitrary measures cannot be summed safely. */
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
	if (spec.type !== 'figure' && !axes(spec.type, columns).some(c => c.name === spec.x)) return false;
	if (singleMeasure(spec.type) && spec.y.length !== 1) return false;
	if ((radial(spec.type) || spec.type === 'scatter') && spec.y.includes(spec.x!)) return false;
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

export interface Slice { label: string; value: number }

/** Never imply that a partial result is a whole, or silently add measures across duplicate categories. */
export function parts(columns: QueryColumn[], rows: unknown[][], spec: ChartSpec, truncated: boolean): { problem: string } | { slices: Slice[]; total: number } {
	if (truncated) return { problem: 'The result was truncated. Group or narrow the query to chart the whole result.' };
	if (rows.length > 8) return { problem: 'Use bars for more than eight categories, or group the query into fewer slices.' };
	const xi = columns.findIndex(c => c.name === spec.x), yi = columns.findIndex(c => c.name === spec.y[0]);
	const slices: Slice[] = [];
	let total = 0;
	for (const row of rows) {
		const value = num(row[yi]);
		if (value === null || value < 0) return { problem: 'Each slice needs a non-negative number. Check missing or negative values in Results.' };
		const label = row[xi] == null ? '(none)' : typeof row[xi] === 'object' ? JSON.stringify(row[xi]) : String(row[xi]);
		if (slices.some(s => s.label === label)) return { problem: 'Group the query by category so each slice has one row.' };
		slices.push({ label, value });
		total += value;
	}
	return total > 0 && Number.isFinite(total) ? { slices, total } : { problem: 'A share of the total needs a finite, positive total.' };
}

export interface Category {
	label: string;
	values: (number | null)[];
}

/** Bars: one category per displayed row in the query's own order. */
export function categories(columns: QueryColumn[], rows: unknown[][], spec: ChartSpec): Category[] {
	const xi = columns.findIndex((c) => c.name === spec.x);
	const yi = spec.y.map((y) => columns.findIndex((c) => c.name === y));
	return rows.slice(0, MAX_BARS).map((r) => ({
		label: r[xi] === null || r[xi] === undefined ? '(none)' : typeof r[xi] === 'object' ? JSON.stringify(r[xi]) : String(r[xi]),
		values: yi.map((i) => num(r[i]))
	}));
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
