import { type BrowserContext } from '@playwright/test';
import { as, createTicket, details, expect, openTicket, test, uid } from './helpers';

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

	test('linking an item routes an unrouted ticket to the item\'s support group', async ({ browser }) => {
		const admin = await as(browser, 'admin');
		const id = uid();
		const group = (await (await admin.context.request.post('/api/v1/groups', { data: { name: `DB team ${id}` } })).json()) as { id: string };
		await addItem(admin.context, 'database', `orders-db-${id}`, { support_group_id: group.id });

		const { context, page } = await as(browser, 'agent');
		const t = await createTicket(context, { title: `Orders time out ${id}` });
		await openTicket(page, t.number);
		await expect(details(page).getByLabel('Assignment group')).toHaveValue('');
		const affected = page.getByRole('region', { name: 'Affected items' });
		await affected.getByLabel('Link an item').fill(`orders-db-${id}`);
		await affected.getByRole('button', { name: `Link orders-db-${id}` }).click();
		// The cmdb app's rule routed it as the link was saved; the panel follows.
		await expect(details(page).getByLabel('Assignment group')).toHaveValue(group.id);
	});

	test('a freeze limited to an item holds back only the changes linked to it', async ({ browser }) => {
		const admin = await as(browser, 'admin');
		const agent = await as(browser, 'agent');
		const id = uid();
		const frozen = await addItem(admin.context, 'database', `ledger-db-${id}`);
		const free = await addItem(admin.context, 'database', `wiki-db-${id}`);
		// Years ahead, on days of their own, so no other test's changes land in it.
		const base = new Date(2033, 0, 6 + Math.floor(Math.random() * 300));
		const at = (days: number, hours = 0) => new Date(base.getFullYear(), base.getMonth(), base.getDate() + days, hours);
		const local = (d: Date) =>
			`${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}T${String(d.getHours()).padStart(2, '0')}:00`;
		const window = { planned_start: at(2, 10).toISOString(), planned_end: at(2, 12).toISOString() };
		let freezeID = '';
		try {
			await admin.page.goto(`/calendar?date=${local(at(2)).slice(0, 10)}`);
			await admin.page.getByRole('button', { name: 'Declare freeze' }).click();
			const form = admin.page.getByRole('form', { name: 'Declare a change freeze' });
			await form.getByLabel('Name').fill(`Ledger close ${id}`);
			await form.getByLabel('Starts').fill(local(at(1)));
			await form.getByLabel('Ends').fill(local(at(3)));
			await form.getByLabel('Limit it to an item').fill(`ledger-db-${id}`);
			// Enter in the item search must not declare the freeze before an item is picked.
			await form.getByLabel('Limit it to an item').press('Enter');
			await form.getByRole('button', { name: `Add ledger-db-${id}` }).click();
			const created = admin.page.waitForResponse((r) => r.url().endsWith('/api/v1/freeze-windows') && r.request().method() === 'POST');
			await form.getByRole('button', { name: 'Declare freeze' }).click();
			freezeID = ((await (await created).json()) as { id: string }).id;
			await expect(admin.page.locator('li.freeze', { hasText: `Ledger close ${id}` })).toContainText(`Only changes to ledger-db-${id}`);

			// A change to another item is planned into it; one to the frozen item is not.
			const refused = await agent.context.request.post('/api/v1/tickets', {
				data: { type: 'change', title: `Ledger patch ${id}`, cis: [frozen.id], change: window }
			});
			expect(refused.status()).toBe(422);
			const other = await createTicket(agent.context, { type: 'change', title: `Wiki upgrade ${id}`, change: window });
			const linked = await agent.context.request.put(`/api/v1/tickets/${other.id}/cis/${free.id}`);
			expect(linked.status()).toBe(200);
			await openTicket(agent.page, other.reference);
			const affected = agent.page.getByRole('region', { name: 'Affected items' });
			await affected.getByLabel('Link an item').fill(`ledger-db-${id}`);
			await affected.getByRole('button', { name: `Link ledger-db-${id}` }).click();
			await expect(affected.getByRole('alert')).toContainText('overlaps a change freeze that applies to it');
			await expect(affected.getByRole('link', { name: `ledger-db-${id}` })).toHaveCount(0);
		} finally {
			if (freezeID) await admin.context.request.delete(`/api/v1/freeze-windows/${freezeID}`);
		}
	});

	// Importing from a spreadsheet: download the template, fill it in, upload it, check what each row will do, import.
	test('an admin imports servers from a CSV file, leaving out a row with a problem', async ({ browser }) => {
		const id = uid();
		const { page } = await as(browser, 'admin');
		await page.goto('/cmdb');
		await page.getByRole('link', { name: 'Import' }).click();
		await expect(page.getByRole('heading', { name: 'Import items' })).toBeVisible();
		await page.getByLabel('Import', { exact: true }).selectOption('server');

		const [template] = await Promise.all([page.waitForEvent('download'), page.getByRole('button', { name: 'Download the template' }).click()]);
		expect(template.suggestedFilename()).toBe('server-template.csv');

		const csv = `name,status,hostname,environment,virtual\nimp-${id}-a,Active,a.example.com,Production,yes\nimp-${id}-b,Broken,,,\n`;
		await page.getByLabel('CSV file').setInputFiles({ name: 'servers.csv', mimeType: 'text/csv', buffer: Buffer.from(csv) });
		await expect(page.getByRole('status').filter({ hasText: 'to add' })).toHaveText('1 to add, 1 with a problem.');
		const rows = page.getByRole('list', { name: 'Rows' });
		await expect(rows.locator('li.create')).toContainText(`imp-${id}-a`);
		await expect(rows.locator('li.error')).toContainText('status must be');

		const go = page.getByRole('button', { name: 'Import 1 row' });
		await expect(go).toBeDisabled();
		await page.getByLabel('Leave out the rows with problems').check();
		await go.click();
		await expect(page.getByRole('status').filter({ hasText: 'Imported' })).toContainText('1 added, 1 skipped');

		await page.goto(`/cmdb`);
		await page.getByRole('searchbox', { name: 'Name' }).fill(`imp-${id}`);
		await expect(page.getByRole('link', { name: new RegExp(`imp-${id}-a`) })).toBeVisible();
		await expect(page.getByRole('link', { name: new RegExp(`imp-${id}-b`) })).toHaveCount(0);
	});

	// A class that others extend imports its whole family from one file, a class column saying which class each row is.
	test('one hardware file adds a printer and a server', async ({ browser }) => {
		const id = uid();
		const { page } = await as(browser, 'admin');
		await page.goto('/cmdb/import');
		await page.getByLabel('Import', { exact: true }).selectOption('hardware');
		await page.getByText('What each column takes').click();
		await expect(page.locator('dt', { hasText: 'class' })).toBeVisible();
		await expect(page.locator('dd', { hasText: 'Server only' }).first()).toBeVisible();

		const csv = `name,class,serial_number,hostname\nprn-${id},,SN-${id},\nsrv-${id},Server,,srv-${id}.example.com\n`;
		await page.getByLabel('CSV file').setInputFiles({ name: 'hardware.csv', mimeType: 'text/csv', buffer: Buffer.from(csv) });
		await expect(page.getByRole('status').filter({ hasText: 'to add' })).toHaveText('2 to add.');
		await expect(page.getByRole('list', { name: 'Rows' }).locator('li', { hasText: `srv-${id}` })).toContainText('Server');
		await page.getByRole('button', { name: 'Import 2 rows' }).click();
		await expect(page.getByRole('status').filter({ hasText: 'Imported' })).toContainText('2 added');

		await page.goto('/cmdb');
		await page.getByRole('searchbox', { name: 'Name' }).fill(id);
		await expect(page.getByRole('link', { name: new RegExp(`srv-${id}`) })).toContainText('Server');
		await expect(page.getByRole('link', { name: new RegExp(`prn-${id}`) })).toContainText('Hardware');
	});
});
