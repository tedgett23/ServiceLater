import { type WebSocketRoute } from '@playwright/test';
import { api, as, composer, createTicket, details, expect, openTicket, row, test, uid } from './helpers';

test.describe('live updates', () => {
	test('a requester sees an agent’s status change and replies without reloading', async ({ browser }) => {
		const agent = await as(browser, 'agent');
		const lena = await as(browser, 'lena');
		const t = await createTicket(lena.context, { title: `Live thread ${uid()}` });
		await openTicket(agent.page, t.number);
		await openTicket(lena.page, t.number);
		await expect(agent.page.getByText('Reconnecting')).toHaveCount(0);

		await agent.page.getByRole('button', { name: 'Start work' }).click();
		await expect(details(lena.page).getByText('In progress')).toBeVisible();

		await composer(agent.page).fill('Rolling out a fix now.');
		await agent.page.getByRole('button', { name: 'Add comment' }).click();
		// Scoped to the ticket: the bell lists the comment too (core-itsm tells the requester of a public comment).
		await expect(lena.page.getByRole('region', { name: 'Ticket', exact: true }).locator('li', { hasText: 'Rolling out a fix now.' })).toHaveCount(1);
		// The author never sees their own comment twice, even though the live event echoes back.
		await expect(agent.page.locator('li', { hasText: 'Rolling out a fix now.' })).toHaveCount(1);
	});

	test('an internal note never reaches the requester', async ({ browser }) => {
		const agent = await as(browser, 'agent');
		const lena = await as(browser, 'lena');
		const t = await createTicket(lena.context, { title: `Private note ${uid()}` });
		await openTicket(agent.page, t.number);
		await openTicket(lena.page, t.number);

		await agent.page.getByLabel('Internal note').check();
		await composer(agent.page).fill('Vendor confirmed the bug.');
		await agent.page.getByRole('button', { name: 'Add internal note' }).click();
		await expect(agent.page.locator('li.internal')).toHaveCount(1);

		// A public reply afterwards proves the socket is alive, so the absence below is meaningful.
		await api(agent.context, 'post', `/api/v1/tickets/${t.id}/comments`, { body: 'Update for you.' });
		await expect(lena.page.getByRole('region', { name: 'Ticket', exact: true }).getByText('Update for you.')).toBeVisible();
		await expect(lena.page.getByText('Vendor confirmed the bug.')).toHaveCount(0);
	});

	test('a new ticket appears in an agent’s queue as it is filed, and only for people who may see it', async ({ browser }) => {
		const agent = await as(browser, 'agent');
		const lena = await as(browser, 'lena');
		const marcus = await as(browser, 'marcus');
		const title = `Badge reader ${uid()}`;

		await agent.page.goto('/tickets');
		await agent.page.getByRole('button', { name: 'Unassigned' }).click();
		await marcus.page.goto('/tickets');
		await marcus.page.getByRole('button', { name: 'All', exact: true }).click();
		await expect(agent.page.getByRole('button', { name: 'Unassigned' })).toHaveAttribute('aria-pressed', 'true');

		await lena.page.goto('/tickets');
		await lena.page.getByRole('button', { name: 'New ticket' }).click();
		await lena.page.getByLabel('Summary').fill(title);
		await lena.page.getByRole('button', { name: 'Create ticket' }).click();

		await expect(row(agent.page, title)).toBeVisible();
		// Another requester's queue is untouched: prove liveness with a ticket of their own first.
		const own = await createTicket(marcus.context, { title: `Marcus own ${uid()}` });
		await expect(row(marcus.page, own.title)).toBeVisible();
		await expect(row(marcus.page, title)).toHaveCount(0);
	});

	test('after a dropped connection it says so, reconnects, and catches up on what it missed', async ({ browser }) => {
		const agent = await as(browser, 'agent');
		const lena = await as(browser, 'lena');
		const t = await createTicket(lena.context, { title: `Catch up ${uid()}` });

		// Sit between the page and the server so the test can swallow events and cut the connection.
		let socket: WebSocketRoute | undefined;
		let muted = false;
		await lena.page.routeWebSocket(/\/ws\/v1\/subscribe/, (ws) => {
			socket = ws;
			ws.connectToServer().onMessage((message) => {
				if (!muted) ws.send(message);
			});
		});
		await openTicket(lena.page, t.number);
		await expect.poll(() => socket !== undefined).toBe(true);

		// An event that never arrives, as if the network had dropped it.
		muted = true;
		await api(agent.context, 'post', `/api/v1/tickets/${t.id}/transitions`, { to: 'in_progress' });
		await lena.page.waitForTimeout(500);
		await expect(details(lena.page).getByText('New')).toBeVisible(); // still stale

		// The connection drops: the app says so, then recovers on its own and refetches.
		muted = false;
		await socket!.close();
		await expect(lena.page.getByRole('status').getByText('Reconnecting')).toBeVisible();
		await expect(lena.page.getByText('Reconnecting')).toHaveCount(0);
		await expect(details(lena.page).getByText('In progress')).toBeVisible();
	});

	// Regressed: on a phone the "Reconnecting…" badge sat in the app bar, which has no room for it, and pushed
	// "Sign out" off the screen while the connection was down.
	test('on a phone the app bar still fits while reconnecting', async ({ browser }) => {
		const { page } = await as(browser, 'agent', { viewport: { width: 390, height: 844 } });
		await page.routeWebSocket(/\/ws/, (ws) => ws.close());
		await page.goto('/tickets');
		await expect(page.getByRole('status').getByText('Reconnecting')).toBeVisible();
		await expect(page.getByRole('status').getByText('Reconnecting')).toBeInViewport();
		expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
		await expect(page.getByRole('button', { name: 'Sign out' })).toBeInViewport({ ratio: 1 });
	});
});
