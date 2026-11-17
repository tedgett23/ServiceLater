import { as, details, expect, freshLogin, openTicket, test } from './helpers';
import { USERS } from './users';

// End users land on the portal: report a problem, or request something, and follow what they asked for.
test('an end user lands on the portal and only sees what they may order', async ({ browser }) => {
	const { page } = await freshLogin(browser, 'lena');
	await page.goto('/');
	await expect(page).toHaveURL(/\/catalog$/);
	await expect(page.getByRole('heading', { name: 'How can we help?' })).toBeVisible();
	await expect(page.getByRole('link', { name: /New laptop/ })).toBeVisible();
	await expect(page.getByRole('link', { name: /New starter/ })).toHaveCount(0); // staff and people managers only
	await page.getByPlaceholder(/Search the catalog/).fill('access');
	await expect(page.getByRole('link', { name: /Software access/ })).toBeVisible();
	await expect(page.getByRole('link', { name: /New laptop/ })).toHaveCount(0);
});

// Ordering asks the item's questions, as the server says for the answers so far, and files a request named after them,
// with its tasks under it; staff read the answers on the request.
test('ordering a laptop asks the right questions and starts the work', async ({ browser }) => {
	const lena = await as(browser, 'lena');
	const page = lena.page;
	await page.goto('/catalog');
	await page.getByRole('link', { name: /New laptop/ }).click();
	await expect(page.getByRole('heading', { name: 'New laptop' })).toBeVisible();
	await page.getByLabel('Model').selectOption('MacBook');
	await expect(page.getByLabel('Asset tag of the old laptop')).toHaveCount(0);
	await page.getByLabel('It replaces a laptop I have now').check();
	await expect(page.getByLabel('Asset tag of the old laptop')).toBeVisible(); // a condition, worked out by the server
	await page.getByLabel('What do you need it for?').fill('Design work');
	await expect(page.getByLabel('Asset tag of the old laptop')).toHaveAttribute('required', ''); // required while replacing
	await page.getByLabel('Asset tag of the old laptop').fill('LT-0042');
	await page.getByRole('button', { name: 'Send request' }).click();

	await expect(page).toHaveURL(/\/tickets\/REQ\d+$/);
	await expect(page.getByRole('heading', { level: 1 })).toHaveText('New laptop: MacBook');
	// Lena has no manager, so the workflow starts the work at once: the first task is for Procurement.
	await expect(page.getByRole('region', { name: 'Tasks' })).toContainText(`Order a MacBook laptop for ${USERS.lena.name}`);
	await expect(details(page).getByText('In progress')).toBeVisible();
	const ref = page.url().split('/').pop()!;

	await page.goto('/catalog');
	await expect(page.getByRole('complementary', { name: 'What you asked for' })).toContainText(ref);

	const agent = await as(browser, 'agent');
	await openTicket(agent.page, ref);
	// The answers, laid out by the item's questions; staff can correct them.
	await expect(agent.page.getByLabel('Asset tag of the old laptop')).toHaveValue('LT-0042');
});

// A manager approves what their people request: Marcus orders software access, Lena (his manager) approves it from
// her approvals, and the work starts.
test('a manager approves a request from their approvals', async ({ browser }) => {
	const admin = await as(browser, 'admin');
	const people = (await (await admin.context.request.get('/api/v1/users?limit=50')).json()).items as { id: string; email: string }[];
	const marcus = people.find((p) => p.email === USERS.marcus.email)!;
	const lenaID = people.find((p) => p.email === USERS.lena.email)!.id;
	await admin.context.request.patch(`/api/v1/users/${marcus.id}`, { data: { manager_id: lenaID } });
	try {
		const m = await as(browser, 'marcus');
		await m.page.goto('/catalog/software-access');
		await m.page.getByLabel('Application').fill('Figma');
		await m.page.getByLabel('What do you need it for?').fill('Mock-ups');
		await m.page.getByRole('button', { name: 'Send request' }).click();
		await expect(m.page).toHaveURL(/\/tickets\/REQ\d+$/);
		// Regressed once: the workflow moved the request the moment it was filed, before the page listened for
		// changes, and the page kept showing Submitted.
		await expect(details(m.page).getByText('Awaiting approval')).toBeVisible();

		const lena = await as(browser, 'lena');
		await lena.page.goto('/approvals');
		const row = lena.page.locator('li', { hasText: 'Access to Figma (standard)' });
		await row.getByRole('button', { name: 'Approve' }).click();
		await expect(details(m.page).getByText('In progress')).toBeVisible(); // live, on Marcus's page
		await expect(m.page.getByRole('region', { name: 'Tasks' })).toContainText(`Give ${USERS.marcus.name} standard access to Figma`);
	} finally {
		await admin.context.request.patch(`/api/v1/users/${marcus.id}`, { data: { manager_id: null } });
	}
});
