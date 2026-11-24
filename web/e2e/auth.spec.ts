import { as, createTicket, expect, freshLogin, row, test, uid } from './helpers';
import { PASSWORD, USERS } from './users';

// These tests sign in for themselves, so they start with no saved session.
test.use({ storageState: { cookies: [], origins: [] } });

test('a deep link goes to sign-in, then returns to the ticket', async ({ page }) => {
	await page.goto('/tickets/1');
	await expect(page).toHaveURL(/\/login\?next=%2Ftickets%2F1/);

	await page.getByLabel('Email').fill(USERS.agent.email);
	await page.getByLabel('Password').fill(PASSWORD);
	await page.getByRole('button', { name: 'Sign in' }).click();

	await expect(page).toHaveURL(/\/tickets\/1$/);
});

test('a wrong password explains what to do without saying which part was wrong', async ({ page }) => {
	await page.goto('/login');
	await page.getByLabel('Email').fill(USERS.agent.email);
	await page.getByLabel('Password').fill('not-the-password');
	await page.getByRole('button', { name: 'Sign in' }).click();

	await expect(page.getByRole('alert')).toContainText('don’t match');
	await expect(page).toHaveURL(/\/login/);
});

test('a next link cannot send you to another site', async ({ page }) => {
	await page.goto('/login?next=' + encodeURIComponent('//evil.example/phish'));
	await page.getByLabel('Email').fill(USERS.agent.email);
	await page.getByLabel('Password').fill(PASSWORD);
	await page.getByRole('button', { name: 'Sign in' }).click();

	await expect(page).toHaveURL(/\/tickets$/);
	expect(new URL(page.url()).hostname).not.toBe('evil.example');
});

test('signing out closes the app again', async ({ browser }) => {
	const { context, page } = await freshLogin(browser, 'agent');
	await page.goto('/tickets');
	await page.getByRole('button', { name: 'Sign out' }).click();
	await expect(page).toHaveURL(/\/login$/);

	await page.goto('/tickets');
	await expect(page).toHaveURL(/\/login\?next=/);
	await context.close();
});

// Regressed: the SPA reused its first user's queue and bell after switching accounts in the same tab.
test('switching accounts in one tab replaces the queue and notifications', async ({ browser }) => {
	const { context, page } = await freshLogin(browser, 'agent');
	const second = await as(browser, 'agent2');
	const admin = await as(browser, 'admin');
	const tickets = [];
	for (const who of [context, second.context]) {
		const me = await (await who.request.get('/api/v1/auth/me')).json();
		const ticket = await createTicket(admin.context, { title: `Account switch ${uid()}` });
		expect((await admin.context.request.patch(`/api/v1/tickets/${ticket.id}`, { data: { assignee_id: me.id } })).ok()).toBe(true);
		tickets.push(ticket);
	}
	await page.goto('/tickets');
	await expect(row(page, tickets[0].title)).toBeVisible();
	await page.locator('.bell summary').click();
	await expect(page.locator('.bell')).toContainText(tickets[0].title);
	await page.locator('.bell summary').click();
	const otherTab = await context.newPage();
	await otherTab.goto('/tickets');
	await expect(row(otherTab, tickets[0].title)).toBeVisible();
	await page.getByRole('button', { name: 'Sign out' }).click();
	await expect(otherTab).toHaveURL(/\/login/);
	await page.getByLabel('Email').fill(USERS.agent2.email);
	await page.getByLabel('Password').fill(PASSWORD);
	await page.getByRole('button', { name: 'Sign in' }).click();
	await expect(row(page, tickets[1].title)).toBeVisible();
	await expect(row(page, tickets[0].title)).toHaveCount(0);
	await page.locator('.bell summary').click();
	await expect(page.locator('.bell')).toContainText(tickets[1].title);
	await expect(page.locator('.bell')).not.toContainText(tickets[0].title);
});

test('an expired access token refreshes silently, with no sign-in screen', async ({ browser }) => {
	const { context, page } = await freshLogin(browser, 'agent');
	// Lose only the short-lived access cookie, as happens after 15 minutes.
	await context.clearCookies({ name: 'sl_access' });
	expect((await context.cookies()).some((c) => c.name === 'sl_access')).toBe(false);

	await page.goto('/tickets');
	await expect(page.getByRole('button', { name: 'New ticket' })).toBeVisible();
	await expect(page).toHaveURL(/\/tickets$/);
	expect((await context.cookies()).some((c) => c.name === 'sl_access')).toBe(true);
	await context.close();
});

test('losing the whole session sends you to sign-in', async ({ browser }) => {
	const { context, page } = await freshLogin(browser, 'agent');
	await context.clearCookies();
	await page.goto('/tickets');
	await expect(page).toHaveURL(/\/login\?next=/);
	await context.close();
});
