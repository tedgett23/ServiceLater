import { type Browser } from '@playwright/test';
import { as, createTicket, expect, test, uid } from './helpers';
import { USERS } from './users';

/** Signs in as someone created during a test, in a context of its own that the caller closes. */
async function signIn(browser: Browser, email: string, password: string) {
	const context = await browser.newContext();
	const res = await context.request.post('/api/v1/auth/login', { data: { email, password } });
	return { context, status: res.status() };
}

test('an admin adds someone, edits them, puts them in a group and resets their password', async ({ browser }) => {
	const tag = uid();
	const admin = await as(browser, 'admin');
	expect((await admin.context.request.post('/api/v1/groups', { data: { name: `Field ${tag}` } })).status()).toBe(201);
	const email = `nia-${tag}@e2e.test`;

	await admin.page.goto('/admin/users');
	const form = admin.page.locator('form.create');
	await form.getByLabel('Name').fill(`Nia ${tag}`);
	await form.getByLabel('Email').fill(email);
	await form.getByLabel('Role').selectOption('agent');
	await form.getByLabel('Password').fill('first-password');
	await form.getByRole('button', { name: 'Add user' }).click();
	await expect(admin.page.getByRole('heading', { name: `Nia ${tag}` })).toBeVisible();
	await expect(admin.page.locator('.lede')).toHaveText('Agent');

	const first = await signIn(browser, email, 'first-password');
	expect(first.status, 'the new person signs in').toBe(200);
	await first.context.close();

	// Rename her, and add her to a group from her own page; the group's page agrees.
	await admin.page.getByLabel('Name').fill(`Nia Obi ${tag}`);
	await admin.page.getByRole('button', { name: 'Save' }).click();
	await expect(admin.page.getByRole('heading', { name: `Nia Obi ${tag}` })).toBeVisible();
	await admin.page.getByLabel('Add to a group').selectOption({ label: `Field ${tag}` });
	await admin.page.getByRole('button', { name: 'Add', exact: true }).click();
	await expect(admin.page.locator('.groups li', { hasText: `Field ${tag}` })).toBeVisible();
	await admin.page.getByRole('link', { name: `Field ${tag}` }).click();
	await expect(admin.page.locator('.members li', { hasText: `Nia Obi ${tag}` })).toBeVisible();

	// Back on her page, a new password replaces the old one.
	await admin.page.getByRole('link', { name: `Nia Obi ${tag}` }).click();
	await admin.page.getByLabel('New password').fill('second-password');
	await admin.page.getByRole('button', { name: 'Set password' }).click();
	await expect(admin.page.getByLabel('New password')).toHaveValue('');
	const old = await signIn(browser, email, 'first-password');
	const now = await signIn(browser, email, 'second-password');
	expect([old.status, now.status]).toEqual([401, 200]);
	await old.context.close();
	await now.context.close();

	// She shows up in the list, found by search.
	await admin.page.goto('/admin/users');
	await admin.page.getByPlaceholder('Search by name or email').fill(`nia-${tag}`);
	await expect(admin.page.locator('.people li')).toHaveCount(1);
	await expect(admin.page.locator('.people li')).toContainText('Agent');
});

test('deactivating someone locks them out until they are reactivated, and someone with history cannot be deleted', async ({ browser }) => {
	const tag = uid();
	const admin = await as(browser, 'admin');
	const email = `sam-${tag}@e2e.test`;
	const sam = await (
		await admin.context.request.post('/api/v1/users', { data: { email, name: `Sam ${tag}`, password: 'sam-password' } })
	).json();
	const own = await signIn(browser, email, 'sam-password');
	await createTicket(own.context, { title: `Sam's laptop ${tag}` });

	await admin.page.goto(`/admin/users/${sam.id}`);
	await admin.page.getByRole('button', { name: 'Deactivate' }).click();
	await expect(admin.page.locator('.lede')).toContainText('deactivated');
	expect((await signIn(browser, email, 'sam-password')).status, 'signing in deactivated').toBe(401);
	expect((await own.context.request.post('/api/v1/auth/refresh')).status(), 'their session ends').toBe(401);
	await own.context.close();

	// Hidden from the list unless asked for.
	await admin.page.goto('/admin/users');
	await admin.page.getByPlaceholder('Search by name or email').fill(`sam-${tag}`);
	await expect(admin.page.getByText('Nobody matches.')).toBeVisible();
	await admin.page.getByLabel('Show deactivated').check();
	await expect(admin.page.locator('.people li')).toContainText('Deactivated');

	// They filed a ticket, so deleting is refused with what to do instead.
	await admin.page.locator('.people a', { hasText: `Sam ${tag}` }).click();
	await admin.page.getByRole('button', { name: 'Delete user' }).click();
	await admin.page.getByRole('button', { name: `Delete Sam ${tag}` }).click();
	await expect(admin.page.getByRole('alert')).toContainText('deactivate them instead');

	await admin.page.getByRole('button', { name: 'Reactivate' }).click();
	await expect(admin.page.locator('.lede')).not.toContainText('deactivated');
	const back = await signIn(browser, email, 'sam-password');
	expect(back.status, 'signing in again').toBe(200);
	await back.context.close();
});

test('someone with no history can be deleted', async ({ browser }) => {
	const tag = uid();
	const admin = await as(browser, 'admin');
	const p = await (
		await admin.context.request.post('/api/v1/users', { data: { email: `temp-${tag}@e2e.test`, name: `Temp ${tag}`, password: 'temp-password' } })
	).json();
	await admin.page.goto(`/admin/users/${p.id}`);
	await admin.page.getByRole('button', { name: 'Delete user' }).click();
	await admin.page.getByRole('button', { name: `Delete Temp ${tag}` }).click();
	await expect(admin.page).toHaveURL(/\/admin\/users$/);
	expect((await admin.context.request.get(`/api/v1/users/${p.id}`)).status()).toBe(404);
});

test('an admin cannot demote, deactivate or delete themselves', async ({ browser }) => {
	const admin = await as(browser, 'admin');
	const me = await (await admin.context.request.get('/api/v1/auth/me')).json();
	await admin.page.goto(`/admin/users/${me.id}`);
	await expect(admin.page.getByRole('heading', { name: USERS.admin.name })).toBeVisible();
	await expect(admin.page.getByRole('combobox', { name: /^Role/ })).toBeDisabled();
	await expect(admin.page.getByRole('button', { name: 'Deactivate' })).toHaveCount(0);
	await expect(admin.page.getByRole('button', { name: 'Delete user' })).toHaveCount(0);
});

test('on a phone the user pages fit', async ({ browser }) => {
	const admin = await as(browser, 'admin', { viewport: { width: 390, height: 844 } });
	const me = await (await admin.context.request.get('/api/v1/auth/me')).json();
	for (const path of ['/admin/users', `/admin/users/${me.id}`]) {
		await admin.page.goto(path);
		await admin.page.waitForLoadState('networkidle');
		const overflow = await admin.page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
		expect(overflow, path).toBeLessThanOrEqual(0);
	}
});
