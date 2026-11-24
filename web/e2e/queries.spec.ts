import { as, expect, test, uid } from './helpers';

// The SQL workbench (/queries, docs/query.md): run a query over the tables at the left, see where a refused one went
// wrong, find it again in History, and save a query as a view that other queries and the API read like a table.

test('an admin runs a query, sees where a refused one went wrong, and saves one as a shared view', async ({ browser }) => {
	const { page, context } = await as(browser, 'admin');
	const id = uid();
	const title = `Workbench ${id}`;
	const made = await (await context.request.post('/api/v1/tickets', { data: { title } })).json();
	const view = `wb_${id}`;
	try {
		await page.goto('/tickets');
		await page.getByRole('navigation').getByRole('link', { name: 'Queries' }).first().click();
		await expect(page.getByRole('heading', { level: 1, name: 'Queries' })).toBeVisible();
		// The tables are listed with their columns, and what a column holds is a click away.
		const tables = page.getByRole('region', { name: 'Tables' });
		await expect(tables.getByRole('button', { name: /^records/ })).toHaveAttribute('aria-expanded', 'true');
		await tables.getByRole('button', { name: /^state_category/ }).click();
		await expect(tables).toContainText('state_category: What the status means');

		const editor = page.getByRole('textbox', { name: 'SQL query' });
		await editor.click();
		await page.keyboard.press('ControlOrMeta+a');
		await page.keyboard.insertText(`SELECT reference, title, priority\nFROM records\nWHERE title = '${title}'`);
		await page.keyboard.press('ControlOrMeta+Enter');
		const results = page.getByRole('region', { name: 'Results' });
		await expect(results.getByRole('link', { name: made.reference })).toHaveAttribute('href', `/tickets/${made.reference}`);
		await expect(page.getByText(/^1 row · \d+ ms/)).toBeVisible();

		// A refused query says why and where, and marks the place.
		await editor.click();
		await page.keyboard.press('ControlOrMeta+a');
		await page.keyboard.insertText("SELECT title\nFROM records\nWHERE set_config('app.permissions', '', true) <> ''");
		await page.getByRole('button', { name: /^Run/ }).click();
		const alert = page.getByRole('alert');
		await expect(alert).toContainText('set_config() is not a function queries may call');
		await expect(alert).toContainText('Line 3, column 7');
		await expect(page.locator('.cm-lintRange-error')).toHaveCount(1);

		// History has both, newest first, and brings one back.
		await page.getByRole('group', { name: 'Show' }).getByRole('button', { name: 'History' }).click();
		const runs = page.getByRole('list', { name: 'Your recent queries' });
		await expect(runs.getByRole('listitem').first()).toContainText('Refused');
		await runs.getByRole('button', { name: new RegExp(`OK.*${title}`) }).first().click();
		await expect(editor).toContainText(`WHERE title = '${title}'`);

		// Saved as a shared view: listed at the left, and read through the API like a table.
		await page.getByRole('button', { name: 'Save as view' }).click();
		const dialog = page.getByRole('dialog', { name: 'Save as view' });
		await dialog.getByLabel('Name').fill(view);
		await dialog.getByLabel('Description').fill('The workbench test ticket');
		await dialog.getByLabel('Share with everyone who runs queries').check();
		await dialog.getByRole('button', { name: 'Save' }).click();
		await expect(dialog).toBeHidden();
		await expect(page.getByRole('status')).toContainText(`Saved as ${view}, shared`);
		await expect(page.getByText(`Editing view ${view}`)).toBeVisible();
		await expect(tables.getByRole('button', { name: new RegExp(`^${view}`) })).toBeVisible();
		const rows = await (await context.request.get(`/api/v1/views/${view}/rows`)).json();
		expect(rows.rows).toEqual([[made.reference, title, 3]]);

		// And a query reads it by name, offered by completion.
		await page.getByRole('button', { name: 'New query' }).click();
		await editor.click();
		await page.keyboard.press('ControlOrMeta+a');
		await page.keyboard.insertText(`SELECT count(*) AS n FROM ${view}`);
		await page.keyboard.press('ControlOrMeta+Enter');
		await expect(results.getByRole('cell', { name: '1', exact: true })).toBeVisible();
	} finally {
		await context.request.delete(`/api/v1/views/${view}`);
	}
});

test('someone who may not run queries has no workbench', async ({ browser }) => {
	const { page } = await as(browser, 'agent');
	await page.goto('/tickets');
	await expect(page.getByRole('link', { name: 'Reports' }).first()).toBeVisible();
	await expect(page.getByRole('link', { name: 'Queries' })).toHaveCount(0);
	await page.goto('/queries');
	await expect(page).not.toHaveURL(/\/queries/);
});

test('the workbench fits a phone screen', async ({ browser }) => {
	const { page } = await as(browser, 'admin', { viewport: { width: 390, height: 844 } });
	await page.goto('/queries');
	await expect(page.getByRole('textbox', { name: 'SQL query' })).toBeVisible();
	await page.getByRole('button', { name: /^Run/ }).click();
	await expect(page.getByRole('region', { name: 'Results' })).toBeVisible();
	const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
	expect(overflow).toBeLessThanOrEqual(0);
});

test('a quoted comment marker does not hide the next parameter', async ({ browser }) => {
	const { page } = await as(browser, 'admin');
	await page.goto('/queries');
	await page.getByRole('textbox', { name: 'SQL query' }).click();
	await page.keyboard.press('ControlOrMeta+a');
	await page.keyboard.insertText("SELECT '--' AS marker, $1::text AS supplied");
	await page.getByLabel('$1', { exact: true }).fill('works');
	await page.getByRole('button', { name: /^Run/ }).click();
	await expect(page.getByRole('cell', { name: 'works', exact: true })).toBeVisible();
});

test('shares, areas and scatter use the chosen columns and a saved doughnut reopens', async ({ browser }, info) => {
	const {page, context} = await as(browser, 'admin');
	const view = `shares_${uid()}`;
	try {
		await page.goto('/queries');
		await page.getByRole('textbox', {name: 'SQL query'}).click();
		await page.keyboard.press('ControlOrMeta+a');
		await page.keyboard.insertText("SELECT * FROM (VALUES ('Desk', 6, 3), ('Field', 2, 4)) AS v(team, tickets, hours)");
		await page.keyboard.press('ControlOrMeta+Enter');
		await page.getByRole('group', {name: 'Show'}).getByRole('button', {name: 'Chart'}).click();
		const kinds = page.getByRole('group', {name: 'Chart type'});
		await kinds.getByRole('button', {name: 'Area'}).click();
		await expect(page.locator('svg path[fill-opacity]')).toHaveCount(1);
		await kinds.getByRole('button', {name: 'Scatter'}).click();
		const plot = page.getByRole('group', {name: /^hours by tickets/});
		await plot.focus();
		await page.keyboard.press('End');
		await expect(plot.getByRole('status')).toContainText('3 hours');
		await kinds.getByRole('button', {name: 'Pie', exact: true}).click();
		await page.getByLabel('Slice by').selectOption('team');
		await page.getByRole('combobox', {name: 'Measure'}).selectOption('tickets');
		await expect(page.getByRole('figure', {name: 'tickets by team'})).toContainText('75%');
		await kinds.getByRole('button', {name: 'Doughnut'}).click();
		await page.getByRole('button', {name: 'Save as view'}).click();
		await page.getByRole('dialog').getByLabel('Name').fill(view);
		await page.getByRole('dialog').getByRole('button', {name: 'Save', exact: true}).click();
		await expect(page).toHaveURL(new RegExp(`view=${view}$`));
		await page.reload();
		await page.getByRole('button', {name: /^Run/}).click();
		const chart = page.getByRole('figure', {name: 'tickets by team'});
		await expect(chart.getByRole('img', {name: /Doughnut/})).toBeVisible();
		for (const [width, scheme] of [[1440,'light'],[1440,'dark'],[390,'light']] as const) {
			await page.setViewportSize({width, height: 900});
			await page.emulateMedia({colorScheme: scheme});
			await chart.scrollIntoViewIfNeeded();
			expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(0);
			await page.screenshot({path: info.outputPath(`doughnut-${width}-${scheme}.png`)});
		}
	} finally {
		await context.request.delete(`/api/v1/views/${view}`);
	}
});

test('a result is drawn as the chart that fits it, and a view keeps its chart', async ({ browser }) => {
	const { page, context } = await as(browser, 'admin');
	const id = uid();
	const view = `chart_${id}`;
	for (const [n, priority] of [[1, 1], [2, 2], [3, 2], [4, 3], [5, 3], [6, 3]] as const)
		await context.request.post('/api/v1/tickets', { data: { title: `Chart ${id} ${n}`, priority } });
	try {
		await page.goto('/queries');
		const editor = page.getByRole('textbox', { name: 'SQL query' });
		const type = async (sql: string) => {
			await editor.click();
			await page.keyboard.press('ControlOrMeta+a');
			await page.keyboard.insertText(sql);
			await page.keyboard.press('ControlOrMeta+Enter');
		};
		await type(`SELECT 'P' || priority AS priority, count(*) AS n\nFROM records WHERE title LIKE 'Chart ${id} %'\nGROUP BY 1 ORDER BY 1`);
		await expect(page.getByText(/^3 rows/)).toBeVisible();
		await page.getByRole('group', { name: 'Show' }).getByRole('button', { name: 'Chart' }).click();

		// Text and a number: bars, one per row, each readable on hover.
		const kinds = page.getByRole('group', { name: 'Chart type' });
		await expect(kinds.getByRole('button', { name: 'Bars' })).toHaveAttribute('aria-pressed', 'true');
		const bars = page.getByRole('group', { name: /^n by priority/ });
		await expect(bars.locator('.brow:not(.axis)')).toHaveCount(3);
		await bars.getByText('P3', { exact: true }).hover();
		await expect(bars.getByRole('status')).toContainText('3 n');

		// A single figure instead.
		await kinds.getByRole('button', { name: 'Figure' }).click();
		await expect(page.getByRole('figure', { name: 'n: 1' })).toBeVisible();
		await expect(page.getByText('From the first of 3 rows')).toBeVisible();

		// A time and a number: a line, read with the keyboard.
		await type(`SELECT date_trunc('day', created_at) AS day, count(*) AS n\nFROM records WHERE title LIKE 'Chart ${id} %'\nGROUP BY 1`);
		await expect(kinds.getByRole('button', { name: 'Line' })).toHaveAttribute('aria-pressed', 'true');
		const plot = page.getByRole('group', { name: /^n by day/ });
		await plot.focus();
		await page.keyboard.press('End');
		await expect(plot.getByRole('status')).toContainText('6 n');

		// Saved with its chart, and opened again on it.
		await page.getByRole('button', { name: 'Save as view' }).click();
		const dialog = page.getByRole('dialog', { name: 'Save as view' });
		await dialog.getByLabel('Name').fill(view);
		const keep = dialog.getByRole('checkbox', { name: /Keep the chart with it/ });
		await expect(keep).toBeChecked();
		await expect(dialog).toContainText('line of n');
		await dialog.getByRole('button', { name: 'Save' }).click();
		await expect(dialog).toBeHidden();
		const saved = await (await context.request.get(`/api/v1/views/${view}`)).json();
		expect(saved.chart).toEqual({ type: 'line', x: 'day', y: ['n'] });

		await page.goto(`/queries?view=${view}`);
		await expect(page.getByText(`Editing view ${view}`)).toBeVisible();
		await expect(page.getByRole('group', { name: 'Show' }).getByRole('button', { name: 'Chart' })).toHaveAttribute('aria-pressed', 'true');
		await page.getByRole('button', { name: /^Run/ }).click();
		await expect(page.getByRole('group', { name: /^n by day/ })).toBeVisible();
	} finally {
		await context.request.delete(`/api/v1/views/${view}`);
	}
});
