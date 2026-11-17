import { expect, test } from '@playwright/test';
import path from 'node:path';
import { installApp, servicelater } from './apps';
import { as, createTicket, details, row, uid } from './helpers';

// The tyler sample app (tests only) defines its own record type: Things, numbered TYL1, TYL2 ...
const TYLER = path.resolve(import.meta.dirname, '../../backend/internal/apps/testdata/tyler');

const uninstallTyler = () => {
	try {
		servicelater('uninstall', 'tyler');
	} catch {
		/* not installed */
	}
};

test.describe('record types from apps', () => {
	test.afterEach(uninstallTyler);

	test('an app adds a type that is filed and shown by its own reference, then retired with the app', async ({ browser }) => {
		installApp(TYLER);
		const { context, page } = await as(browser, 'agent');
		const title = `Desk lamp ${uid()}`;

		await page.goto('/tickets');
		await page.getByRole('button', { name: 'New ticket' }).click();
		const dialog = page.getByRole('dialog', { name: 'New ticket' });
		await dialog.getByText('Thing', { exact: true }).click();
		await dialog.getByLabel('Summary').fill(title);
		await dialog.getByLabel('Size').selectOption({ label: 'Large' });
		await dialog.getByRole('button', { name: 'Create ticket' }).click();

		// Its own prefix and numbering, lifecycle and fields.
		await expect(page.getByRole('heading', { level: 1 })).toHaveText(title);
		await expect(page).toHaveURL(/\/tickets\/TYL\d+$/);
		const ref = new URL(page.url()).pathname.split('/').pop()!;
		// Read as "Thing TYL1". Regressed: the space after the type's name was dropped (a leading or trailing space in
		// markup is), so screen readers heard "ThingTYL1".
		await expect(page.locator('.stub')).toHaveText(`Thing ${ref}`);
		await expect(details(page).getByText('New', { exact: true })).toBeVisible();
		await expect(page.getByRole('region', { name: 'Thing' }).getByLabel('Size')).toHaveValue('large');

		// The page shows exactly what the API says.
		const fromApi = (await (await context.request.get(`/api/v1/tickets/${ref}`)).json()) as { reference: string; priority: number };
		expect(fromApi.reference).toBe(ref);
		expect(fromApi.priority).toBe(2); // the app's rule: large things matter

		await page.getByRole('button', { name: 'All', exact: true }).click();
		await expect(row(page, title)).toContainText(ref);

		// Removing the app hides its type everywhere, but the record still opens by reference.
		const anchor = await createTicket(context, { title: `Still listed ${uid()}` });
		servicelater('uninstall', 'tyler');
		await page.goto('/tickets');
		await page.getByRole('button', { name: 'All', exact: true }).click();
		await expect(row(page, anchor.title)).toBeVisible(); // the list has loaded
		await expect(row(page, title)).toHaveCount(0);
		await page.getByRole('button', { name: 'New ticket' }).click();
		await expect(dialog.getByText('Thing', { exact: true })).toHaveCount(0);
		await page.keyboard.press('Escape');

		await page.goto(`/tickets/${ref}`);
		await expect(page.getByRole('heading', { level: 1 })).toHaveText(title);
		await expect(page.locator('.stub')).toContainText(ref);
	});
});
