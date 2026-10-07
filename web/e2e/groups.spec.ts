import { expect, test, type Page } from '@playwright/test';
import { as, createTicket, details, openTicket, row, uid } from './helpers';
import { USERS } from './users';

const assignee = (page: Page) => details(page).getByLabel('Assignee');
const group = (page: Page) => details(page).getByLabel('Assignment group');

test('an admin builds a team, an agent routes a ticket to it, and only its members can take it', async ({ browser }) => {
	const tag = uid();
	const admin = await as(browser, 'admin');
	const agent = await as(browser, 'agent');
	const tomas = await as(browser, 'agent2');
	const lena = await as(browser, 'lena');

	// The admin makes the group and puts Tomás in it, from the admin section's Groups page.
	await admin.page.goto('/tickets');
	await admin.page.getByRole('link', { name: 'Admin' }).first().click();
	await admin.page.getByRole('navigation', { name: 'Admin' }).getByRole('link', { name: 'Groups' }).click();
	await expect(admin.page.getByRole('heading', { name: 'Groups', level: 1 })).toBeVisible(); // the users page has a Name field too
	await admin.page.getByLabel('Name').fill(`Network ${tag}`);
	await admin.page.getByLabel('Description').fill('Routers and links');
	await admin.page.getByRole('button', { name: 'Create group' }).click();
	await expect(admin.page.getByRole('heading', { name: `Network ${tag}` })).toBeVisible();
	await admin.page.getByLabel('Add someone').fill('Tomás');
	await admin.page.getByRole('button', { name: `Add ${USERS.agent2.name}` }).click();
	await expect(admin.page.locator('.members li', { hasText: USERS.agent2.name })).toBeVisible();
	// A second team without him, to hand the ticket on to.
	const other = await admin.context.request.post('/api/v1/groups', { data: { name: `Desk ${tag}` } });
	expect(other.status()).toBe(201);

	const t = await createTicket(lena.context, { title: `Link flapping ${tag}` });
	await openTicket(agent.page, t.number);
	await group(agent.page).selectOption({ label: `Network ${tag}` });
	await expect(agent.page.getByText(`Routed to Network ${tag}`)).toBeAttached();

	// Only members are offered, so Priya (not in the group) cannot take it, and Tomás can be picked.
	await expect(assignee(agent.page).locator('option')).toHaveText(['Unassigned', USERS.agent2.name]);
	await expect(details(agent.page).getByRole('button', { name: 'Assign to me' })).toHaveCount(0);
	await assignee(agent.page).selectOption({ label: USERS.agent2.name });
	await expect(assignee(agent.page).locator('option:checked')).toHaveText(USERS.agent2.name);

	// It stuck, and Tomás finds it under his groups; Priya does not.
	await agent.page.reload();
	await expect(group(agent.page).locator('option:checked')).toHaveText(`Network ${tag}`);
	await tomas.page.goto('/tickets');
	await tomas.page.getByRole('button', { name: 'My groups' }).click();
	await expect(row(tomas.page, t.title)).toContainText(`Network ${tag}`);
	await agent.page.goto('/tickets');
	await agent.page.getByRole('button', { name: 'My groups' }).click();
	await expect(row(agent.page, t.title)).toHaveCount(0);

	// The requester sees which team has it, but cannot change it.
	await openTicket(lena.page, t.number);
	await expect(details(lena.page)).toContainText(`Network ${tag}`);
	await expect(group(lena.page)).toHaveCount(0);

	// Handing it to a team Tomás is not in lets go of him.
	await openTicket(agent.page, t.number);
	await group(agent.page).selectOption({ label: `Desk ${tag}` });
	await expect(assignee(agent.page).locator('option:checked')).toHaveText('Unassigned');
	await agent.page.reload();
	await expect(assignee(agent.page).locator('option:checked')).toHaveText('Unassigned');
});

test('a group with tickets cannot be deleted, and says why', async ({ browser }) => {
	const tag = uid();
	const admin = await as(browser, 'admin');
	const g = await (await admin.context.request.post('/api/v1/groups', { data: { name: `Busy ${tag}` } })).json();
	const t = await createTicket(admin.context, { title: `Held ${tag}` });
	expect((await admin.context.request.patch(`/api/v1/tickets/${t.id}`, { data: { assignment_group_id: g.id } })).ok()).toBeTruthy();

	await admin.page.goto(`/admin/groups/${g.id}`);
	await admin.page.getByRole('button', { name: 'Delete group' }).click();
	await admin.page.getByRole('button', { name: `Delete Busy ${tag}` }).click();
	await expect(admin.page.getByRole('alert')).toContainText('move them to another group first');
	await expect(admin.page).toHaveURL(new RegExp(`/admin/groups/${g.id}$`));
});

test('only admins reach the admin section', async ({ browser }) => {
	const admin = await as(browser, 'admin');
	const g = await (await admin.context.request.post('/api/v1/groups', { data: { name: `Look ${uid()}` } })).json();
	for (const who of ['agent', 'lena'] as const) {
		const { page } = await as(browser, who);
		await page.goto('/tickets');
		await expect(page.getByRole('link', { name: 'Admin' })).toHaveCount(0);
		for (const path of ['/admin/users', `/admin/groups/${g.id}`]) {
			await page.goto(path);
			await expect(page, `${who} at ${path}`).toHaveURL(/\/tickets$/);
		}
	}
});

test('the API reference is public, generated from the server, and links straight to an endpoint', async ({ browser }) => {
	const context = await browser.newContext(); // signed out
	const page = await context.newPage();
	const spec = await context.request.get('/api/v1/openapi.json');
	expect(spec.ok()).toBeTruthy();
	expect((await spec.json()).openapi).toBe('3.1.0');

	await page.goto('/docs/api#addGroupMember');
	const heading = page.getByRole('heading', { name: 'Add group member' });
	await expect(heading).toBeInViewport();
	const op = page.locator('article#addGroupMember');
	await expect(op).toContainText('/api/v1/groups/{id}/members/{userId}');
	await expect(op).toContainText('Signed in as admin');

	await page.getByPlaceholder('Filter endpoints').fill('members');
	await expect(page.locator('article#listGroupMembers')).toBeVisible();
	await expect(page.locator('article#listTickets')).toHaveCount(0);
	await context.close();
});

test('on a phone the admin header and the groups pages fit', async ({ browser }) => {
	const admin = await as(browser, 'admin', { viewport: { width: 390, height: 844 } });
	const g = await (await admin.context.request.post('/api/v1/groups', { data: { name: `Narrow ${uid()}` } })).json();
	for (const path of ['/tickets', '/admin/users', '/admin/groups', `/admin/groups/${g.id}`, '/docs/api']) {
		await admin.page.goto(path);
		await admin.page.waitForLoadState('networkidle');
		const overflow = await admin.page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
		expect(overflow, path).toBeLessThanOrEqual(0);
	}
	// The queue filters stay on one line each (regression: "My groups" wrapped onto two at 390px).
	await admin.page.goto('/tickets');
	const box = await admin.page.getByRole('button', { name: 'My groups' }).boundingBox();
	expect(box!.height).toBeLessThan(40);
});
