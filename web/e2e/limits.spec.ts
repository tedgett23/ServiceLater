import { request } from '@playwright/test';
import { as, expect, test, uid } from './helpers';

// Admin → Rate limits: an admin raises the API limit for people and lifts the webhook limit, and it holds after a
// reload; then a service account's key is given its own limit, which its responses report. Only raises limits (the
// server is shared with every other test; global setup turns the API limit off) and puts back what it found.
test('an admin sets the rate limits, and one API key gets its own', async ({ browser, baseURL }) => {
	const id = uid();
	const admin = await as(browser, 'admin');
	const { page } = admin;
	const before = await (await admin.context.request.get('/api/v1/settings/limits')).json();
	delete before.defaults;
	try {
		await page.goto('/admin');
		await page.getByRole('navigation', { name: 'Admin' }).getByRole('link', { name: 'Rate limits' }).click();
		await expect(page.getByRole('heading', { level: 1, name: 'Rate limits' })).toBeVisible();
		const apiGroup = page.getByRole('group', { name: 'API requests' });
		await apiGroup.getByLabel('No limit').first().uncheck();
		await expect(apiGroup.getByLabel('A person')).toHaveValue('1200');
		await apiGroup.getByLabel('A person').fill('5');
		await expect(apiGroup.getByText('10 to 1,000,000, or no limit.').first()).toBeVisible();
		await expect(page.getByRole('button', { name: 'Save' })).toBeDisabled();
		await apiGroup.getByLabel('A person').fill('5000');
		await page.getByRole('group', { name: 'Webhooks' }).getByLabel('No limit').check();
		await page.getByRole('button', { name: 'Save' }).click();
		await expect(page.getByRole('button', { name: 'Save' })).toBeDisabled();
		await page.reload();
		await expect(apiGroup.getByLabel('A person')).toHaveValue('5000');
		await expect(page.getByRole('group', { name: 'Webhooks' }).getByLabel('No limit')).toBeChecked();
		// The web app's own requests now say the new limit.
		const res = await admin.context.request.get('/api/v1/tickets');
		await expect.poll(async () => (await admin.context.request.get('/api/v1/tickets')).headers()['ratelimit-limit']).toBe('5000');
		expect(res.status()).toBe(200);

		// A key with its own limit.
		const sa = (await (await admin.context.request.post('/api/v1/service-accounts', { data: { name: `Monitoring ${id}`, role: 'agent' } })).json()) as { id: string };
		const made = (await (await admin.context.request.post(`/api/v1/users/${sa.id}/api-keys`, { data: { name: 'Alertmanager' } })).json()) as { key: string };
		await page.goto(`/admin/users/${sa.id}`);
		const keys = page.getByRole('region', { name: 'API keys' });
		const row = keys.locator('li', { hasText: 'Alertmanager' });
		await expect(row).toContainText('Rate limit: the setting for keys');
		await row.getByRole('button', { name: 'Rate limit for Alertmanager' }).click();
		const form = keys.getByRole('form', { name: 'Rate limit for Alertmanager' });
		await form.getByLabel('Its own:').check();
		await form.getByLabel('Requests a minute').fill('3000');
		await form.getByRole('button', { name: 'Save' }).click();
		await expect(row).toContainText('Rate limit: 3,000 a minute');
		const integration = await request.newContext({ baseURL, extraHTTPHeaders: { Authorization: `Bearer ${made.key}` } });
		try {
			await expect.poll(async () => (await integration.get('/api/v1/tickets')).headers()['ratelimit-limit']).toBe('3000');
		} finally {
			await integration.dispose();
		}
		await page.goto('/admin/limits');
		await page.getByRole('button', { name: 'Use the defaults' }).click();
		await expect(page.getByRole('group', { name: 'API requests' }).getByLabel('A person')).toHaveValue('1200');
		await expect(page.getByRole('group', { name: 'Webhooks' }).getByLabel('No limit')).not.toBeChecked();
	} finally {
		expect((await admin.context.request.put('/api/v1/settings/limits', { data: before })).status()).toBe(200);
	}
});
