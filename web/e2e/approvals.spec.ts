import { expect, test } from '@playwright/test';
import { as, createTicket, details, openTicket, uid } from './helpers';
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
    next: after
  after:
    type: condition
    if: 'vars.ask_decision == "approved"'
    then: yes
    else: no
  yes:
    type: comment.add
    body: '="Approved: " + vars.ask_comment'
    next: done
  no:
    type: comment.add
    body: 'Rejected'
    next: done
  done:
    type: end
`;

test('an approval asks a named person, who sees it, decides it, and it leaves their inbox', async ({ browser }) => {
	const agent = await as(browser, 'agent');
	const lena = await as(browser, 'lena');
	const marcus = await as(browser, 'marcus');
	const tag = uid();

	const me = await lena.context.request.get('/api/v1/auth/me');
	const lenaID = (await me.json()).id as string;

	const t = await createTicket(agent.context, { title: `Needs a yes ${tag}` });
	const app = tempApp(`e2e-approval-${tag}`, {
		'default/workflows/ask.yaml': askFlow('ask', lenaID),
		'default/rules/start.yaml': startRule(tag, 'ask')
	});
	try {
		installApp(app.dir);
		await openTicket(agent.page, t.number);
		await details(agent.page).getByLabel('Priority').selectOption('Critical');

		// Not the addressed approver: marcus never sees it, on the ticket list's nav or the page itself.
		await marcus.page.goto('/tickets');
		await expect(marcus.page.getByRole('link', { name: /Approvals/ })).toHaveCount(0);
		await marcus.page.goto('/approvals');
		await expect(marcus.page.getByText('Nothing is waiting on you.')).toBeVisible();

		// Lena is named in the workflow: it shows up in her header and her inbox, with the ticket it is about.
		await lena.page.goto('/tickets');
		await expect(lena.page.getByRole('link', { name: 'Approvals (1)' })).toBeVisible({ timeout: 15_000 });
		await lena.page.getByRole('link', { name: 'Approvals (1)' }).click();
		const row = lena.page.locator('li', { hasText: t.reference });
		await expect(row).toBeVisible();
		await expect(row).toContainText('ask');
		await row.getByRole('textbox').fill('looks fine to me');
		await row.getByRole('button', { name: 'Approve' }).click();

		// Answered: it is gone from her inbox and the header no longer offers the link.
		await expect(lena.page.getByText('Nothing is waiting on you.')).toBeVisible();
		await expect(lena.page.getByRole('link', { name: /Approvals/ })).toHaveCount(0);

		// The workflow saw her decision and comment, and left them on the ticket.
		await expect(agent.page.locator('.activity li', { hasText: 'Approved: looks fine to me' })).toBeVisible({ timeout: 15_000 });
	} finally {
		servicelater('uninstall', `e2e-approval-${tag}`);
		app.cleanup();
	}
});
