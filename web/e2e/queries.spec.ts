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
