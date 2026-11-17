import { expect, freshLogin, test } from './helpers';
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
