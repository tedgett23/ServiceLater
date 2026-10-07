import { expect, test } from '@playwright/test';
import { as, createTicket, details, openTicket, uid } from './helpers';
import { installApp, servicelater, tempApp } from './apps';

// A rule that starts a workflow when this test's ticket is raised to Critical. Scoped to the ticket by its title tag,
// because the server is shared with every other test.
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

const napFlow = (name: string) => `kind: workflow
name: ${name}
record_type: incident
start: note
steps:
  note:
    type: set
    vars:
      reason: '="raised to " + string(record.priority)'
    next: nap
  nap:
    type: wait
    for: 1s
    next: say
  say:
    type: comment.add
    body: '="Workflow saw it: " + vars.reason'
    next: done
  done:
    type: end
`;

test('a workflow started by a rule runs to the end and can be followed in the run viewer', async ({ browser }) => {
	const { context, page } = await as(browser, 'agent');
	const tag = uid();
	const t = await createTicket(context, { title: `Flow ${tag}` });
	const app = tempApp(`e2e-flow-${tag}`, {
		'default/workflows/nap-flow.yaml': napFlow('nap-flow'),
		'default/rules/start.yaml': startRule(tag, 'nap-flow')
	});
	try {
		installApp(app.dir);
		await openTicket(page, t.number);
		await details(page).getByLabel('Priority').selectOption('Critical');

		// The workflow's own comment arrives on the open ticket, after it slept.
		await expect(page.locator('.activity li', { hasText: 'Workflow saw it: raised to 1' })).toBeVisible({ timeout: 15_000 });

		await page.getByRole('link', { name: 'Runs', exact: true }).click();
		const row = page.locator('a.row', { hasText: t.reference });
		await expect(row).toBeVisible();
		await expect(row).toContainText('nap-flow');
		await row.click();

		await expect(page.getByRole('heading', { name: 'nap-flow' })).toBeVisible();
		await expect(page.getByText('Done', { exact: true }).first()).toBeVisible();
		// Every step, in order, and what happened at each visit: it slept once and was woken.
		await expect(page.locator('.step')).toHaveCount(4);
		await expect(page.locator('.step').first()).toContainText('note');
		const history = page.locator('tbody tr');
		await expect(history).toHaveCount(5);
		await expect(history.nth(1)).toContainText('Went to sleep');
		await expect(history.nth(2)).toContainText('nap');
		await expect(page.locator('dt', { hasText: 'reason' })).toBeVisible();
		await expect(page.getByRole('link', { name: t.reference })).toBeVisible();
	} finally {
		servicelater('uninstall', `e2e-flow-${tag}`);
		app.cleanup();
	}
});

test('a run that fails says where it stopped and why', async ({ browser }) => {
	const { context, page } = await as(browser, 'agent');
	const tag = uid();
	const t = await createTicket(context, { title: `Broken ${tag}` });
	const app = tempApp(`e2e-broken-${tag}`, {
		'default/workflows/breaks.yaml': `kind: workflow
name: breaks
record_type: incident
start: first
steps:
  first:
    type: comment.add
    body: this step works
    next: second
  second:
    type: set
    vars:
      x: '=string(record.priority / (record.priority - 1))'
    next: done
  done:
    type: end
`,
		'default/rules/start.yaml': startRule(tag, 'breaks')
	});
	try {
		installApp(app.dir);
		await openTicket(page, t.number);
		await details(page).getByLabel('Priority').selectOption('Critical');
		await expect(page.locator('.activity li', { hasText: 'this step works' })).toBeVisible({ timeout: 15_000 });

		await page.goto('/runs?');
		await page.getByRole('button', { name: 'Failed' }).click();
		const row = page.locator('a.row', { hasText: t.reference });
		await expect(row).toBeVisible();
		await expect(row).toContainText('could not be evaluated');
		await row.click();

		await expect(page.getByRole('alert').filter({ hasText: 'It stopped at second.' })).toBeVisible();
		await expect(page.locator('.step.failed')).toContainText('second');
		await expect(page.locator('.step').first()).toContainText('Done');
		await expect(page.locator('tbody tr.failed')).toContainText('Failed');
		// The step before it kept its effect: its comment is on the ticket.
		await expect(page.getByRole('button', { name: 'Cancel this run' })).toHaveCount(0);
	} finally {
		servicelater('uninstall', `e2e-broken-${tag}`);
		app.cleanup();
	}
});

test('a sleeping run can be cancelled from the viewer', async ({ browser }) => {
	const { context, page } = await as(browser, 'agent');
	const tag = uid();
	const t = await createTicket(context, { title: `Cancel ${tag}` });
	const app = tempApp(`e2e-cancel-${tag}`, {
		'default/workflows/long-nap.yaml': `kind: workflow
name: long-nap
record_type: incident
start: nap
steps:
  nap:
    type: wait
    for: 1h
    next: done
  done:
    type: end
`,
		'default/rules/start.yaml': startRule(tag, 'long-nap')
	});
	try {
		installApp(app.dir);
		await openTicket(page, t.number);
		await details(page).getByLabel('Priority').selectOption('Critical');
		await page.goto('/runs');
		const row = page.locator('a.row', { hasText: t.reference });
		await expect(row).toBeVisible({ timeout: 10_000 });
		await row.click();
		await expect(page.getByText('Waiting', { exact: true }).first()).toBeVisible({ timeout: 10_000 });
		await expect(page.locator('.step.current')).toContainText('nap');

		await page.getByRole('button', { name: 'Cancel this run' }).click();
		await expect(page.getByText('Cancelled', { exact: true }).first()).toBeVisible();
		await expect(page.getByRole('button', { name: 'Cancel this run' })).toHaveCount(0);
	} finally {
		servicelater('uninstall', `e2e-cancel-${tag}`);
		app.cleanup();
	}
});

test('a requester has no workflow runs page', async ({ browser }) => {
	const { page } = await as(browser, 'lena');
	await page.goto('/tickets');
	await expect(page.getByRole('link', { name: 'Runs', exact: true })).toHaveCount(0);
	await page.goto('/runs');
	await expect(page).toHaveURL(/\/tickets$/);
});

// Regressed: the links between Tickets and Runs were first put in the app bar, which has no room for them on a phone:
// the ticket list and this page became wider than the screen and pushed "Sign out" off it. They have a bar of their own.
test('the ticket list and the runs pages fit a phone screen, and both are one tap away', async ({ browser }) => {
	const { page } = await as(browser, 'agent', { viewport: { width: 390, height: 844 } });
	const fits = () => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth);
	for (const path of ['/tickets', '/runs']) {
		await page.goto(path);
		await expect(page.getByRole('navigation', { name: 'Sections' })).toBeVisible();
		expect(await fits(), `${path} is wider than the screen`).toBe(true);
		await expect(page.getByRole('button', { name: 'Sign out' })).toBeInViewport();
	}
	await page.getByRole('link', { name: 'Tickets', exact: true }).click();
	await expect(page).toHaveURL(/\/tickets/);
	await page.getByRole('link', { name: 'Runs', exact: true }).click();
	await expect(page.getByRole('heading', { name: 'Workflow runs' })).toBeVisible();
});
