import { as, createTicket, details, expect, openTicket, row, test, uid } from './helpers';

// change-management is installed at startup: changes are a second record type, filed from the same dialog.
test.describe('changes', () => {
	test('an agent files a change and moves it through its own lifecycle', async ({ browser }) => {
		const { page } = await as(browser, 'agent');
		const title = `Patch the mail server ${uid()}`;

		await page.goto('/tickets');
		await page.getByRole('button', { name: 'New ticket' }).click();
		const dialog = page.getByRole('dialog', { name: 'New ticket' });
		// Regressed once: types came sorted by name, so the dialog started on Change instead of Incident.
		await expect(dialog.getByRole('radio', { name: 'Incident' })).toBeChecked();
		await dialog.getByText('Change', { exact: true }).click();
		await dialog.getByLabel('Summary').fill(title);
		await dialog.getByRole('button', { name: 'Create ticket' }).click();

		// Named by its own prefix, at its own address, in its own lifecycle's first state.
		await expect(page.getByRole('heading', { level: 1 })).toHaveText(title);
		await expect(page).toHaveURL(/\/tickets\/CHG\d+$/);
		await expect(page.locator('.stub')).toContainText('CHG');
		await expect(details(page).getByText('Draft')).toBeVisible();
		const panel = page.getByRole('region', { name: 'Change' });
		await panel.getByLabel('Implementation plan').fill('Apply the vendor patch.');
		await panel.getByLabel('Back-out plan').fill('Restore the snapshot.');
		await panel.getByLabel('Risk').focus(); // leaving the plan saves it
		await page.getByRole('button', { name: 'Submit' }).click();
		await expect(details(page).getByText('Assess')).toBeVisible();

		// The queue row says what it is too.
		await page.getByRole('button', { name: 'All', exact: true }).click();
		await expect(row(page, title)).toContainText('CHG');
		await expect(row(page, title)).toContainText('Assess');
	});

	test('an end user files incidents only, so the dialog offers no choice', async ({ browser }) => {
		const { page } = await as(browser, 'lena');
		await page.goto('/tickets');
		await page.getByRole('button', { name: 'New ticket' }).click();
		const dialog = page.getByRole('dialog', { name: 'New ticket' });
		await expect(dialog.getByLabel('Summary')).toBeVisible();
		await expect(dialog.getByRole('radio')).toHaveCount(0);
	});

	test('an incident and a change with the same number are different tickets', async ({ browser }) => {
		const { context, page } = await as(browser, 'agent');
		const change = await createTicket(context, { type: 'change', title: `Swap the core switch ${uid()}` });
		expect(change.reference).toBe(`CHG${change.number}`);
		await openTicket(page, change.reference);
		await expect(page.getByRole('heading', { level: 1 })).toHaveText(change.title);
		await expect(page).toHaveTitle(new RegExp(`^${change.reference} `));
	});

	test('the change panel saves its fields, and the requester reads them', async ({ browser }) => {
		const agent = await as(browser, 'agent');
		const lena = await as(browser, 'lena');
		const me = (await (await lena.context.request.get('/api/v1/auth/me')).json()) as { id: string };
		const c = await createTicket(agent.context, { type: 'change', title: `Rotate the TLS certs ${uid()}`, requester_id: me.id });

		await openTicket(agent.page, c.reference);
		const panel = agent.page.getByRole('region', { name: 'Change' });
		await panel.getByLabel('Risk').selectOption('high');
		await expect(agent.page.getByText('Risk set to High')).toBeAttached();
		await panel.getByLabel('Planned start').fill('2026-12-01T21:00');
		await panel.getByLabel('Planned end').fill('2026-12-01T23:30');
		await panel.getByLabel('Back-out plan').fill('Restore the old certificates from the vault.');
		await panel.getByLabel('Test plan').click(); // leaving the plan saves it
		await expect(agent.page.getByText('Back-out plan saved')).toBeAttached();

		await agent.page.reload();
		await expect(panel.getByLabel('Risk')).toHaveValue('high');
		await expect(panel.getByLabel('Planned end')).toHaveValue('2026-12-01T23:30');
		await expect(panel.getByLabel('Back-out plan')).toHaveValue('Restore the old certificates from the vault.');

		// A planned end before the start is refused, and the panel shows the server's reason.
		await panel.getByLabel('Planned end').fill('2026-11-30T23:30');
		await expect(details(agent.page).getByRole('alert')).toContainText('the planned end must be after the planned start');

		await openTicket(lena.page, c.reference);
		const theirs = lena.page.getByRole('region', { name: 'Change' });
		await expect(theirs).toContainText('High');
		await expect(theirs).toContainText('Restore the old certificates from the vault.');
		await expect(theirs.getByRole('combobox')).toHaveCount(0);
	});

	// The whole normal change process from the browser: the guards say what is missing, requesting authorization
	// asks the CAB (Tomás, granted cab-member for this test), and his approval moves the change to Scheduled on the
	// agent's screen. The grant is revoked whatever happens: later tests rely on Tomás holding no extra roles.
	test('a normal change is authorized by the change advisory board', async ({ browser }) => {
		const admin = await as(browser, 'admin');
		const agent = await as(browser, 'agent');
		const tomas = await as(browser, 'agent2');
		const tomasID = ((await (await tomas.context.request.get('/api/v1/auth/me')).json()) as { id: string }).id;
		const c = await createTicket(agent.context, { type: 'change', title: `Upgrade the firewall ${uid()}` });

		const grant = await admin.context.request.post('/api/v1/roles/cab-member/grants', { data: { user_id: tomasID } });
		expect(grant.status()).toBe(201);
		const grantID = ((await grant.json()) as { id: string }).id;
		try {
			await openTicket(agent.page, c.reference);
			const panel = agent.page.getByRole('region', { name: 'Change' });
			const meta = details(agent.page);

			// Draft: Submit says what it needs until the plans are written.
			await expect(meta.getByText('Write the implementation and back-out plans before submitting.')).toBeVisible();
			await panel.getByLabel('Implementation plan').fill('Load the new image on the standby, fail over.');
			await panel.getByLabel('Back-out plan').fill('Fail back to the primary.');
			await panel.getByLabel('Risk').focus(); // leaving the plan saves it
			await expect(meta.getByRole('button', { name: 'Submit' })).toBeEnabled();
			await meta.getByRole('button', { name: 'Submit' }).click();
			await expect(meta.getByText('Assess', { exact: true })).toBeVisible();

			// Assess: rate it and plan the window, then ask for authorization.
			await panel.getByLabel('Risk').selectOption('medium');
			await panel.getByLabel('Impact').selectOption('low');
			await panel.getByLabel('Planned start').fill('2026-01-10T22:00');
			await panel.getByLabel('Planned end').fill('2026-01-11T01:00');
			await meta.getByRole('button', { name: 'Request authorization' }).click();
			await expect(meta.getByText('Authorize', { exact: true })).toBeVisible();
			await expect(meta.getByRole('button', { name: 'Authorize' })).toHaveCount(0); // the board's move, not ours

			// Tomás is on the board: he is asked, and approves.
			await tomas.page.goto('/approvals');
			const ask = tomas.page.getByRole('list', { name: 'Waiting for your decision' }).locator('li', { hasText: c.reference });
			await expect(ask).toBeVisible({ timeout: 15_000 });
			await ask.getByRole('textbox').fill('Go ahead.');
			await ask.getByRole('button', { name: 'Approve' }).click();

			// The workflow moves it on, and the agent's open page follows.
			await expect(meta.getByText('Scheduled', { exact: true })).toBeVisible({ timeout: 15_000 });
			await expect(agent.page.locator('.activity')).toContainText('Authorized by the change advisory board. Go ahead.');
		} finally {
			await admin.context.request.delete(`/api/v1/roles/cab-member/grants/${grantID}`);
		}
	});

	test('the change calendar lists the changes of the week and flags overlapping windows', async ({ browser }) => {
		const { context, page } = await as(browser, 'agent');
		const tag = uid();
		// Tomorrow at noon and an hour later, in the browser's own time zone, so they land in this week's or next
		// week's view whatever day the suite runs on.
		const at = (hours: number) => {
			const d = new Date();
			d.setDate(d.getDate() + 1);
			d.setHours(12 + hours, 0, 0, 0);
			return d.toISOString();
		};
		const a = await createTicket(context, { type: 'change', title: `Calendar A ${tag}`, change: { planned_start: at(0), planned_end: at(2) } });
		const b = await createTicket(context, { type: 'change', title: `Calendar B ${tag}`, change: { planned_start: at(1), planned_end: at(3) } });

		const tomorrow = new Date();
		tomorrow.setDate(tomorrow.getDate() + 1);
		const day = `${tomorrow.getFullYear()}-${String(tomorrow.getMonth() + 1).padStart(2, '0')}-${String(tomorrow.getDate()).padStart(2, '0')}`;
		await page.goto(`/calendar?date=${day}`);
		const entryA = page.locator('li', { hasText: `Calendar A ${tag}` });
		await expect(entryA).toBeVisible();
		await expect(entryA).toContainText(`Overlaps ${b.reference}`);
		await expect(page.locator('li', { hasText: `Calendar B ${tag}` })).toContainText(`Overlaps ${a.reference}`);

		// Rescheduling one clears the conflict, live.
		await context.request.patch(`/api/v1/tickets/${b.id}`, { data: { change: { planned_start: at(4), planned_end: at(5) } } });
		await expect(entryA).not.toContainText('Overlaps');

		// The month view has them too, and the entry opens the change.
		await page.getByRole('button', { name: 'Month' }).click();
		await entryA.getByRole('link').click();
		await expect(page.getByRole('heading', { level: 1 })).toHaveText(`Calendar A ${tag}`);
	});

	test('an admin declares, renames and lifts a freeze on the calendar, and an agent sees it live', async ({ browser }) => {
		const agent = await as(browser, 'agent');
		const admin = await as(browser, 'admin');
		const tag = uid();
		// Freezes apply to every change, so this one is years ahead, on a day of its own, where no other test plans.
		const base = new Date(2031, 0, 6 + Math.floor(Math.random() * 300));
		const at = (days: number, hours = 0) => new Date(base.getFullYear(), base.getMonth(), base.getDate() + days, hours);
		const local = (d: Date) =>
			`${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}T${String(d.getHours()).padStart(2, '0')}:00`;
		const url = `/calendar?date=${local(at(2)).slice(0, 10)}`;
		// Planned before the freeze exists.
		const c = await createTicket(agent.context, {
			type: 'change',
			title: `Frozen change ${tag}`,
			change: { planned_start: at(2, 10).toISOString(), planned_end: at(2, 12).toISOString() }
		});
		let freezeID = '';
		try {
			await agent.page.goto(url);
			const entry = agent.page.locator('li', { hasText: `Frozen change ${tag}` });
			await expect(entry).toBeVisible();
			await expect(entry).not.toContainText('In ');
			// Agents read freezes but do not declare them.
			await expect(agent.page.getByRole('button', { name: 'Declare freeze' })).toHaveCount(0);

			await admin.page.goto(url);
			await admin.page.getByRole('button', { name: 'Declare freeze' }).click();
			const form = admin.page.getByRole('form', { name: 'Declare a change freeze' });
			await form.getByLabel('Name').fill(`Freeze ${tag}`);
			await form.getByLabel('Starts').fill(local(at(1)));
			await form.getByLabel('Ends').fill(local(at(3)));
			await form.getByLabel(/Reason/).fill('Quarter end.');
			const created = admin.page.waitForResponse((r) => r.url().endsWith('/api/v1/freeze-windows') && r.request().method() === 'POST');
			await form.getByRole('button', { name: 'Declare freeze' }).click();
			freezeID = ((await (await created).json()) as { id: string }).id;
			const listed = admin.page.locator('li.freeze', { hasText: `Freeze ${tag}` });
			await expect(listed).toContainText('Quarter end.');

			// The agent's open calendar follows: the freeze is listed, and the change it now covers says so.
			await expect(agent.page.locator('li.freeze', { hasText: `Freeze ${tag}` })).toBeVisible();
			await expect(entry).toContainText(`In Freeze ${tag}`);
			await expect(agent.page.locator('section.day', { has: entry }).getByRole('heading')).toContainText('Freeze');
			// A new window inside it is refused, by change-management's rule.
			const refused = await agent.context.request.patch(`/api/v1/tickets/${c.id}`, {
				data: { change: { planned_start: at(1, 9).toISOString(), planned_end: at(1, 10).toISOString() } }
			});
			expect(refused.status()).toBe(422);
			expect(await refused.text()).toContain('overlaps a change freeze');

			await listed.getByRole('button', { name: 'Edit' }).click();
			const edit = admin.page.getByRole('form', { name: `Change Freeze ${tag}` });
			await edit.getByLabel('Name').fill(`Renamed ${tag}`);
			await edit.getByRole('button', { name: 'Save freeze' }).click();
			await expect(entry).toContainText(`In Renamed ${tag}`);

			const renamed = admin.page.locator('li.freeze', { hasText: `Renamed ${tag}` });
			await renamed.getByRole('button', { name: 'Lift', exact: true }).click();
			await renamed.getByRole('button', { name: `Lift Renamed ${tag}` }).click();
			await expect(renamed).toHaveCount(0);
			await expect(entry).not.toContainText('In ');
			freezeID = '';
		} finally {
			if (freezeID) await admin.context.request.delete(`/api/v1/freeze-windows/${freezeID}`);
		}
	});

	test('people who cannot see changes have no calendar', async ({ browser }) => {
		const { page } = await as(browser, 'lena');
		await page.goto('/tickets');
		await expect(page.getByRole('link', { name: 'Calendar' })).toHaveCount(0);
		await page.goto('/calendar');
		await expect(page).toHaveURL(/\/tickets$/);
	});
});
