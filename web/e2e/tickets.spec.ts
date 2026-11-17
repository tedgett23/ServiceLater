import { api, as, composer, createTicket, details, expect, openTicket, row, test, uid } from './helpers';
import { USERS } from './users';

test.describe('an agent working the queue', () => {
	test('files a ticket from the keyboard and works it through to resolved', async ({ browser }) => {
		const { page } = await as(browser, 'agent');
		const title = `Laptop won't boot ${uid()}`;

		await page.goto('/tickets');
		await expect(page.getByLabel('Search ticket titles')).toBeVisible(); // the app has mounted and is listening for keys
		await page.keyboard.press('c');
		const dialog = page.getByRole('dialog', { name: 'New ticket' });
		await expect(dialog).toBeVisible();
		await dialog.getByLabel('Summary').fill(title);
		await dialog.getByLabel('Details').fill('Black screen after the update.');
		await dialog.getByLabel('Priority').selectOption('High');
		await dialog.getByRole('button', { name: 'Create ticket' }).click();

		// Lands on the new ticket, whose number is set as the big stub.
		await expect(page.getByRole('heading', { level: 1 })).toHaveText(title);
		await expect(page.locator('.stub')).toContainText(/\d+/);
		await expect(page.getByText('Black screen after the update.')).toBeVisible();

		await page.getByRole('button', { name: 'Assign to me' }).click();
		await expect(details(page).getByLabel('Assignee')).toHaveValue(/.+/);

		// A new ticket offers "Start work" as its main action; edits apply instantly.
		await page.getByRole('button', { name: 'Start work' }).click();
		await expect(details(page).getByText('In progress')).toBeVisible();
		await page.getByRole('button', { name: 'Resolve' }).click();
		await expect(details(page).getByText('Resolved')).toBeVisible();

		// Everything survived a reload.
		await page.reload();
		await expect(details(page).getByText('Resolved')).toBeVisible();
		await expect(details(page).getByLabel('Priority')).toHaveValue('2');
		await expect(details(page).getByLabel('Assignee')).not.toHaveValue('');
	});

	test('only offers the moves the ticket can actually make', async ({ browser }) => {
		const { context, page } = await as(browser, 'agent');
		const t = await createTicket(context, { title: `Moves ${uid()}` });
		await openTicket(page, t.number);

		const actions = details(page).locator('.actions');
		await expect(actions.getByRole('button')).toHaveText(['Start work', 'Put on hold', 'Resolve', 'Close']);

		await api(context, 'post', `/api/v1/tickets/${t.id}/transitions`, { to: 'closed' });
		await page.reload();
		await expect(details(page).getByText('Closed')).toBeVisible();
		await expect(actions).toHaveCount(0); // closed is terminal
	});

	test('comments and internal notes', async ({ browser }) => {
		const { context, page } = await as(browser, 'agent');
		const t = await createTicket(context, { title: `Thread ${uid()}` });
		await openTicket(page, t.number);

		const box = composer(page);
		await box.fill('Reproduced on staging.');
		await box.press('Control+Enter'); // submit from the keyboard
		await expect(page.locator('li', { hasText: 'Reproduced on staging.' })).toHaveCount(1);
		await expect(page.locator('li.pending')).toHaveCount(0);

		// Marking a note internal changes what the button says and how the note looks.
		await page.getByLabel('Internal note').check();
		await expect(page.getByRole('button', { name: 'Add internal note' })).toBeVisible();
		await box.fill('Customer is on the old firmware.');
		await page.getByRole('button', { name: 'Add internal note' }).click();
		await expect(page.locator('li.internal', { hasText: 'old firmware' })).toBeVisible();

		await page.reload();
		await expect(page.locator('.activity li')).toHaveCount(2);
		await expect(page.locator('.activity li.internal')).toHaveCount(1);
	});

	test('changing priority and assignee sticks, and can be undone', async ({ browser }) => {
		const { context, page } = await as(browser, 'agent');
		const t = await createTicket(context, { title: `Reassign ${uid()}` });
		await openTicket(page, t.number);

		// Each change saves in the background; wait for it before reloading, or the reload can cancel it (this raced).
		const saved = () => page.waitForResponse((r) => r.url().endsWith(`/api/v1/tickets/${t.id}`) && r.request().method() === 'PATCH');
		await Promise.all([saved(), details(page).getByLabel('Priority').selectOption('Critical')]);
		await Promise.all([saved(), details(page).getByLabel('Assignee').selectOption({ label: USERS.agent2.name })]);
		await page.reload();
		await expect(details(page).getByLabel('Priority')).toHaveValue('1');
		await expect(details(page).getByLabel('Assignee').locator('option:checked')).toHaveText(USERS.agent2.name);

		await Promise.all([saved(), details(page).getByLabel('Assignee').selectOption('')]);
		await page.reload();
		await expect(details(page).getByLabel('Assignee').locator('option:checked')).toHaveText('Unassigned');
	});

	test('the queue filters and search find the right tickets', async ({ browser }) => {
		const { context, page } = await as(browser, 'agent');
		const word = `zebra${uid()}`;
		const mine = await createTicket(context, { title: `Mine ${word}` });
		const loose = await createTicket(context, { title: `Loose ${word}` });
		await api(context, 'patch', `/api/v1/tickets/${mine.id}`, {
			assignee_id: (await (await context.request.get('/api/v1/users/staff')).json()).items.find(
				(s: { email: string }) => s.email === USERS.agent.email
			).id
		});

		await page.goto('/tickets');
		await page.getByRole('button', { name: 'All', exact: true }).click();
		await page.getByLabel('Search ticket titles').fill(word);
		await expect(row(page, mine.title)).toBeVisible();
		await expect(row(page, loose.title)).toBeVisible();

		await page.getByRole('button', { name: 'Mine' }).click();
		await expect(row(page, mine.title)).toBeVisible();
		await expect(row(page, loose.title)).toHaveCount(0);

		// Unowned work is highlighted so it stands out in a scan.
		await page.getByRole('button', { name: 'Unassigned' }).click();
		await expect(row(page, loose.title).getByText('Unassigned')).toBeVisible();
		await expect(row(page, mine.title)).toHaveCount(0);

		await page.getByLabel('Search ticket titles').fill('no ticket has this title');
		await expect(page.getByText(/No tickets match/)).toBeVisible();
	});

	test('j and k move through the queue and / jumps to search', async ({ browser }) => {
		const { context, page } = await as(browser, 'agent');
		const tag = uid();
		const a = await createTicket(context, { title: `Older ${tag}` });
		const b = await createTicket(context, { title: `Newer ${tag}` });

		await page.goto('/tickets');
		await page.getByRole('button', { name: 'All', exact: true }).click();
		await page.getByLabel('Search ticket titles').fill(tag);
		await expect(row(page, a.title)).toBeVisible();
		await page.getByLabel('Search ticket titles').blur();

		await openTicket(page, b.number); // newest first, so b is above a
		await page.keyboard.press('j');
		await expect(page).toHaveURL(new RegExp(`/tickets/${a.reference}$`));
		await page.keyboard.press('k');
		await expect(page).toHaveURL(new RegExp(`/tickets/${b.reference}$`));

		await page.keyboard.press('/');
		await expect(page.getByLabel('Search ticket titles')).toBeFocused();
	});

	test('a ticket is named by its type and number, however you reach it', async ({ browser }) => {
		const { context, page } = await as(browser, 'agent');
		const t = await createTicket(context, { title: `Named ${uid()}` });
		expect(t.reference).toBe(`INC${t.number}`);

		await page.goto(`/tickets/${t.reference}`); // by reference
		await expect(page.getByRole('heading', { level: 1 })).toHaveText(t.title);
		await expect(page).toHaveTitle(new RegExp(`^${t.reference} `));
		await page.goto(`/tickets/${t.number}`); // a bare number still means an incident
		await expect(page.getByRole('heading', { level: 1 })).toHaveText(t.title);
		await page.goto(`/tickets/CHG${t.number}`); // the same number under another type is a different ticket
		await expect(page.getByText('doesn’t exist, or you don’t have access')).toBeVisible();
	});

	test('resolution notes are kept with the incident, and the requester reads them', async ({ browser }) => {
		const agent = await as(browser, 'agent');
		const lena = await as(browser, 'lena');
		const t = await createTicket(lena.context, { title: `Resolved ${uid()}` });
		await openTicket(agent.page, t.number);

		const notes = agent.page.getByLabel('Resolution notes');
		await notes.fill('Replaced the gateway certificate and restarted the tunnel.');
		await notes.blur(); // saved when you leave the box
		await agent.page.reload();
		await expect(agent.page.getByLabel('Resolution notes')).toHaveValue('Replaced the gateway certificate and restarted the tunnel.');

		// The requester sees them as text, with nothing to edit.
		await openTicket(lena.page, t.number);
		await expect(lena.page.getByRole('heading', { name: 'Resolution' })).toBeVisible();
		await expect(lena.page.getByText('Replaced the gateway certificate and restarted the tunnel.')).toBeVisible();
		await expect(lena.page.getByLabel('Resolution notes')).toHaveCount(0);
	});

	test('a missing ticket explains itself', async ({ browser }) => {
		const { page } = await as(browser, 'agent');
		await page.goto('/tickets/99999999');
		await expect(page.getByText('doesn’t exist, or you don’t have access')).toBeVisible();
	});
});
