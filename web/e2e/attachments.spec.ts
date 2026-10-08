import fs from 'node:fs';
import { expect, test, type Page } from '@playwright/test';
import { as, createTicket, openTicket, uid } from './helpers';
import { USERS } from './users';

const section = (page: Page) => page.getByRole('region', { name: 'Attachments' });

// A tiny PNG, so the ticket shows a preview.
const PNG = Buffer.from(
	'iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAFklEQVR4nGP8z8DwnwEIGBkZGRgYGAAwBgQAg6bRyQAAAABJRU5ErkJggg==',
	'base64'
);

// Files chosen while filing upload once the ticket exists, carry on as the dialog closes, and are then there for
// staff to download.
test('a requester attaches files as she files a ticket, and an agent downloads them', async ({ browser }) => {
	const id = uid();
	const lena = await as(browser, 'lena');
	await lena.page.goto('/');
	await lena.page.getByRole('button', { name: 'New ticket' }).click();
	const dialog = lena.page.getByRole('dialog', { name: 'New ticket' });
	await dialog.locator('#nt-title').fill(`Scanner shows an error ${id}`);
	await dialog.getByLabel('Attach files').setInputFiles([
		{ name: 'error screen.png', mimeType: 'image/png', buffer: PNG },
		{ name: 'scanner.log', mimeType: 'text/plain', buffer: Buffer.from(`paper jam at tray 2 ${id}\n`) }
	]);
	await expect(dialog.getByRole('list', { name: 'Files to attach' }).getByRole('listitem')).toHaveCount(2);
	await dialog.getByRole('button', { name: 'Create ticket' }).click();
	await expect(lena.page).toHaveURL(/\/tickets\/INC\d+$/);
	const files = section(lena.page);
	await expect(files.getByRole('button', { name: 'error screen.png', exact: true })).toBeVisible();
	await expect(files.getByRole('button', { name: 'scanner.log', exact: true })).toBeVisible();
	await expect(files.locator('img')).toHaveCount(1);
	const ref = lena.page.url().split('/').pop()!;

	// The agent sees both, and downloads the log as it was sent.
	const { page } = await as(browser, 'agent');
	await openTicket(page, ref);
	await expect(section(page).getByText(USERS.lena.name).first()).toBeVisible();
	const download = page.waitForEvent('download');
	await section(page).getByRole('button', { name: 'scanner.log', exact: true }).click();
	const d = await download;
	expect(d.suggestedFilename()).toBe('scanner.log');
	expect(fs.readFileSync(await d.path(), 'utf8')).toBe(`paper jam at tray 2 ${id}\n`);
});

// Staff attach on the record itself, everyone watching sees it arrive, and removing one asks first and is in the
// history.
test('an agent attaches and removes a file, and the requester sees it live', async ({ browser }) => {
	const id = uid();
	const lena = await as(browser, 'lena');
	const t = await createTicket(lena.context, { title: `VPN drops hourly ${id}` });
	await openTicket(lena.page, t.number);
	await expect(section(lena.page)).toContainText('No files yet');

	const { page } = await as(browser, 'agent');
	await openTicket(page, t.number);
	await section(page).getByLabel('Attach files').setInputFiles({ name: `vpn-${id}.txt`, mimeType: 'text/plain', buffer: Buffer.from('trace') });
	await expect(section(page).getByRole('button', { name: `vpn-${id}.txt`, exact: true })).toBeVisible();
	await expect(section(lena.page).getByRole('button', { name: `vpn-${id}.txt`, exact: true })).toBeVisible(); // live, no reload
	// The requester may not remove what the agent attached.
	await expect(section(lena.page).getByRole('button', { name: `Remove vpn-${id}.txt` })).toHaveCount(0);

	await section(page).getByRole('button', { name: `Remove vpn-${id}.txt` }).click();
	await section(page).getByRole('group', { name: `Remove vpn-${id}.txt?` }).getByRole('button', { name: 'Remove' }).click();
	await expect(section(page).getByRole('button', { name: `vpn-${id}.txt`, exact: true })).toHaveCount(0);
	await expect(section(lena.page).getByRole('button', { name: `vpn-${id}.txt`, exact: true })).toHaveCount(0);

	const activity = page.locator('section.activity');
	await activity.getByLabel('Show').selectOption('history');
	await expect(activity.locator('li.change', { hasText: 'attached' })).toContainText(`${USERS.agent.name} attached vpn-${id}.txt`);
	await expect(activity.locator('li.change', { hasText: 'removed' })).toContainText(`removed the attachment vpn-${id}.txt`);
	await activity.getByLabel('Show').selectOption('all'); // the choice is remembered: leave the default for later tests
});

// Someone who cannot see a record has nothing to attach to, and an end user's own ticket offers attaching.
test('only those who may attach are offered it', async ({ browser }) => {
	const lena = await as(browser, 'lena');
	const t = await createTicket(lena.context, { title: `Mouse ${uid()}` });
	const marcus = await as(browser, 'marcus');
	const res = await marcus.context.request.post(`/api/v1/tickets/${t.id}/attachments`, { data: { filename: 'x.txt', size: 1 } });
	expect(res.status()).toBe(404);
	await openTicket(lena.page, t.number);
	await expect(section(lena.page).getByText('Attach files')).toBeVisible();
});

// The admin page says where files go and tests it from the server.
test('an admin sees where files are kept and tests storage', async ({ browser }) => {
	const { page } = await as(browser, 'admin');
	await page.goto('/admin/storage');
	await expect(page.getByRole('radio', { name: /A folder on the server/ })).toBeChecked();
	await expect(page.getByText('.attachments')).toBeVisible();
	await page.getByRole('button', { name: 'Test storage' }).click();
	await expect(page.getByRole('status')).toContainText('Storage works');

	// Choosing a bucket shows its settings and the CORS rules browsers need; nothing is saved here.
	await page.getByRole('radio', { name: /An S3 bucket/ }).check();
	await expect(page.getByRole('textbox', { name: 'Bucket' })).toBeVisible();
	await expect(page.locator('pre')).toContainText('"ETag"');
	await expect(page.getByRole('button', { name: 'Test storage' })).toBeDisabled();
});

// Files sit in the conversation where they arrived, so nobody has to guess which screenshot came before which reply;
// one removed later is still accounted for there.
test('files appear among the comments in the order they came', async ({ browser }) => {
	const id = uid();
	const lena = await as(browser, 'lena');
	const t = await createTicket(lena.context, { title: `Login loops ${id}` });
	const { page } = await as(browser, 'agent');
	await openTicket(lena.page, t.number);
	await openTicket(page, t.number);
	const activity = page.locator('section.activity');
	await activity.getByLabel('Show').selectOption('comments');

	await section(lena.page).getByLabel('Attach files').setInputFiles({ name: `before-${id}.png`, mimeType: 'image/png', buffer: PNG });
	await expect(activity.locator('li.file')).toHaveCount(1);
	await page.locator('.composer').getByRole('textbox').fill(`Try clearing cookies ${id}`);
	await page.getByRole('button', { name: 'Add comment' }).click();
	await expect(activity.locator('li', { hasText: `Try clearing cookies ${id}` })).toBeVisible();
	await section(lena.page).getByLabel('Attach files').setInputFiles({ name: `after-${id}.png`, mimeType: 'image/png', buffer: PNG });
	await expect(activity.locator('li.file')).toHaveCount(2);

	const items = activity.locator('ol > li');
	await expect(items).toHaveCount(3);
	await expect(items.nth(0)).toContainText(`${USERS.lena.name} attached a file`);
	await expect(items.nth(0)).toContainText(`before-${id}.png`);
	await expect(items.nth(0).locator('img')).toBeVisible();
	await expect(items.nth(1)).toContainText(`Try clearing cookies ${id}`);
	await expect(items.nth(2)).toContainText(`after-${id}.png`);

	// Lena removes the first: the conversation still says it was there, and that it went.
	await section(lena.page).getByRole('button', { name: `Remove before-${id}.png` }).click();
	await section(lena.page).getByRole('button', { name: 'Remove', exact: true }).click();
	await expect(activity.locator('li.change', { hasText: `attached before-${id}.png` })).toBeVisible();
	await expect(activity.locator('li.change', { hasText: `removed the attachment before-${id}.png` })).toBeVisible();
	await expect(items).toHaveCount(4);
	// Still in the order things happened: attached, the reply, the second file, then the removal.
	await expect(items.nth(0)).toContainText(`${USERS.lena.name} attached before-${id}.png`);
	await expect(items.nth(1)).toContainText(`Try clearing cookies ${id}`);
	await expect(items.nth(2)).toContainText(`after-${id}.png`);
	await expect(items.nth(3)).toContainText(`removed the attachment before-${id}.png`);
});
