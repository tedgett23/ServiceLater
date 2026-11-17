import { as, createTicket, details, expect, openTicket, test, uid } from './helpers';
import { USERS } from './users';

// A request is fulfilled by tasks under it: staff add one by hand, it links back, the requester follows it, and the
// request page keeps count as tasks finish.
test('a request lists its tasks and keeps count as they finish', async ({ browser }) => {
	const id = uid();
	const { page, context } = await as(browser, 'agent');
	const lena = await as(browser, 'lena');
	const me = await (await lena.context.request.get('/api/v1/auth/me')).json();
	const req = await createTicket(context, { type: 'request', title: `New monitor ${id}`, requester_id: me.id });
	await openTicket(page, req.reference);
	const tasks = page.getByRole('region', { name: 'Tasks' });
	await expect(tasks).toContainText('No tasks yet');
	await tasks.getByRole('button', { name: 'Add a task' }).click();
	await tasks.getByLabel('Task').fill(`Order the monitor ${id}`);
	await tasks.getByRole('button', { name: 'Add', exact: true }).click();
	const row = tasks.locator('li', { hasText: `Order the monitor ${id}` });
	await expect(row).toContainText('Open');
	await expect(tasks).toContainText('1 of 1 still to do');

	// Lena sees it under her request too.
	await openTicket(lena.page, req.reference);
	await expect(lena.page.getByRole('region', { name: 'Tasks' }).locator('li', { hasText: `Order the monitor ${id}` })).toBeVisible();

	// The task says what it is part of, and finishing it shows on the request, live for Lena.
	await row.getByRole('link').click();
	await expect(page.getByText(`Part of ${req.reference}`)).toBeVisible();
	await details(page).getByRole('button', { name: 'Done' }).first().click();
	await page.getByRole('link', { name: req.reference }).click();
	await expect(page.getByRole('region', { name: 'Tasks' })).toContainText('All 1 finished');
	await expect(lena.page.getByRole('region', { name: 'Tasks' })).toContainText('All 1 finished');
});

// The manager is who approves what someone requests; an admin sets it on their page.
test('an admin sets someone’s manager', async ({ browser }) => {
	const { page } = await as(browser, 'admin');
	await page.goto('/admin/users');
	await page.getByRole('link', { name: USERS.marcus.name }).click();
	const section = page.getByRole('region', { name: 'Manager' });
	await section.getByLabel('Manager').fill(USERS.lena.name.split(' ')[0]);
	await section.getByRole('button', { name: new RegExp(USERS.lena.name) }).click();
	await expect(section.getByText(USERS.lena.name)).toBeVisible();
	await page.reload();
	await expect(page.getByRole('region', { name: 'Manager' }).getByText(USERS.lena.name)).toBeVisible();
	await page.getByRole('region', { name: 'Manager' }).getByRole('button', { name: 'Remove manager' }).click();
	await expect(page.getByRole('region', { name: 'Manager' }).getByLabel('Manager')).toBeVisible();
});
