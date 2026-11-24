import assert from 'node:assert/strict';
import test from 'node:test';
import { paramCount } from '../src/lib/sqlParams.ts';
import { categories } from '../src/lib/charts/resultChart.ts';

test('the workbench offers inputs only for positional parameters outside literals and comments', () => {
	for (const [sql, want] of [
		["SELECT '--', $1", 1], ["SELECT 'it''s $8', $3, $1", 3], ['SELECT "$9", identifier$5', 0],
		['SELECT $tag$ $7 -- $tag$, $2 -- $8\n, $3', 3], ['SELECT /* /* $5 */ $6 */ $1', 1],
		[String.raw`SELECT E'it\'s $8', $1`, 1], ['SELECT $2, $$unfinished $6', 2], ['SELECT $99', 20]
	]) assert.equal(paramCount(sql), want, sql);
});

test('bar charts keep individual percentages and query order when the result exceeds 50 rows', () => {
	const rows = Array.from({ length: 52 }, (_, i) => [`team ${52-i}`, 75]);
	const shown = categories([{name: 'team', type: 'text'}, {name: 'rate', type: 'numeric'}], rows, {type: 'bars', x: 'team', y: ['rate']});
	assert.deepEqual(shown.map(c => [c.label, ...c.values]), rows.slice(0, 50));
});
