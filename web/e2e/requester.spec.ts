import { expect, test } from '@playwright/test';
import { api, as, composer, createTicket, details, openTicket, row, uid } from './helpers';

test.describe('a requester', () => {
	test('sees their ticket read-only, with staff replies but no internal notes', async ({ browser }) => {
		const lena = await as(browser, 'lena');
		const agent = await as(browser, 'agent');
		const t = await createTicket(lena.context, { title: `VPN drops ${uid()}`, description: 'Every 20 minutes.' });
		await api(agent.context, 'post', `/api/v1/tickets/${t.id}/comments`, { body: 'Looking into it.' });
		await api(agent.context, 'post', `/api/v1/tickets/${t.id}/comments`, { body: 'Firmware rollback needed.', internal: true });

		await openTicket(lena.page, t.number);
		await expect(lena.page.getByText('Every 20 minutes.')).toBeVisible();
		await expect(lena.page.getByText('Looking into it.')).toBeVisible();
		await expect(lena.page.getByText('Firmware rollback needed.')).toHaveCount(0);

		// No staff controls: no action buttons, no pickers, no internal-note switch.
		await expect(details(lena.page).locator('.actions')).toHaveCount(0);
		await expect(details(lena.page).getByRole('combobox')).toHaveCount(0); // no priority or assignee pickers
		await expect(details(lena.page).getByRole('button')).toHaveCount(0);
		await expect(details(lena.page).locator('.inline')).toContainText('Medium'); // but the priority is still shown
		await expect(lena.page.getByLabel('Internal note')).toHaveCount(0);

		// But they can still reply.
		await composer(lena.page).fill('Still happening today.');
		await lena.page.getByRole('button', { name: 'Add comment' }).click();
		await expect(lena.page.locator('li', { hasText: 'Still happening today.' })).toHaveCount(1);
	});

	test("cannot open or list someone else's ticket", async ({ browser }) => {
		const lena = await as(browser, 'lena');
		const marcus = await as(browser, 'marcus');
		const secret = await createTicket(marcus.context, { title: `Marcus only ${uid()}` });

		// Not found, and indistinguishable from a ticket that never existed.
		await lena.page.goto(`/tickets/${secret.number}`);
		await expect(lena.page.getByText('doesn’t exist, or you don’t have access')).toBeVisible();

		await lena.page.goto('/tickets');
		await lena.page.getByRole('button', { name: 'All', exact: true }).click();
		await expect(row(lena.page, secret.title)).toHaveCount(0);
		await marcus.page.goto('/tickets');
		await marcus.page.getByRole('button', { name: 'All', exact: true }).click();
		await expect(row(marcus.page, secret.title)).toBeVisible();
	});

	test('cannot use the staff API even by hand', async ({ browser }) => {
		const lena = await as(browser, 'lena');
		const t = await createTicket(lena.context, { title: `Mine ${uid()}` });
		expect((await lena.context.request.patch(`/api/v1/tickets/${t.id}`, { data: { title: 'hacked' } })).status()).toBe(403);
		expect((await lena.context.request.post(`/api/v1/tickets/${t.id}/transitions`, { data: { to: 'closed' } })).status()).toBe(403);
		expect((await lena.context.request.get('/api/v1/users/staff')).status()).toBe(403);
	});

	test('files a ticket from an empty queue', async ({ browser }) => {
		const { page } = await as(browser, 'lena');
		const title = `Printer jam ${uid()}`;
		await page.goto('/tickets');
		await page.getByRole('button', { name: 'New ticket' }).click();
		await page.getByLabel('What’s wrong?').fill(title);
		await page.getByRole('button', { name: 'Create ticket' }).click();

		await expect(page.getByRole('heading', { level: 1 })).toHaveText(title);
		await expect(row(page, title)).toBeVisible();
		await expect(details(page).getByText('New')).toBeVisible();
	});
});
