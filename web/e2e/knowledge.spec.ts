import { expect, test } from '@playwright/test';
import { as, createTicket, openTicket, uid } from './helpers';

// The knowledge base: an agent writes an article and sends it for review, an admin publishes it, and a requester finds
// it by searching, is offered it while filing a ticket, and reads it once an agent links it to their ticket.
test.describe('Knowledge', () => {
	test('an article is written, reviewed, published, found and suggested', async ({ browser }) => {
		const id = uid();
		const title = `Clearing a ${id} jam`;
		const agent = await as(browser, 'agent');
		await agent.page.goto('/kb');
		await agent.page.getByRole('link', { name: 'New article' }).click();
		const editor = agent.page.getByRole('form', { name: 'New article' });
		await editor.getByLabel('Title').fill(title);
		await editor.getByLabel('Category').fill(`Printing ${id}`);
		await editor.getByLabel('Tags').fill('printer, jam');
		await editor
			.getByLabel('Article text (Markdown)')
			.fill(`Open **tray 2** and pull the ${id} paper out.\n\n<img src="x" onerror="window.__pwned = 1">\n\n[More help](javascript:alert(1))`);
		await editor.getByRole('tab', { name: 'Preview' }).click();
		await expect(editor.locator('.preview strong')).toHaveText('tray 2');
		await editor.getByRole('button', { name: 'Save draft' }).click();

		const page = agent.page;
		await expect(page.getByRole('heading', { level: 1 })).toHaveText(title);
		await expect(page.locator('.ref')).toContainText('Draft');
		// The Markdown is rendered and sanitised: no event handlers, no javascript: links.
		await expect(page.locator('.body strong')).toHaveText('tray 2');
		expect(await page.locator('.body [onerror]').count()).toBe(0);
		expect(await page.locator('.body a[href^="javascript"]').count()).toBe(0);
		expect(await page.evaluate(() => (window as unknown as { __pwned?: number }).__pwned)).toBeUndefined();
		const reference = (await page.locator('.ref').textContent())!.match(/KB\d+/)![0];

		await page.getByRole('button', { name: 'Send for review' }).click();
		await expect(page.locator('.ref')).toContainText('In review');
		// Agents write; publishing is for those who publish.
		await expect(page.getByRole('button', { name: 'Publish' })).toHaveCount(0);

		const admin = await as(browser, 'admin');
		await admin.page.goto('/kb?status=review');
		await admin.page.getByRole('link', { name: new RegExp(title) }).click();
		await admin.page.getByRole('button', { name: 'Publish', exact: true }).click();
		await expect(admin.page.locator('.ref')).not.toContainText('In review');

		// A requester finds it, with the match highlighted, and reads it but cannot change it.
		const lena = await as(browser, 'lena');
		await lena.page.goto('/tickets');
		await lena.page.getByRole('navigation', { name: 'Sections' }).first().getByRole('link', { name: 'Knowledge' }).click();
		await lena.page.getByLabel('Search articles').fill(`${id} paper`);
		await lena.page.getByRole('button', { name: 'Search' }).click();
		const hit = lena.page.getByRole('list', { name: 'Articles' }).locator('li', { hasText: title });
		await expect(hit.locator('mark').first()).toBeVisible();
		// Regressed: snippets once showed the Markdown marks (**tray 2**) as typed.
		await expect(hit).toContainText('tray 2');
		await expect(hit).not.toContainText('**');
		await hit.getByRole('link').click();
		await expect(lena.page.getByRole('heading', { level: 1 })).toHaveText(title);
		await expect(lena.page.getByRole('link', { name: 'Edit' })).toHaveCount(0);

		// Filing a ticket about it, she is offered the article.
		await lena.page.getByRole('button', { name: 'New ticket' }).click();
		const dialog = lena.page.getByRole('dialog', { name: 'New ticket' });
		await dialog.getByLabel('Summary').fill(`My printer has a ${id} jam again`);
		await expect(dialog.getByRole('region', { name: 'Articles that might help' }).getByRole('link', { name: title })).toBeVisible();
		await dialog.getByRole('button', { name: 'Cancel' }).click();

		// An agent links it to her ticket, and she sees it there.
		const t = await createTicket(lena.context, { title: `Printer stuck ${id}` });
		await openTicket(page, t.number);
		const section = page.getByRole('region', { name: 'Articles' });
		await section.getByLabel('Link an article').fill(`${id} jam`);
		await section.getByRole('button', { name: `Link ${reference}` }).click();
		await expect(section.getByRole('link', { name: new RegExp(title) })).toBeVisible();
		await openTicket(lena.page, t.number);
		await expect(lena.page.getByRole('region', { name: 'Articles' }).getByRole('link', { name: new RegExp(title) })).toBeVisible();
	});

	test('an agent writes an article from a resolved ticket, and it starts with the ticket\'s problem and fix', async ({ browser }) => {
		const id = uid();
		const agent = await as(browser, 'agent');
		const t = await createTicket(agent.context, { title: `VPN drops every hour ${id}`, description: 'The tunnel resets at :00.' });
		const fix = `Renew the ${id} certificate.`;
		expect((await agent.context.request.patch(`/api/v1/tickets/${t.id}`, { data: { resolution: fix } })).ok()).toBeTruthy();
		expect((await agent.context.request.post(`/api/v1/tickets/${t.id}/transitions`, { data: { to: 'resolved' } })).ok()).toBeTruthy();

		await openTicket(agent.page, t.number);
		await agent.page.getByRole('link', { name: 'Write an article from this ticket' }).click();
		const editor = agent.page.getByRole('form', { name: 'New article' });
		await expect(editor.getByLabel('Title')).toHaveValue(`VPN drops every hour ${id}`);
		await expect(editor.getByLabel('Article text (Markdown)')).toHaveValue(/The tunnel resets at :00\.[\s\S]*## Fix[\s\S]*Renew the/);
		await editor.getByRole('button', { name: 'Save draft' }).click();
		await expect(agent.page.getByRole('heading', { level: 1 })).toHaveText(`VPN drops every hour ${id}`);
		// It is linked to the ticket it came from.
		await expect(agent.page.getByRole('region', { name: 'Linked tickets' })).toContainText(t.reference);
	});
});
