import { as, createTicket, details, expect, openTicket, test, uid } from './helpers';

// A script step in the workflow builder: written in CodeMirror, its mistakes marked as it is typed, tried against a
// ticket (which says what it would do and changes nothing), saved, and run. The workflow is removed in finally.
test('an admin writes a script step, tries it on a ticket, and it runs', async ({ browser }) => {
	const id = uid();
	const name = `raise-${id}`;
	const admin = await as(browser, 'admin');
	const { page } = admin;
	try {
		// change-management may edit changes (its manifest says so), so its workflows' scripts may too.
		const t = await createTicket(admin.context, { type: 'change', title: `Script target ${id}`, priority: 3 });
		await page.goto('/workflows');
		const form = page.getByRole('form', { name: 'New workflow' });
		await form.getByLabel('App').selectOption('change-management');
		await form.getByLabel('Name').fill(name);
		await form.getByLabel('For').selectOption('change');
		await form.getByRole('button', { name: 'Create workflow' }).click();
		await expect(page.getByRole('heading', { level: 1 })).toHaveText(name);

		await page.getByLabel('Add a step').selectOption('script');
		const panel = page.getByRole('complementary', { name: 'Step script' });
		const editor = panel.getByRole('textbox', { name: 'Script of script' });
		await expect(editor).toBeVisible();

		// A mistake is marked as it is typed.
		await editor.click();
		await page.keyboard.press('ControlOrMeta+a');
		await page.keyboard.insertText('upate(record, {"priority": 1})');
		await expect(panel.getByRole('alert')).toContainText('Line 1: undefined: upate');

		await page.keyboard.press('ControlOrMeta+a');
		await page.keyboard.insertText(`if record.priority > 1:\n    update(record, {"priority": 1}, comment="Raised by a script (${id})")\nprint("was", record.priority)`);
		await expect(panel.getByRole('alert')).toHaveCount(0);
		await panel.getByLabel('Next step').selectOption('done');
		await panel.getByRole('button', { name: 'Start here' }).click();

		// Tried on the ticket: what it would do, and nothing changed.
		const trial = panel.getByRole('group', { name: 'Test run' });
		await trial.getByLabel('Record to test against').fill(t.reference);
		await trial.getByRole('button', { name: 'Run' }).click();
		await expect(trial.getByRole('status')).toContainText('It would succeed');
		await expect(trial.getByRole('status')).toContainText(`${t.reference}`);
		await expect(trial.getByRole('status')).toContainText('priority = 1');
		await expect(trial.getByRole('status')).toContainText('was 3');

		await page.getByRole('button', { name: 'Save', exact: true }).click();
		await expect(page.locator('.lede')).toContainText('Edited here');

		// It runs: the ticket is raised, with the script's comment.
		const started = await admin.context.request.post('/api/v1/workflow-runs', { data: { workflow: `change-management/${name}`, ticket: t.reference } });
		expect(started.status()).toBe(201);
		await openTicket(page, t.reference);
		await expect(page.locator('.activity')).toContainText(`Raised by a script (${id})`);
		await expect(details(page).getByLabel('Priority')).toHaveValue('1');

		// An agent sees the script but cannot change it.
		const agent = await as(browser, 'agent');
		await agent.page.goto(`/workflows/change-management/${name}`);
		await agent.page.locator('.svelte-flow__node', { hasText: 'script' }).click();
		await expect(agent.page.getByRole('textbox', { name: 'Script of script' })).toHaveAttribute('contenteditable', 'false');
	} finally {
		await admin.context.request.delete(`/api/v1/workflows/change-management/${name}/edits`);
	}
});

// A script file holds a function many scripts can load: created and saved from the Workflows page, loaded by a script
// step, and run; its page says what loads it. Both are removed in finally.
test('a script file is written once and loaded by a workflow', async ({ browser }) => {
	const id = uid();
	const file = `helpers_${id}.star`;
	const name = `raise-${id}`;
	const admin = await as(browser, 'admin');
	const { page } = admin;
	try {
		await page.goto('/workflows');
		const form = page.getByRole('form', { name: 'New script file' });
		await form.getByLabel('App').selectOption('change-management');
		await form.getByLabel('File name').fill(`helpers_${id}`);
		await form.getByRole('button', { name: 'Create script file' }).click();
		await expect(page.getByRole('heading', { level: 1 })).toHaveText(file);
		const editor = page.getByRole('textbox', { name: `Script file ${file}` });
		await editor.click();
		await page.keyboard.press('ControlOrMeta+a');
		await page.keyboard.insertText('def raise_it(record, note):\n    update(record, {"priority": 1}, comment=note)\n');
		await page.getByRole('button', { name: 'Save', exact: true }).click();
		await expect(page.locator('.lede')).toContainText('Added here');

		// A workflow's script step loads it; saving checks the load.
		const flow = {
			record_type: 'change',
			start: 's',
			steps: [
				{ id: 's', type: 'script', script: `load("${file}", "raise_it")\nraise_it(record, "Raised by a shared function (${id})")`, next: 'done' },
				{ id: 'done', type: 'end' }
			]
		};
		expect((await admin.context.request.put(`/api/v1/workflows/change-management/${name}`, { data: flow })).status()).toBe(200);
		const t = await createTicket(admin.context, { type: 'change', title: `Shared ${id}`, priority: 3 });
		expect((await admin.context.request.post('/api/v1/workflow-runs', { data: { workflow: `change-management/${name}`, ticket: t.reference } })).status()).toBe(201);
		await openTicket(page, t.reference);
		await expect(page.locator('.activity')).toContainText(`Raised by a shared function (${id})`);

		// Opened directly (a reload, a shared link): the address has no .star, which the server would take for a file
		// to send and answer 404 (it did).
		await page.goto(`/workflows/change-management/scripts/helpers_${id}`);
		await expect(page.getByRole('region', { name: 'Loaded by' })).toContainText(`workflow ${name}`);
	} finally {
		await admin.context.request.delete(`/api/v1/workflows/change-management/${name}/edits`);
		await admin.context.request.delete(`/api/v1/scripts/change-management/${file}/edits`);
	}
});
