import { request } from '@playwright/test';
import { as, expect, test, uid } from './helpers';

// An admin adds a service account for an integration, makes it a key narrowed to filing incidents, and the key works
// from outside the browser, as that account, until it is revoked.
test('an admin makes a service account and an API key that works until revoked', async ({ browser, baseURL }) => {
	const id = uid();
	const { page } = await as(browser, 'admin');
	await page.goto('/admin/users');
	const form = page.getByRole('form', { name: 'Add a service account' });
	await form.getByLabel('Name').fill(`Monitoring ${id}`);
	await form.getByLabel('Base role').selectOption('agent');
	await form.getByLabel('What it is for').fill('Files incidents from alerts.');
	await form.getByRole('button', { name: 'Add service account' }).click();

	await expect(page.getByRole('heading', { level: 1, name: `Monitoring ${id}` })).toBeVisible();
	await expect(page.getByText('Service account, base role agent')).toBeVisible();
	await expect(page.getByRole('heading', { name: 'Password' })).toHaveCount(0);
	const keys = page.getByRole('region', { name: 'API keys' });
	await keys.getByRole('button', { name: 'New key' }).click();
	await keys.getByLabel('Name').fill('Alertmanager');
	await keys.getByLabel('Only some of what this account can do').check();
	await keys.getByRole('checkbox', { name: 'records:create:*' }).check();
	await keys.getByRole('checkbox', { name: 'records:read:*' }).check();
	await keys.getByRole('button', { name: 'Make key' }).click();

	const shown = keys.getByRole('region', { name: 'New key' }).locator('code.key');
	await expect(shown).toContainText('slk_');
	const key = (await shown.textContent())!.trim();
	await keys.getByRole('button', { name: 'Done' }).click();
	await expect(keys.locator('li', { hasText: 'Alertmanager' })).toContainText('Only: records:create:*, records:read:*');

	const integration = await request.newContext({ baseURL, extraHTTPHeaders: { Authorization: `Bearer ${key}` } });
	try {
		const me = await integration.get('/api/v1/auth/me');
		expect(me.status()).toBe(200);
		expect((await me.json()).name).toBe(`Monitoring ${id}`);
		const filed = await integration.post('/api/v1/tickets', { data: { title: `Disk full on db-1 ${id}` } });
		expect(filed.status()).toBe(201);
		const users = await integration.get('/api/v1/users/staff');
		expect(users.status(), 'narrowed away from what its agent role allows').toBe(403);

		await page.reload();
		await expect(keys.locator('li', { hasText: 'Alertmanager' })).toContainText('Last used');
		await keys.getByRole('button', { name: 'Revoke Alertmanager' }).click();
		await keys.getByRole('button', { name: 'Revoke Alertmanager' }).click();
		await expect(keys.locator('li', { hasText: 'Alertmanager' })).toContainText('Revoked');
		expect((await integration.get('/api/v1/auth/me')).status()).toBe(401);
	} finally {
		await integration.dispose();
	}
});
