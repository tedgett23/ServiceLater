import { api, as, createTicket, details, expect, openTicket, test, uid } from './helpers';

// The SLA report and the hours each SLA counts. The server and database are shared: the report is narrowed to a group
// the test makes, and the rules it saves only touch records routed to its own groups (and are undone in finally).
test.describe('SLA reporting and hours', () => {
	test('the report shows how a team met its SLAs, as a chart or a table, and exports them', async ({ browser }) => {
		const admin = await as(browser, 'admin');
		const name = `Report desk ${uid()}`;
		const group = await api(admin.context, 'post', '/api/v1/groups', { name, description: '' });

		const { context, page } = await as(browser, 'agent');
		const t = await createTicket(context, { title: `Report me ${uid()}`, priority: 2 });
		await api(context, 'patch', `/api/v1/tickets/${t.id}`, { assignment_group_id: group.id });
		for (const to of ['in_progress', 'resolved']) await api(context, 'post', `/api/v1/tickets/${t.id}/transitions`, { to });

		await page.goto('/');
		await page.getByRole('link', { name: 'Reports' }).click();
		await expect(page.getByRole('heading', { name: 'SLA report', level: 1 })).toBeVisible();
		await page.getByRole('combobox', { name: 'Assignment group' }).selectOption({ label: name });
		await expect(page).toHaveURL(new RegExp(`group=${group.id}`));

		const summary = page.getByRole('region', { name: 'Summary' });
		await expect(summary).toContainText(/Met\s*100%/);
		await expect(summary).toContainText('of 2 clocks that ended');
		const groups = page.getByRole('region', { name: 'By assignment group' });
		await expect(groups).toContainText(name);
		await expect(groups).toContainText('2 met · 0 breached · 0 running');

		// The trend as a table: the same figures, with the counts behind them.
		const trend = page.getByRole('region', { name: /^Met, by/ });
		await trend.getByRole('button', { name: 'Table' }).click();
		await expect(trend.getByRole('columnheader', { name: 'Resolve met' })).toBeVisible();
		await expect(trend.getByRole('cell', { name: '100% (1 of 1)' }).first()).toBeVisible();

		const download = page.waitForEvent('download');
		await page.getByRole('button', { name: 'Export CSV' }).click();
		const file = await download;
		const csv = await (await file.createReadStream()).toArray();
		const text = Buffer.concat(csv).toString('utf8');
		expect(text).toContain(t.reference);
		expect(text).toContain('Assignment group');

		// End users have no reports.
		const lena = await as(browser, 'lena');
		expect((await lena.context.request.get('/api/v1/reports/slas')).status()).toBe(403);
	});

	test("an SLA counts a team's own hours, or every hour, by rules an admin sets", async ({ browser }) => {
		const { context, page } = await as(browser, 'admin');
		const office = await api(context, 'post', '/api/v1/groups', { name: `Long hours ${uid()}`, description: '' });
		const always = await api(context, 'post', '/api/v1/groups', { name: `Follow the sun ${uid()}`, description: '' });
		try {
			// The group's own hours, on its page.
			await page.goto(`/admin/groups/${office.id}`);
			await page.getByLabel('Working hours').selectOption({ label: 'Extended hours · Mon–Fri 07:00–19:00 (UTC)' });
			await page.getByRole('button', { name: 'Save' }).click();
			await page.reload(); // regressed: the group's hours were saved but the page read them back as none
			await expect(page.getByLabel('Working hours')).toHaveValue(/.+/);

			// Resolve: the follow-the-sun team counts every hour; any other team with hours of its own counts those.
			await page.goto('/admin/slas');
			const resolve = page.getByRole('region', { name: 'Resolve' });
			await resolve.getByRole('button', { name: '+ Add a rule' }).click();
			await resolve.getByRole('button', { name: '+ Add a rule' }).click();
			await resolve.getByRole('group', { name: 'Rule 1: how' }).getByRole('button', { name: 'Simple' }).click();
			await resolve.getByRole('combobox', { name: 'Rule 1: field 1' }).selectOption({ label: 'Assignment group' });
			await resolve.getByRole('combobox', { name: 'Rule 1: value 1' }).selectOption({ label: always.name });
			await expect(resolve.locator('code.cel')).toHaveText(`has(record.assignment_group) && record.assignment_group == "${always.name}"`);
			await resolve.getByRole('combobox', { name: 'Rule 1: hours' }).selectOption('24x7');
			await resolve.getByRole('button', { name: 'Save' }).click();
			await expect(resolve.getByText('Changed here')).toBeVisible();

			// Saved and read back: the condition is shown as the simple one it is.
			await page.reload();
			await expect(page.getByRole('region', { name: 'Resolve' }).getByRole('combobox', { name: 'Rule 1: value 1' })).toHaveValue(always.name);

			const agent = await as(browser, 'agent');
			for (const [g, want] of [
				[always, 'every hour counts'],
				[office, 'in Extended hours']
			] as const) {
				const t = await createTicket(agent.context, { title: `Hours ${uid()}`, priority: 3 });
				await api(agent.context, 'patch', `/api/v1/tickets/${t.id}`, { assignment_group_id: g.id });
				await openTicket(agent.page, t.reference);
				const clocks = details(agent.page).locator('.clocks');
				await expect(clocks.locator('li', { hasText: 'Resolve' })).toContainText(want);
				await expect(clocks.locator('li', { hasText: 'Respond' })).toContainText('in Business hours');
			}
		} finally {
			await context.request.delete('/api/v1/slas/incident-management/resolve/edits');
			await context.request.patch(`/api/v1/groups/${office.id}`, { data: { calendar_id: null } });
		}
	});
});
