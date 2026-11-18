import type { BusinessCalendar } from './types';

const dayShort = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];

/** A calendar's working week in a line: "Mon–Fri 09:00–17:00", or each day when they differ. */
export function week(c: Pick<BusinessCalendar, 'hours'>): string {
	const byDay = new Map<number, string>();
	for (const h of [...c.hours].sort((a, b) => a.start.localeCompare(b.start))) {
		byDay.set(h.day, [byDay.get(h.day), `${h.start}–${h.end}`].filter(Boolean).join(', '));
	}
	const order = [1, 2, 3, 4, 5, 6, 0].filter((d) => byDay.has(d));
	const runs: { from: number; to: number; hours: string }[] = [];
	for (const d of order) {
		const last = runs.at(-1);
		const prev = last ? order[order.indexOf(d) - 1] : -1;
		if (last && last.hours === byDay.get(d) && (prev + 1) % 7 === d) last.to = d;
		else runs.push({ from: d, to: d, hours: byDay.get(d)! });
	}
	return runs.map((r) => `${dayShort[r.from]}${r.to !== r.from ? `–${dayShort[r.to]}` : ''} ${r.hours}`).join('; ');
}

/** A calendar as a choice: its label, with its week and zone. */
export function calendarChoice(c: BusinessCalendar): string {
	return `${c.label} · ${week(c)} (${c.time_zone})`;
}
