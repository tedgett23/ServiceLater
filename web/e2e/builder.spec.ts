import { as, createTicket, expect, openTicket, test, uid } from './helpers';

// The workflow builder: an admin creates a workflow in incident-management, gets every problem with a broken save, saves it,
// and it runs; agents can look but not change; deleting it removes it. Edits made here live in the app's studio
// layer, so the test removes its workflow in `finally`.
test.describe('Workflow builder', () => {
	test('an admin builds a workflow that comments on a ticket, and it runs', async ({ browser }) => {
		const id = uid();
		const name = `greet-${id}`;
		const admin = await as(browser, 'admin');
		const { page } = admin;
		try {
			await page.goto('/workflows');
			const form = page.getByRole('form', { name: 'New workflow' });
			await form.getByLabel('App').selectOption('incident-management');
			await form.getByLabel('Name').fill(name);
			await form.getByLabel('For').selectOption('incident');
			await form.getByRole('button', { name: 'Create workflow' }).click();
			await expect(page.getByRole('heading', { level: 1 })).toHaveText(name);
			await expect(page.locator('.lede')).toContainText('not saved yet');
			// Its steps act as the app: the panel says what the app may do. (Built first in core-itsm, whose manifest reads no
			// records, the workflow failed when it ran; nothing on screen said why.)
			await expect(page.getByRole('complementary', { name: 'Workflow settings' })).toContainText('records:read:incident');

			// A comment step, joined to the end and made the start, from the panel.
			await page.getByLabel('Add a step').selectOption('comment.add');
			const panel = page.getByRole('complementary', { name: 'Step comment_add' });
			await panel.getByLabel('Comment').fill(`Thanks, we have it (${id}).`);
			await panel.getByLabel('Comment').blur();
			await panel.getByLabel('Next step').selectOption('done');
			await panel.getByRole('button', { name: 'Start here' }).click();
			await expect(page.locator('.svelte-flow__edge')).toHaveCount(1);

			// A broken step: every problem is listed, and nothing is saved.
			await page.getByRole('complementary').getByLabel('Step id').fill('Bad Id');
			await page.getByRole('complementary').getByLabel('Step id').blur();
			await page.getByRole('button', { name: 'Save', exact: true }).click();
			const problems = page.getByRole('alert').filter({ hasText: 'It was not saved' });
			await expect(problems).toContainText('step id "Bad Id"');
			await page.getByRole('complementary').getByLabel('Step id').fill('thank');
			await page.getByRole('complementary').getByLabel('Step id').blur();

			await page.getByRole('button', { name: 'Save', exact: true }).click();
			await expect(page.locator('.lede')).toContainText('Edited here');
			await expect(problems).toHaveCount(0);
			await page.locator('.yaml summary').click();
			await expect(page.locator('.yaml pre').first()).toContainText('type: comment.add');

			// It runs: started by hand on a ticket, it writes its comment.
			const t = await createTicket(admin.context, { title: `Builder run ${id}` });
			const started = await admin.context.request.post('/api/v1/workflow-runs', { data: { workflow: `incident-management/${name}`, ticket: t.reference } });
			expect(started.status()).toBe(201);
			await openTicket(page, t.number);
			await expect(page.locator('.activity')).toContainText(`Thanks, we have it (${id}).`);

			// Agents can look, not change.
			const agent = await as(browser, 'agent');
			await agent.page.goto(`/workflows/incident-management/${name}`);
			await expect(agent.page.getByRole('heading', { level: 1 })).toHaveText(name);
			await expect(agent.page.getByRole('button', { name: 'Save', exact: true })).toHaveCount(0);
			await expect(agent.page.getByText('changing it needs the workflows:edit permission')).toBeVisible();

			// Deleting it removes it.
			await page.goto(`/workflows/incident-management/${name}`);
			await page.getByRole('button', { name: 'Delete workflow' }).click();
			await page.getByRole('button', { name: 'Delete this workflow' }).click();
			await expect(page).toHaveURL(/\/workflows$/);
			await expect(page.getByRole('link', { name })).toHaveCount(0);
		} finally {
			await admin.context.request.delete(`/api/v1/workflows/incident-management/${name}/edits`);
		}
	});

	// An edit of a workflow the app ships is kept as only the change, so the app's later changes to the rest still
	// arrive; resetting takes it back to the app's version.
	test('editing a shipped workflow keeps only the change', async ({ browser }) => {
		const admin = await as(browser, 'admin');
		const { page } = admin;
		try {
			await page.goto('/workflows/change-management/cab-approval');
			await page.locator('.svelte-flow__node', { hasText: 'ask' }).first().click();
			const panel = page.getByRole('complementary', { name: 'Step ask' });
			await panel.getByLabel('Time limit').fill('6d');
			await panel.getByLabel('Time limit').blur();
			await page.getByRole('button', { name: 'Save', exact: true }).click();
			await expect(page.locator('.lede')).toContainText('Edited here');
			await page.locator('.yaml summary').click();
			const change = page.locator('.yaml pre').first();
			await expect(change).toContainText('timeout: 6d');
			await expect(change).not.toContainText('still_waiting');
			await expect(change).not.toContainText('override');

			await page.getByRole('button', { name: 'Reset to the app’s version' }).click();
			await page.getByRole('button', { name: "Go back to the app's version" }).click();
			await expect(page.locator('.lede')).not.toContainText('Edited here');
		} finally {
			await admin.context.request.delete('/api/v1/workflows/change-management/cab-approval/edits');
		}
	});

	// The builder shows where a workflow's runs are, and follows them live: here, runs waiting an hour at one step.
	test('the builder shows where runs are, live', async ({ browser }) => {
		const id = uid();
		const name = `hold-${id}`;
		const admin = await as(browser, 'admin');
		const path = `/api/v1/workflows/incident-management/${name}`;
		try {
			const saved = await admin.context.request.put(path, {
				data: {
					record_type: 'incident',
					start: 'pause',
					steps: [
						{ id: 'pause', type: 'wait', for: '1h', next: 'done' },
						{ id: 'done', type: 'end' }
					]
				}
			});
			expect(saved.ok()).toBeTruthy();
			const start = async (title: string) => {
				const t = await createTicket(admin.context, { title });
				const r = await admin.context.request.post('/api/v1/workflow-runs', { data: { workflow: `incident-management/${name}`, ticket: t.reference } });
				expect(r.status()).toBe(201);
				return t;
			};
			const first = await start(`Held ${id} one`);

			const { page } = admin;
			await page.goto(`/workflows/incident-management/${name}`);
			const pause = page.locator('.svelte-flow__node', { hasText: 'pause' });
			await expect(pause).toContainText('1 here now');
			await expect(pause.locator('.step')).toHaveClass(/live/);
			await expect(page.getByRole('complementary', { name: 'Workflow settings' })).toContainText(first.reference);

			// Another run starts while the page is open: the count follows without a reload.
			await start(`Held ${id} two`);
			await expect(pause).toContainText('2 here now');

			// Hidden on request.
			await page.getByLabel('Show where its runs are').uncheck();
			await expect(pause).not.toContainText('here now');
		} finally {
			await admin.context.request.delete(`${path}/edits`);
		}
	});
});
