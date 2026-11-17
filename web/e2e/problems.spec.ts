import { api, as, createTicket, details, expect, openTicket, test, uid } from './helpers';

// A problem raised from an incident starts out linked to it; resolving the problem resolves the incident with the fix,
// and the problem's page counts it, live.
test('a problem raised from an incident resolves it', async ({ browser }) => {
	const id = uid();
	const { page, context } = await as(browser, 'agent');
	const inc = await createTicket(context, { title: `Wi-Fi drops on floor 3 ${id}` });
	await openTicket(page, inc.reference);
	const problem = page.getByRole('region', { name: 'Problem', exact: true });
	await expect(problem).toContainText('Not linked to a problem');
	await problem.getByRole('button', { name: 'Raise a problem' }).click();
	await problem.getByLabel('Problem').fill(`Floor 3 access points reboot ${id}`);
	await problem.getByRole('button', { name: 'Raise' }).click();

	await expect(page.getByRole('heading', { level: 1, name: `Floor 3 access points reboot ${id}` })).toBeVisible();
	const incidents = page.getByRole('region', { name: 'Incidents', exact: true });
	await expect(incidents.locator('li', { hasText: inc.reference })).toBeVisible();
	await expect(incidents).toContainText('1 of 1 still open');
	const prbRef = new URL(page.url()).pathname.split('/').pop()!;

	await api(context, 'patch', `/api/v1/tickets/${prbRef}`, { fields: { root_cause: 'Firmware 4.1 leaks memory.', fix: 'Firmware 4.2.' } });
	for (const verb of ['Assess', 'Find the root cause', 'Start the fix', 'Resolve']) {
		await details(page).getByRole('button', { name: verb, exact: true }).click();
		await expect(details(page).getByRole('button', { name: verb, exact: true })).toHaveCount(0);
	}
	// The app resolves the incident in the background, and the problem's page hears of it.
	await expect(incidents).toContainText('All 1 resolved');

	await incidents.getByRole('link', { name: new RegExp(inc.reference) }).click();
	await expect(details(page)).toContainText('Resolved');
	await expect(page.locator('.activity li', { hasText: `Resolved with problem ${prbRef}` })).toBeVisible();
	await expect(page.getByRole('region', { name: 'Problem', exact: true }).getByRole('link', { name: new RegExp(prbRef) })).toBeVisible();
});

// An incident is linked to an open problem from its page, and unlinked again.
test('an incident is linked to a problem and unlinked', async ({ browser }) => {
	const id = uid();
	const { page, context } = await as(browser, 'agent');
	const prb = await createTicket(context, { type: 'problem', title: `Printers lose their queue ${id}` });
	const inc = await createTicket(context, { title: `Cannot print ${id}` });
	await openTicket(page, inc.reference);
	const problem = page.getByRole('region', { name: 'Problem', exact: true });
	await problem.getByRole('button', { name: 'Link to a problem' }).click();
	await problem.getByLabel('Open problem').selectOption({ label: `${prb.reference} ${prb.title}` });
	await problem.getByRole('button', { name: 'Link', exact: true }).click();
	await expect(problem.getByRole('link', { name: new RegExp(prb.reference) })).toBeVisible();

	await openTicket(page, prb.reference);
	await expect(page.getByRole('region', { name: 'Incidents', exact: true }).locator('li', { hasText: inc.reference })).toBeVisible();

	await openTicket(page, inc.reference);
	await page.getByRole('region', { name: 'Problem', exact: true }).getByRole('button', { name: 'Unlink' }).click();
	await expect(page.getByRole('region', { name: 'Problem', exact: true })).toContainText('Not linked to a problem');
});

// A change raised from a problem says which problem it fixes, and the problem lists it.
test('a change is raised from a problem', async ({ browser }) => {
	const id = uid();
	const { page, context } = await as(browser, 'agent');
	const prb = await createTicket(context, { type: 'problem', title: `Backups overrun ${id}` });
	await openTicket(page, prb.reference);
	const changes = page.getByRole('region', { name: 'Changes', exact: true });
	await changes.getByRole('button', { name: 'Raise a change' }).click();
	await expect(changes.getByLabel('Change')).toHaveValue(`Fix: Backups overrun ${id}`);
	await changes.getByRole('button', { name: 'Raise' }).click();
	await expect(page.getByRole('heading', { level: 1, name: `Fix: Backups overrun ${id}` })).toBeVisible();
	await expect(page.getByRole('region', { name: 'Problem it fixes' }).getByRole('link', { name: new RegExp(prb.reference) })).toBeVisible();
	await page.getByRole('region', { name: 'Problem it fixes' }).getByRole('link', { name: new RegExp(prb.reference) }).click();
	await expect(page.getByRole('region', { name: 'Changes', exact: true }).locator('li', { hasText: `Fix: Backups overrun ${id}` })).toBeVisible();
});

// The Problems page lists open problems, and the known errors with their workarounds for the service desk.
test('the problems page lists open problems and known errors', async ({ browser }) => {
	const id = uid();
	const { page, context } = await as(browser, 'agent');
	const known = await createTicket(context, { type: 'problem', title: `Outlook freezes ${id}` });
	await api(context, 'patch', `/api/v1/tickets/${known.id}`, { fields: { known_error: true, workaround: `Start it in safe mode ${id}` } });
	const inc = await createTicket(context, { title: `Outlook frozen again ${id}` });
	await api(context, 'patch', `/api/v1/tickets/${inc.id}`, { problem_id: known.id });
	await createTicket(context, { type: 'problem', title: `Slow logons ${id}` });

	await page.goto('/tickets');
	await page.getByRole('link', { name: 'Problems' }).click();
	await expect(page.getByRole('heading', { level: 1, name: 'Problems' })).toBeVisible();
	await page.getByRole('search').getByLabel('Title').fill(id);
	const list = page.getByRole('list', { name: 'Problems' });
	await expect(list.locator('li')).toHaveCount(2);
	await expect(list.locator('li', { hasText: `Outlook freezes ${id}` })).toContainText('1 open incident');

	await page.getByRole('button', { name: 'Known errors' }).click();
	await expect(page).toHaveURL(/show=known/);
	await expect(list.locator('li')).toHaveCount(1);
	await expect(list.locator('li')).toContainText(`Start it in safe mode ${id}`);
	await list.getByRole('link', { name: new RegExp(`Outlook freezes ${id}`) }).click();
	await expect(page.getByRole('heading', { level: 1, name: `Outlook freezes ${id}` })).toBeVisible();
});

// New problem on the Problems page opens the New ticket dialog set to a problem.
test('new problem opens the dialog on the problem type', async ({ browser }) => {
	const id = uid();
	const { page } = await as(browser, 'agent');
	await page.goto('/problems');
	await page.getByRole('button', { name: 'New problem' }).click();
	const dialog = page.getByRole('dialog');
	await dialog.getByLabel('Summary').fill(`Disk alerts flap ${id}`);
	await dialog.getByRole('button', { name: /^(Create|File)/ }).click();
	await expect(page.getByRole('heading', { level: 1, name: `Disk alerts flap ${id}` })).toBeVisible();
	await expect(page.locator('.stub')).toContainText('PRB');
});

// An incident suggests the open problems that look like it, and links to one in a click; a problem suggests the open
// incidents it may explain.
test('incidents and problems suggest each other', async ({ browser }) => {
	const id = uid();
	const { page, context } = await as(browser, 'agent');
	const prb = await createTicket(context, { type: 'problem', title: `Zebra label printers jam ${id}` });
	const inc = await createTicket(context, { title: `Label printer jammed ${id}` });
	const other = await createTicket(context, { title: `Zebra printer jams again ${id}` });

	await openTicket(page, inc.reference);
	const possible = page.getByRole('region', { name: 'Possible problems' });
	const row = possible.locator('li', { hasText: prb.reference });
	await expect(row).toContainText('Similar title');
	await row.getByRole('button', { name: 'Link' }).click();
	await expect(page.getByRole('region', { name: 'Problem', exact: true }).getByRole('link', { name: new RegExp(prb.reference) })).toBeVisible();
	await expect(page.getByRole('region', { name: 'Possible problems' })).toHaveCount(0);

	await openTicket(page, prb.reference);
	const related = page.getByRole('region', { name: 'Possibly related incidents' });
	await expect(related.locator('li', { hasText: inc.reference })).toHaveCount(0); // already linked
	await related.locator('li', { hasText: other.reference }).getByRole('button', { name: 'Link' }).click();
	await expect(page.getByRole('region', { name: 'Incidents', exact: true }).locator('li', { hasText: other.reference })).toBeVisible();
	await expect(page.getByRole('region', { name: 'Incidents', exact: true })).toContainText('2 of 2 still open');
});

// Filing an incident that looks like a known error shows its workaround, and can file it already linked.
test('a known error is suggested while filing an incident', async ({ browser }) => {
	const id = uid();
	const { page, context } = await as(browser, 'agent');
	const prb = await createTicket(context, { type: 'problem', title: `Teams camera goes black ${id}` });
	await api(context, 'patch', `/api/v1/tickets/${prb.id}`, { fields: { known_error: true, workaround: `Unplug the dock and plug it back ${id}` } });

	await page.goto('/tickets');
	await page.getByRole('button', { name: 'New ticket' }).first().click();
	const dialog = page.getByRole('dialog');
	await dialog.getByRole('radio', { name: 'Incident' }).check();
	await dialog.getByLabel('Summary').fill(`Camera black in Teams ${id}`);
	const known = dialog.getByRole('region', { name: 'Known problems' });
	const row = known.locator('li', { hasText: prb.reference });
	await expect(row).toContainText('Known error');
	await expect(row).toContainText(`Unplug the dock and plug it back ${id}`);
	await row.getByRole('button', { name: 'Link to it' }).click();
	await expect(row.getByRole('button', { name: 'Will be linked' })).toBeVisible();
	await dialog.getByRole('button', { name: 'Create ticket' }).click();
	await expect(page.getByRole('heading', { level: 1, name: `Camera black in Teams ${id}` })).toBeVisible();
	await expect(page.getByRole('region', { name: 'Problem', exact: true }).getByRole('link', { name: new RegExp(prb.reference) })).toBeVisible();
});
