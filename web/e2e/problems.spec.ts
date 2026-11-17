import { api, as, createTicket, details, expect, openTicket, test, uid } from './helpers';

// A problem raised from an incident starts out linked to it; resolving the problem resolves the incident with the fix,
// and the problem's page counts it, live.
test('a problem raised from an incident resolves it', async ({ browser }) => {
	const id = uid();
	const { page, context } = await as(browser, 'agent');
	const inc = await createTicket(context, { title: `Wi-Fi drops on floor 3 ${id}` });
	await openTicket(page, inc.reference);
	const problem = page.getByRole('region', { name: 'Problem' });
	await expect(problem).toContainText('Not linked to a problem');
	await problem.getByRole('button', { name: 'Raise a problem' }).click();
	await problem.getByLabel('Problem').fill(`Floor 3 access points reboot ${id}`);
	await problem.getByRole('button', { name: 'Raise' }).click();

	await expect(page.getByRole('heading', { level: 1, name: `Floor 3 access points reboot ${id}` })).toBeVisible();
	const incidents = page.getByRole('region', { name: 'Incidents' });
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
	await expect(page.getByRole('region', { name: 'Problem' }).getByRole('link', { name: new RegExp(prbRef) })).toBeVisible();
});

// An incident is linked to an open problem from its page, and unlinked again.
test('an incident is linked to a problem and unlinked', async ({ browser }) => {
	const id = uid();
	const { page, context } = await as(browser, 'agent');
	const prb = await createTicket(context, { type: 'problem', title: `Printers lose their queue ${id}` });
	const inc = await createTicket(context, { title: `Cannot print ${id}` });
	await openTicket(page, inc.reference);
	const problem = page.getByRole('region', { name: 'Problem' });
	await problem.getByRole('button', { name: 'Link to a problem' }).click();
	await problem.getByLabel('Open problem').selectOption({ label: `${prb.reference} ${prb.title}` });
	await problem.getByRole('button', { name: 'Link', exact: true }).click();
	await expect(problem.getByRole('link', { name: new RegExp(prb.reference) })).toBeVisible();

	await openTicket(page, prb.reference);
	await expect(page.getByRole('region', { name: 'Incidents' }).locator('li', { hasText: inc.reference })).toBeVisible();

	await openTicket(page, inc.reference);
	await page.getByRole('region', { name: 'Problem' }).getByRole('button', { name: 'Unlink' }).click();
	await expect(page.getByRole('region', { name: 'Problem' })).toContainText('Not linked to a problem');
});
