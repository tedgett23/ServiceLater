import { expect, test, type BrowserContext } from '@playwright/test';
import { as, createTicket, openTicket, uid } from './helpers';

type CI = { id: string; name: string };

async function addItem(context: BrowserContext, cls: string, name: string, more: Record<string, unknown> = {}): Promise<CI> {
	const res = await context.request.post('/api/v1/cis', { data: { class: cls, name, ...more } });
	expect(res.status(), `add ${name}`).toBe(201);
	return res.json();
}

async function relate(context: BrowserContext, from: CI, type: string, to: CI) {
	const res = await context.request.post(`/api/v1/cis/${from.id}/relationships`, { data: { type, to: to.id } });
	expect(res.status(), `${from.name} ${type} ${to.name}`).toBe(201);
}

// The CMDB (the cmdb app's classes and relationship types): admins keep it, agents read it and link items to the
// tickets they work on, and requesters never see it.
test.describe('CMDB', () => {
	test('an admin adds a server with its class fields, relates it, and sees what its failure reaches', async ({ browser }) => {
		const { context, page } = await as(browser, 'admin');
		const id = uid();
		const payroll = await addItem(context, 'business_service', `Payroll ${id}`);
		const app = await addItem(context, 'application', `Payroll app ${id}`);
		await relate(context, payroll, 'depends_on', app);

		await page.goto('/cmdb');
		await page.getByRole('link', { name: 'New item' }).click();
		await page.getByLabel('Class').selectOption('server');
		// A server is hardware, so it has hardware's fields as well as its own.
		await expect(page.getByLabel('Serial number')).toBeVisible();
		await page.getByLabel('Name', { exact: true }).fill(`app-host-${id}`);
		await page.getByLabel('Hostname').fill(`app-host-${id}.example.net`);
		await page.getByLabel('Environment').selectOption('production');
		await page.getByRole('button', { name: 'Add item' }).click();

		// A new item has no relationships or tickets: the page once rendered blank because the server left the empty
		// lists out.
		await expect(page.getByRole('heading', { level: 1 })).toHaveText(`app-host-${id}`);
		await expect(page.getByLabel('Hostname')).toHaveValue(`app-host-${id}.example.net`);

		// "The payroll app runs on app-host": added from the server's side, where it reads "app-host runs the payroll app".
		await page.getByLabel(/Add a relationship/).selectOption({ label: 'Runs' });
		await page.getByLabel('Runs…').fill(`Payroll app ${id}`);
		await page.getByRole('button', { name: `Add Payroll app ${id}` }).click();
		const relationships = page.getByRole('region', { name: 'Relationships' });
		await expect(relationships.getByRole('link', { name: `Payroll app ${id}` })).toBeVisible();

		// The payroll service depends on the app, which runs on this server. (The separator once lost its space,
		// "Business service· through", when it started an {#if} block.)
		const fails = page.getByRole('region', { name: 'If it fails' });
		await expect(fails.locator('li')).toHaveText([
			new RegExp(`Payroll app ${id}\\s*Application`),
			new RegExp(`Payroll ${id}\\s*Business service · through Payroll app ${id}`)
		]);

		// Retiring it hides it from the list unless retired items are asked for.
		await page.getByLabel('Status').selectOption('retired');
		await expect(page.locator('.lede')).toContainText('Retired');
		await page.goto('/cmdb');
		await page.getByLabel('Name').fill(`app-host-${id}`);
		await expect(page.getByText('Nothing matches.')).toBeVisible();
		await page.getByLabel('Include retired').check();
		await expect(page.getByRole('link', { name: new RegExp(`app-host-${id}`) })).toBeVisible();
	});

	test('an agent links an item to an incident and sees the services it affects; the requester sees none of it', async ({ browser }) => {
		const admin = await as(browser, 'admin');
		const id = uid();
		const mail = await addItem(admin.context, 'business_service', `Mail ${id}`);
		const mx = await addItem(admin.context, 'server', `mx-${id}`);
		await relate(admin.context, mail, 'depends_on', mx);

		const lena = await as(browser, 'lena');
		const t = await createTicket(lena.context, { title: `Mail is not arriving ${id}` });

		const { page } = await as(browser, 'agent');
		await openTicket(page, t.number);
		const affected = page.getByRole('region', { name: 'Affected items' });
		await affected.getByLabel('Link an item').fill(`mx-${id}`);
		await affected.getByRole('button', { name: `Link mx-${id}` }).click();
		await expect(affected.getByRole('link', { name: `mx-${id}` })).toBeVisible();
		await expect(affected.getByRole('list', { name: 'Also affected' })).toContainText(`Mail ${id}`);
		await expect(affected.getByRole('list', { name: 'Also affected' })).toContainText(`through mx-${id}`);

		// The item lists the ticket.
		await affected.getByRole('link', { name: `mx-${id}` }).click();
		await expect(page.getByRole('region', { name: 'Tickets' })).toContainText(`Mail is not arriving ${id}`);
		// Agents read the CMDB but do not change it.
		await expect(page.getByRole('button', { name: 'Remove item' })).toHaveCount(0);
		await expect(page.getByLabel('Hostname')).toHaveCount(0);

		// The requester's own ticket shows no items, and the CMDB is not theirs to open.
		await openTicket(lena.page, t.number);
		await expect(lena.page.getByRole('heading', { level: 1 })).toHaveText(`Mail is not arriving ${id}`);
		await expect(lena.page.getByRole('region', { name: 'Affected items' })).toHaveCount(0);
		await expect(lena.page.getByRole('link', { name: 'CMDB' })).toHaveCount(0);
		await lena.page.goto('/cmdb');
		await expect(lena.page).toHaveURL(/\/tickets/);

		// An item linked to a ticket cannot be removed.
		const refused = await admin.context.request.delete(`/api/v1/cis/${mx.id}`);
		expect(refused.status()).toBe(409);
	});

	// Regressed: adding CMDB gave an admin five sections, more than the phone's section bar holds, and every page became
	// wider than the screen. The bar scrolls sideways on its own now.
	test('an admin with every section still fits a phone, and reaches the CMDB from the section bar', async ({ browser }) => {
		const { page } = await as(browser, 'admin', { viewport: { width: 390, height: 844 } });
		const fits = () => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth);
		for (const path of ['/tickets', '/cmdb', '/cmdb/new']) {
			await page.goto(path);
			await expect(page.getByRole('navigation', { name: 'Sections' })).toBeVisible();
			expect(await fits(), `${path} is wider than the screen`).toBe(true);
		}
		await page.getByRole('navigation', { name: 'Sections' }).getByRole('link', { name: 'CMDB' }).click();
		await expect(page.getByRole('heading', { name: 'Configuration items' })).toBeVisible();
	});
});
