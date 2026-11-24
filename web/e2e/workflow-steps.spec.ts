import { as, createTicket, expect, openTicket, test, uid } from './helpers';

// The new workflow steps in the builder: a For each step is added and filled in from the step form, runs a child
// workflow per item, and the run viewer lists the runs it started. The workflows are removed in finally.
test('a for each step runs a workflow per item, and the viewer shows the runs it started', async ({ browser }) => {
	const id = uid();
	const child = `note-${id}`;
	const parent = `fan-${id}`;
	const admin = await as(browser, 'admin');
	const { page } = admin;
	try {
		// The child: comments on the change with its item.
		const childFlow = {
			record_type: 'change',
			start: 'say',
			steps: [
				{ id: 'say', type: 'comment.add', body: { expr: `"item " + string(vars.item) + " (${id})"` }, next: 'done' },
				{ id: 'done', type: 'end' }
			]
		};
		expect((await admin.context.request.put(`/api/v1/workflows/change-management/${child}`, { data: childFlow })).status()).toBe(200);

		// The parent, built in the builder.
		await page.goto('/workflows');
		const form = page.getByRole('form', { name: 'New workflow' });
		await form.getByLabel('For').selectOption('change');
		await form.getByLabel('App').selectOption('change-management');
		await form.getByLabel('Name').fill(parent);
		await form.getByRole('button', { name: 'Create workflow' }).click();
		await expect(page.getByRole('heading', { level: 1 })).toHaveText(parent);
		await page.getByLabel('Add a step').selectOption('foreach');
		const panel = page.getByRole('complementary', { name: 'Step foreach' });
		await panel.getByLabel('Workflow to run').selectOption(child);
		await panel.getByLabel('For each item of').fill('[1, 2]');
		await panel.getByLabel('For each item of').blur();
		await panel.getByLabel('At once').fill('2');
		await panel.getByLabel('At once').blur();
		await panel.getByLabel('Next step').selectOption('done');
		await panel.getByRole('button', { name: 'Start here' }).click();
		await page.getByRole('button', { name: 'Save', exact: true }).click();
		await expect(page.locator('.lede')).toContainText('Edited here');

		const t = await createTicket(admin.context, { type: 'change', title: `Fan out ${id}` });
		const started = await admin.context.request.post('/api/v1/workflow-runs', { data: { workflow: `change-management/${parent}`, ticket: t.reference } });
		expect(started.status()).toBe(201);
		const run = (await started.json()) as { id: string };
		await openTicket(page, t.reference);
		await expect(page.locator('.activity')).toContainText(`item 1 (${id})`);
		await expect(page.locator('.activity')).toContainText(`item 2 (${id})`);

		await page.goto(`/runs/${run.id}`);
		// Regressed: navigating from a finished parent reused its page; its stopped poll never loaded the child.
		await expect(page.locator('header .meta')).toContainText('Finished');
		const children = page.getByRole('region', { name: 'Runs it started' });
		await expect(children.getByRole('link', { name: child })).toHaveCount(2);
		await children.getByRole('link', { name: child }).first().click();
		await expect(page.getByText('Started by a step of')).toBeVisible();
	} finally {
		await admin.context.request.delete(`/api/v1/workflows/change-management/${parent}/edits`);
		await admin.context.request.delete(`/api/v1/workflows/change-management/${child}/edits`);
	}
});
