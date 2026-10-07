import { expect, test } from '@playwright/test';
import { as, createTicket, details, openTicket, row, uid } from './helpers';

// incident-management ships respond and resolve SLAs, counted by the business-hours calendar. These tests never
// change that calendar: the server and database are shared, and every other test's incidents count by it.
test.describe('SLAs', () => {
	test('an incident shows its clocks: responding meets one, putting it on hold pauses the other', async ({ browser }) => {
		const { context, page } = await as(browser, 'agent');
		const title = `Projector flickers ${uid()}`;
		const t = await createTicket(context, { title, priority: 2 });

		await openTicket(page, t.reference);
		const clocks = details(page).locator('.clocks');
		const respond = clocks.locator('li', { hasText: 'Respond' });
		const resolve = clocks.locator('li', { hasText: 'Resolve' });
		await expect(respond).toContainText(/Due (in|now)/);
		await expect(resolve).toContainText('of 8h used');

		await details(page).getByRole('button', { name: 'Assign to me' }).click();
		await expect(respond).toContainText('Met');
		await page.getByRole('button', { name: 'Put on hold' }).click();
		await expect(resolve).toContainText('Paused');
		await page.getByRole('button', { name: 'Resume' }).click();
		await expect(resolve).toContainText(/Due (in|now)/);

		// The queue's Due view lists open work by SLA, and the row says when it is due.
		await page.goto('/tickets');
		await page.getByRole('button', { name: 'Due', exact: true }).click();
		await expect(row(page, title).locator('.sla')).toHaveText(/Due (in|now)/);
	});

	// Regressed: adding the Due view made six queue views, which no longer fit a phone: All was cut off and had to be
	// scrolled to.
	test('every queue view, Due included, fits a phone screen', async ({ browser }) => {
		const { page } = await as(browser, 'agent', { viewport: { width: 390, height: 844 } });
		await page.goto('/tickets');
		const views = page.getByRole('group', { name: 'Show' });
		for (const name of ['Mine', 'My groups', 'Unassigned', 'Due', 'Open', 'All']) {
			await expect(views.getByRole('button', { name, exact: true })).toBeInViewport({ ratio: 1 });
		}
		expect(await views.evaluate((e) => e.scrollWidth <= e.clientWidth)).toBe(true);
	});

	test('an admin adds a calendar, gives it a holiday and removes it', async ({ browser }) => {
		const { page } = await as(browser, 'admin');
		const id = uid();
		const label = `Night shift ${id}`;
		await page.goto('/admin');
		await page.getByRole('navigation', { name: 'Admin' }).getByRole('link', { name: 'Calendars' }).click();
		await expect(page.getByRole('heading', { name: 'Calendars' })).toBeVisible();
		// The calendar incident-management's SLAs name is there from the start.
		await expect(page.locator('.calendars li', { hasText: 'business-hours' })).toContainText('Mon–Fri 09:00–17:00');

		await page.getByRole('button', { name: 'Add calendar' }).click();
		const form = page.getByRole('form', { name: 'Add a calendar' });
		await form.getByLabel('Label').fill(label);
		await form.getByLabel(/^Name/).fill(`night-${id}`);
		await form.getByLabel('Time zone').fill('Europe/London');
		await form.getByRole('button', { name: 'Add hours on Saturday' }).click();
		await form.getByLabel('Saturday from').fill('22:00');
		await form.getByLabel('Saturday until').fill('23:30');
		await form.getByRole('button', { name: 'Add calendar' }).click();
		const item = page.locator('.calendars li', { hasText: label });
		await expect(item).toContainText('Sat 22:00–23:30');
		await expect(item).toContainText('Europe/London');

		await item.getByRole('button', { name: `Edit ${label}` }).click();
		const edit = page.getByRole('form', { name: `Change ${label}` });
		await edit.getByLabel('Holiday date').fill('2031-12-25');
		await edit.getByRole('button', { name: 'Add holiday' }).click();
		await edit.getByRole('button', { name: 'Save calendar' }).click();
		await expect(item).toContainText('1 holiday');

		await item.getByRole('button', { name: `Remove ${label}` }).click();
		await page.getByRole('button', { name: `Remove ${label}` }).last().click();
		await expect(page.locator('.calendars li', { hasText: label })).toHaveCount(0);
	});
});
