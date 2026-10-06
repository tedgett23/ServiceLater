import { expect, test } from '@playwright/test';
import { as, createTicket, details, openTicket, uid } from './helpers';
import { servicelater, tempApp } from './apps';

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
		servicelater('install', app.dir);
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
