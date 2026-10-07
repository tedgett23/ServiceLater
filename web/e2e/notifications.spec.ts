import { expect, test } from '@playwright/test';
import { api, as, createTicket, details, openTicket, uid } from './helpers';
import { installApp, servicelater, tempApp } from './apps';

const startRule = (tag: string, workflow: string) => `kind: rule
name: start-${workflow}
record_type: incident
timing: after
on: [update]
when: 'record.title.contains("${tag}") && record.priority == 1 && old.priority != 1'
run:
  - action: workflow.start
    workflow: ${workflow}
`;

const askFlow = (name: string, approverID: string) => `kind: workflow
name: ${name}
record_type: incident
start: ask
steps:
  ask:
    type: approval
    approvers: '["${approverID}"]'
    next: done
  done:
    type: end
`;

const bell = (page: import('@playwright/test').Page) => page.locator('.bell summary');

// Regression: the header used to poll every 5s for approvals with no notification of its own (see approvals slice).
// This is the badge that replaced the poll.
test('a pending approval shows up as a live notification, and reading it clears the badge', async ({ browser }) => {
	const agent = await as(browser, 'agent');
	const lena = await as(browser, 'lena');
	const tag = uid();

	const me = await lena.context.request.get('/api/v1/auth/me');
	const lenaID = (await me.json()).id as string;

	// Lena is already sitting on a page before the approval exists, to prove the badge appears live, not on load.
	// (Other tests may have left lena a read notification of her own, so this does not assume the badge starts hidden.)
	await lena.page.goto('/tickets');
	await expect(bell(lena.page)).toBeVisible();

	const t = await createTicket(agent.context, { title: `Needs a yes ${tag}` });
	const app = tempApp(`e2e-notify-${tag}`, {
		'default/workflows/ask.yaml': askFlow('ask', lenaID),
		'default/rules/start.yaml': startRule(tag, 'ask')
	});
	try {
		installApp(app.dir);
		await openTicket(agent.page, t.number);
		await details(agent.page).getByLabel('Priority').selectOption('Critical');

		await expect(lena.page.locator('.bell .badge')).toBeVisible({ timeout: 15_000 });
		await bell(lena.page).click();
		// Newest first, so this one is hers: other tests' notifications to lena are already read by now.
		const item = lena.page.locator('.bell li').first().locator('a');
		await expect(item).toHaveClass(/unread/);
		await expect(item).toContainText('Something needs your approval');

		await item.click();
		await expect(lena.page).toHaveURL(/\/approvals/);
		await bell(lena.page).click();
		await expect(lena.page.locator('.bell li').first().locator('a')).not.toHaveClass(/unread/);
	} finally {
		servicelater('uninstall', `e2e-notify-${tag}`);
		app.cleanup();
	}
});

// core-itsm's notify-assignee rule and ticket-assigned template, end to end.
test('being assigned a ticket lights the bell with what it is, and opening the ticket clears it', async ({ browser }) => {
	const agent = await as(browser, 'agent');
	const tomas = await as(browser, 'agent2');
	const tag = uid();
	const tomasID = (await (await tomas.context.request.get('/api/v1/auth/me')).json()).id as string;
	await tomas.page.goto('/tickets');
	await expect(bell(tomas.page)).toBeVisible();

	// Assigned while he is looking: it arrives live, says which ticket, and leads to it.
	const first = await createTicket(agent.context, { title: `Router reboot ${tag}` });
	await api(agent.context, 'patch', `/api/v1/tickets/${first.id}`, { assignee_id: tomasID });
	await bell(tomas.page).click();
	const item = tomas.page.locator('.bell li a', { hasText: `${first.reference} was assigned to you` });
	await expect(item).toHaveClass(/unread/, { timeout: 15_000 });
	await expect(item).toContainText(`Router reboot ${tag}`);
	await item.click();
	await expect(tomas.page).toHaveURL(new RegExp(`/tickets/${first.reference}$`));
	await expect(tomas.page).toHaveTitle(new RegExp(`Router reboot ${tag}`));

	// Opening the ticket some other way answers its notification too.
	const second = await createTicket(agent.context, { title: `Badge reader ${tag}` });
	await api(agent.context, 'patch', `/api/v1/tickets/${second.id}`, { assignee_id: tomasID });
	const other = tomas.page.locator('.bell li a', { hasText: `${second.reference} was assigned to you` });
	await bell(tomas.page).click();
	await expect(other).toHaveClass(/unread/, { timeout: 15_000 });
	await bell(tomas.page).click();
	await openTicket(tomas.page, second.number);
	await bell(tomas.page).click();
	await expect(other).not.toHaveClass(/unread/);

	// Priya, who did the assigning, was told nothing.
	const mine = (await (await agent.context.request.get('/api/v1/notifications')).json()).items as { task_id?: string }[];
	expect(mine.filter((n) => n.task_id === first.id || n.task_id === second.id)).toEqual([]);
});
