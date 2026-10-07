import { expect, test, type Browser, type BrowserContext, type Page } from '@playwright/test';
import { PASSWORD, stateFile, USERS, type Who } from './users';

export type Ticket = { id: string; type: string; number: number; reference: string; title: string; status: string };

/** Short unique text so tests can find their own data among everyone else's. */
export const uid = () => Math.random().toString(36).slice(2, 8);

// Contexts made by these helpers are not fixtures, so Playwright would keep them (and their live connections) open
// until the whole run ends. The server allows a user ten WebSocket connections, and the agent user is in most tests, so
// unclosed contexts eventually make a page's live connection be refused ("Reconnecting..."). Close them per test.
const opened: BrowserContext[] = [];
test.afterEach(async () => {
	await Promise.all(opened.splice(0).map((c) => c.close().catch(() => {})));
});

/** A browser context that is already signed in as this user (shared saved session). */
export async function as(browser: Browser, who: Who, options: Parameters<Browser['newContext']>[0] = {}) {
	const context = await browser.newContext({ storageState: stateFile(who), ...options });
	opened.push(context);
	const page = await context.newPage();
	return { context, page };
}

/** Signs in with a brand-new session that no other test shares. Use for sign-out and token-refresh tests. */
export async function freshLogin(browser: Browser, who: Who) {
	const context = await browser.newContext();
	opened.push(context);
	const res = await context.request.post('/api/v1/auth/login', { data: { email: USERS[who].email, password: PASSWORD } });
	expect(res.ok(), 'login').toBeTruthy();
	return { context, page: await context.newPage() };
}

/** Creates a ticket through the API as the user this context is signed in as. */
export async function createTicket(
	context: BrowserContext,
	data: { type?: string; title: string; description?: string; priority?: number; requester_id?: string; change?: Record<string, unknown> }
): Promise<Ticket> {
	const res = await context.request.post('/api/v1/tickets', { data });
	expect(res.status(), 'create ticket').toBe(201);
	return res.json();
}

export async function api(context: BrowserContext, method: 'post' | 'patch', path: string, data: unknown) {
	const res = await context.request[method](path, { data });
	expect(res.ok(), `${method} ${path}`).toBeTruthy();
	return res.json();
}

/** The queue row for a ticket, found by its (unique) title. */
export const row = (page: Page, title: string) => page.locator('a.row', { hasText: title });

/** Opens a ticket (by number, which means an incident, or by reference) and waits for it to render. */
export async function openTicket(page: Page, ref: number | string) {
	await page.goto(`/tickets/${ref}`);
	await expect(page.locator('.stub')).toContainText(String(ref).replace(/^[A-Z]+/, ''));
}

/** The comment box. Staff also have a resolution notes box on an incident, so `getByRole('textbox')` is ambiguous. */
export const composer = (page: Page) => page.locator('.composer').getByRole('textbox');

/** The ticket details panel (status, priority, assignee). Scoping to it avoids matching the new-ticket dialog. */
export const details = (page: Page) => page.getByRole('complementary', { name: 'Ticket details' });
