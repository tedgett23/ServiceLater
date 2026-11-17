import { as, createTicket, expect, openTicket, test, uid } from './helpers';

// Regressions found by looking at real screenshots.

test('on a phone the header fits and the ticket reads problem, then actions, then thread', async ({ browser }) => {
	const { context, page } = await as(browser, 'agent', { viewport: { width: 390, height: 844 } });
	const t = await createTicket(context, { title: `Phone ${uid()}`, description: 'Nothing loads.' });

	await page.goto('/tickets');
	const newTicket = page.getByRole('button', { name: 'New ticket' });
	await expect(newTicket).toBeVisible();
	expect((await newTicket.boundingBox())!.height).toBeLessThan(48); // one line, not wrapped onto two

	await openTicket(page, t.number);
	const y = async (sel: string) => (await page.locator(sel).first().boundingBox())!.y;
	expect(await y('.desc')).toBeLessThan(await y('.meta'));
	expect(await y('.meta')).toBeLessThan(await y('.activity'));

	// One pane at a time: the back link returns to the queue.
	await page.getByRole('link', { name: 'Queue', exact: true }).click();
	await expect(page).toHaveURL(/\/tickets$/);
	await expect(page.getByLabel('Search ticket titles')).toBeVisible();
});

test('in dark mode the app bar stays dark so the wordmark is readable', async ({ browser }) => {
	const { page } = await as(browser, 'agent', { colorScheme: 'dark' });
	await page.goto('/tickets');
	const bar = page.locator('header').first();
	await expect(bar).toBeVisible();
	const [r, g, b] = (await bar.evaluate((el) => getComputedStyle(el).backgroundColor)).match(/\d+/g)!.map(Number);
	expect((0.2126 * r + 0.7152 * g + 0.0722 * b) / 255).toBeLessThan(0.15);
});

test('the sign-in wordmark is not clipped on a narrow screen', async ({ browser }) => {
	const context = await browser.newContext({ viewport: { width: 360, height: 700 } });
	const page = await context.newPage();
	await page.goto('/login');
	const title = page.getByRole('heading', { name: 'ServiceLater' });
	await expect(title).toBeVisible();
	const fits = await title.evaluate((h) => {
		const stub = h.parentElement!.getBoundingClientRect();
		const range = document.createRange();
		range.selectNodeContents(h);
		return range.getBoundingClientRect().right <= stub.right + 0.5;
	});
	expect(fits).toBe(true);
	await context.close();
});

test('the app works with reduced motion and keyboard only: sign-in has visible focus', async ({ browser }) => {
	const context = await browser.newContext({ reducedMotion: 'reduce' });
	const page = await context.newPage();
	await page.goto('/login');
	await page.getByLabel('Email').focus();
	const outline = await page.getByLabel('Email').evaluate((el) => getComputedStyle(el).outlineStyle);
	expect(outline).not.toBe('none');
	await context.close();
});
