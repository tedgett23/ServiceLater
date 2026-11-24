import { as, composer, createTicket, expect, openTicket, test, uid } from './helpers';
import { USERS } from './users';

// @mentions: an agent types @ and part of a name in the comment box, picks the person, and sends. The comment shows
// the mention, and the person mentioned is told (core-itsm's notify-mentioned), with the comment as the body; the
// requester is told of the public comment too. Removing a picked name from the text before sending drops the mention.
test('an agent mentions a colleague in a comment, and the colleague is told', async ({ browser }) => {
	const lena = await as(browser, 'lena');
	const priya = await as(browser, 'agent');
	const tomas = await as(browser, 'agent2');
	const title = `Docking station ${uid()}`;
	const tk = await createTicket(lena.context, { title });

	const { page } = priya;
	await openTicket(page, tk.reference);
	const box = composer(page);
	await box.click();
	await box.pressSequentially('Can you take this, @Tom');
	const people = page.getByRole('listbox', { name: 'People to mention' });
	await expect(people.getByRole('option', { name: /Tomás Ibarra/ })).toBeVisible();
	await page.keyboard.press('Enter');
	await expect(box).toHaveValue('Can you take this, @Tomás Ibarra ');
	await box.pressSequentially('? It is on the third floor.');
	await page.getByRole('button', { name: 'Add comment' }).click();

	const comment = page.locator('li.comment', { hasText: 'third floor' });
	await expect(comment.locator('.mention')).toHaveText('@Tomás Ibarra');
	await expect(comment).not.toContainText('Sending');

	// Tomás is told he was mentioned, with the comment's words; Lena that someone commented.
	await expect
		.poll(async () => {
			const items = (await (await tomas.context.request.get('/api/v1/notifications')).json()).items as { task_id: string; title: string; body: string }[];
			return items.filter((n) => n.task_id === tk.id).map((n) => `${n.title} | ${n.body}`);
		})
		.toEqual([`${USERS.agent.name} mentioned you on ${tk.reference} | Can you take this, @Tomás Ibarra ? It is on the third floor.`]);
	const told = (await (await lena.context.request.get('/api/v1/notifications')).json()).items as { task_id: string; title: string }[];
	expect(told.filter((n) => n.task_id === tk.id).map((n) => n.title)).toContain(`${USERS.agent.name} commented on ${tk.reference}`);

	// A name picked and then deleted from the text is not mentioned.
	await box.click();
	await box.pressSequentially('@Tom');
	await page.keyboard.press('Enter');
	await box.fill('Never mind, I have it.');
	await page.getByRole('button', { name: 'Add comment' }).click();
	const second = page.locator('li.comment', { hasText: 'Never mind' });
	await expect(second).not.toContainText('Sending');
	await expect(second.locator('.mention')).toHaveCount(0);
	const comments = (await (await priya.context.request.get(`/api/v1/tickets/${tk.id}/comments`)).json()).items as { body: string; mentions: { name: string }[] }[];
	expect(comments.map((c) => c.mentions.map((m) => m.name))).toEqual([[USERS.agent2.name], []]);
});
