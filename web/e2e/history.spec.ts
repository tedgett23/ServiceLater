import { expect, test } from '@playwright/test';
import { as, createTicket, details, openTicket, uid } from './helpers';
import { USERS } from './users';

// The history of a record: every change, by whom, from what to what, beside the comments or on its own. The choice
// of what Activity shows is remembered in the browser.
test('a ticket shows who changed what, beside its comments or on its own', async ({ browser }) => {
	const id = uid();
	const lena = await as(browser, 'lena');
	const t = await createTicket(lena.context, { title: `Badge reader dead ${id}` });
	await lena.context.request.post(`/api/v1/tickets/${t.id}/comments`, { data: { body: `It beeps red ${id}` } });

	const { page } = await as(browser, 'agent');
	await openTicket(page, t.number);
	const activity = page.locator('section.activity');
	await activity.getByLabel('Show').selectOption('history');
	await expect(activity.locator('li.change').first()).toContainText(`${USERS.lena.name} created this incident`);
	await expect(activity).not.toContainText(`It beeps red ${id}`);

	// A change made here appears as it is made.
	await details(page).getByLabel('Priority').selectOption('1');
	await expect(activity.locator('li.change').last()).toContainText(`${USERS.agent.name} changed Priority from Medium to Critical`);

	// Everything: the comment and the changes in the order they happened.
	await activity.getByLabel('Show').selectOption('all');
	await expect(activity.locator('li')).toHaveCount(3);
	await expect(activity.locator('li').nth(1)).toContainText(`It beeps red ${id}`);
	await page.reload();
	await expect(page.locator('section.activity').getByLabel('Show')).toHaveValue('all');

	// Its requester reads the history of her own ticket.
	await openTicket(lena.page, t.number);
	await lena.page.locator('section.activity').getByLabel('Show').selectOption('history');
	await expect(lena.page.locator('section.activity li.change').last()).toContainText('changed Priority from Medium to Critical');
});

test('a configuration item shows its history', async ({ browser }) => {
	const id = uid();
	const admin = await as(browser, 'admin');
	const ci = await (await admin.context.request.post('/api/v1/cis', { data: { class: 'server', name: `hist-${id}` } })).json();
	await admin.page.goto(`/cmdb/${ci.id}`);
	await admin.page.getByLabel('Status').selectOption('maintenance');
	const history = admin.page.getByRole('region', { name: 'History' });
	await expect(history.locator('li').first()).toContainText(`${USERS.admin.name} changed Status from Active to In maintenance`);
	await expect(history.locator('li').last()).toContainText(`${USERS.admin.name} created this item`);
});

// An admin chooses what history records: with incidents left out, only their basics are recorded, and the choice
// itself is logged. Everything is recorded again afterwards, for the other tests.
test('an admin leaves incidents out of field history, and the basics are still recorded', async ({ browser }) => {
	const admin = await as(browser, 'admin');
	try {
		await admin.page.goto('/admin');
		await admin.page.getByRole('navigation', { name: 'Admin' }).getByRole('link', { name: 'History' }).click();
		const form = admin.page.getByRole('form', { name: 'What is recorded' });
		await form.getByLabel('Every record type, including ones added later').uncheck();
		await form.getByLabel('Incident').uncheck();
		await form.getByRole('button', { name: 'Save' }).click();
		await expect(admin.page.getByRole('region', { name: 'Changes to these settings' })).toContainText(`${USERS.admin.name} set Record types to`);

		const id = uid();
		const agent = await as(browser, 'agent');
		const t = await createTicket(agent.context, { title: `Quiet ${id}` });
		await openTicket(agent.page, t.number);
		await details(agent.page).getByLabel('Priority').selectOption('1');
		await details(agent.page).getByRole('button', { name: 'Start work' }).click();
		const activity = agent.page.locator('section.activity');
		await activity.getByLabel('Show').selectOption('history');
		await expect(activity.locator('li.change').last()).toContainText('changed Status from New to In progress');
		await expect(activity).not.toContainText('Priority');
	} finally {
		await admin.context.request.put('/api/v1/settings/history', { data: { record_types: ['*'], cmdb: true } });
	}
});
