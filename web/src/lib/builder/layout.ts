import type { Step } from './workflow';
import { linkFields } from './workflow';

export const NODE_W = 170;
export const NODE_H = 62;
const COL_W = 230;
const ROW_H = 100;

/**
 * Left-to-right columns by distance from the start step, the way the engine walks the graph; steps not reachable yet
 * (just added) go in a column of their own after the rest. Used for steps that have no position of their own.
 */
export function autoLayout(steps: Step[], start: string): Map<string, { x: number; y: number }> {
	const ids = new Set(steps.map((s) => s.id));
	const level = new Map<string, number>();
	const root = ids.has(start) ? start : steps[0]?.id;
	if (root) level.set(root, 0);
	for (let changed = true, guard = 0; changed && guard <= steps.length; guard++) {
		changed = false;
		for (const s of steps) {
			const l = level.get(s.id);
			if (l === undefined) continue;
			for (const f of linkFields) {
				const to = s[f];
				if (to && ids.has(to) && (level.get(to) ?? -1) < l + 1 && l + 1 <= steps.length) {
					level.set(to, l + 1);
					changed = true;
				}
			}
		}
	}
	const deepest = Math.max(0, ...level.values());
	const columns = new Map<number, string[]>();
	for (const s of steps) {
		const l = level.get(s.id) ?? deepest + 1;
		columns.set(l, [...(columns.get(l) ?? []), s.id]);
	}
	const out = new Map<string, { x: number; y: number }>();
	for (const [l, col] of columns) col.forEach((id, i) => out.set(id, { x: l * COL_W, y: i * ROW_H }));
	return out;
}
