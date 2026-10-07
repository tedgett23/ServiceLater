import { expect, test } from '@playwright/test';
import { api, as, createTicket, details, openTicket, uid } from './helpers';
import { USERS } from './users';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { APP_PERMISSIONS, bumpVersion, installApp, installFirstParty, modifiedIncidentApp, servicelater, tempApp, writeLocalOverride } from './apps';

// These tests change the installed apps, so they put the first-party versions back afterwards.
test.afterEach(() => {
	installFirstParty();
});

test('changing an app updates screens that are already open, with no reload', async ({ browser }) => {
	const { context, page } = await as(browser, 'agent');
	const t = await createTicket(context, { title: `Live flow ${uid()}` });
	await openTicket(page, t.number);
	await expect(page.getByRole('button', { name: 'Start work' })).toBeVisible();
	await expect(details(page).getByText('New', { exact: true })).toBeVisible();

	// An operator ships a new version of the app: new label and a new button name.
	const app = modifiedIncidentApp({
		'app.yaml': bumpVersion('0.2.1'),
		'default/lifecycles/incident.yaml': (text) =>
			text.replace('label: New,', 'label: Fresh,').replace('verb: Start work', 'verb: Pick it up')
	});
	try {
		installApp(app.dir);
		// The open page picks it up through the live connection.
		await expect(page.getByRole('button', { name: 'Pick it up' })).toBeVisible();
		await expect(details(page).getByText('Fresh', { exact: true })).toBeVisible();
		await expect(page.getByRole('button', { name: 'Start work' })).toHaveCount(0);

		// And the changed flow really works end to end.
		await page.getByRole('button', { name: 'Pick it up' }).click();
		await expect(details(page).getByText('In progress')).toBeVisible();
	} finally {
		app.cleanup();
	}

	// Putting the first-party version back restores the original wording, live again.
	installFirstParty();
	await expect(details(page).getByText('In progress')).toBeVisible();
	await openTicket(page, (await createTicket(context, { title: `After restore ${uid()}` })).number);
	await expect(page.getByRole('button', { name: 'Start work' })).toBeVisible();
});

test('an update that would strand tickets is refused, and nothing changes', async ({ browser }) => {
	const { context } = await as(browser, 'agent');
	const t = await createTicket(context, { title: `On hold ${uid()}` });
	await api(context, 'post', `/api/v1/tickets/${t.id}/transitions`, { to: 'on_hold' });

	// A version of the app with no on_hold state, while a ticket is on hold.
	const app = modifiedIncidentApp({
		'app.yaml': bumpVersion('0.3.0'),
		'default/lifecycles/incident.yaml': (text) =>
			text
				.replace(/^  on_hold:.*\n/m, '')
				.replace(/^.*to: on_hold.*\n/gm, '')
				.replace(/^.*from: on_hold.*\n/gm, '')
	});
	try {
		expect(() => installApp(app.dir)).toThrow(/on_hold.*renamed_from|renamed_from.*on_hold/s);
	} finally {
		app.cleanup();
	}

	// The installed lifecycle is untouched.
	const res = await context.request.get('/api/v1/lifecycles/incident');
	const lifecycle = await res.json();
	expect(lifecycle.version).toBe('0.2.0');
	expect(lifecycle.states.map((s: { id: string }) => s.id)).toContain('on_hold');
});

test('uninstalling the incident app leaves tickets working, and they can still be closed', async ({ browser }) => {
	const { context, page } = await as(browser, 'agent');
	const t = await createTicket(context, { title: `Orphan ${uid()}` });
	await api(context, 'post', `/api/v1/tickets/${t.id}/transitions`, { to: 'in_progress' });
	await openTicket(page, t.number);
	await expect(details(page).getByText('In progress')).toBeVisible();

	servicelater('uninstall', 'incident-management');
	// The page falls back to the built-in flow, live: a stranded ticket can only be closed.
	await expect(page.getByRole('button', { name: 'Close' })).toBeVisible();
	await expect(page.getByRole('button', { name: 'Resolve' })).toHaveCount(0);
	await page.getByRole('button', { name: 'Close' }).click();
	await expect(details(page).getByText('Closed')).toBeVisible();
});

test('the server installed its bundled apps at startup', async ({ browser }) => {
	const { context } = await as(browser, 'agent');
	const lifecycle = await (await context.request.get('/api/v1/lifecycles/incident')).json();
	expect(lifecycle.app).toBe('incident-management');
	expect(lifecycle.states.map((s: { id: string }) => s.id)).toEqual(['new', 'in_progress', 'on_hold', 'resolved', 'closed']);
});

// An operator's own changes live outside the binary, in a local layer that upgrades never touch.
test('a local change to a bundled app applies live, and removing it restores the shipped app', async ({ browser }) => {
	const { context, page } = await as(browser, 'agent');
	const t = await createTicket(context, { title: `Local layer ${uid()}` });
	await openTicket(page, t.number);
	await expect(details(page).getByText('New', { exact: true })).toBeVisible();

	// Change one label and one button. Everything else must stay as shipped.
	const remove = writeLocalOverride(
		'incident-management',
		'lifecycles/incident.yaml',
		'states:\n  new:\n    label: Backlog\n  in_progress:\n    label: Working\n'
	);
	try {
		servicelater('sync');
		await expect(details(page).getByText('Backlog', { exact: true })).toBeVisible();
		await page.getByRole('button', { name: 'Start work' }).click(); // the shipped button is untouched
		await expect(details(page).getByText('Working', { exact: true })).toBeVisible();
	} finally {
		remove();
	}

	servicelater('sync');
	await expect(details(page).getByText('In progress', { exact: true })).toBeVisible();
});

test('a guard blocks a move, says why, and lifts as soon as the ticket is fixed', async ({ browser }) => {
	const { context, page } = await as(browser, 'agent');
	const t = await createTicket(context, { title: `Guarded ${uid()}` });
	await openTicket(page, t.number);
	const start = page.getByRole('button', { name: 'Start work' });
	await expect(start).toBeEnabled();

	// A new release of the app: starting work now needs an assignee.
	const app = modifiedIncidentApp({
		'app.yaml': bumpVersion('0.3.0'),
		'default/lifecycles/incident.yaml': (text) =>
			text.replace(
				/verb: Start work,\s+primary: true \}/,
				"verb: Start work, primary: true, when: 'has(record.assignee_id)', error: Assign the incident before starting work. }"
			)
	});
	try {
		installApp(app.dir);

		// The open page learns of it live: the button is disabled and the reason sits under it.
		await expect(start).toBeDisabled();
		await expect(page.getByText('Assign the incident before starting work.')).toBeVisible();

		// Asking anyway is refused by the server with the same message.
		const refused = await context.request.post(`/api/v1/tickets/${t.id}/transitions`, { data: { to: 'in_progress' } });
		expect(refused.status()).toBe(422);
		expect(await refused.text()).toContain('Assign the incident before starting work.');

		// Fixing the ticket lifts the block, without reloading.
		await page.getByRole('button', { name: 'Assign to me' }).click();
		await expect(start).toBeEnabled();
		await expect(page.getByText('Assign the incident before starting work.')).toHaveCount(0);
		await start.click();
		await expect(details(page).getByText('In progress', { exact: true })).toBeVisible();
	} finally {
		app.cleanup();
	}
});

test('an app with a mistake in a guard is refused before it can do any harm', async () => {
	const app = modifiedIncidentApp({
		'app.yaml': bumpVersion('0.4.0'),
		'default/lifecycles/incident.yaml': (text) =>
			text.replace(/verb: Start work,\s+primary: true \}/, "verb: Start work, primary: true, when: 'record.prioity <= 2', error: Nope }")
	});
	try {
		expect(() => installApp(app.dir)).toThrow(/undefined field 'prioity'/);
	} finally {
		app.cleanup();
	}
});

// The design's own example of a guard: resolving needs resolution notes. The notes live in the incident's own table,
// yet the guard reads them as an ordinary field of the ticket.
test('a guard can require resolution notes, and writing them unlocks the move', async ({ browser }) => {
	const { context, page } = await as(browser, 'agent');
	const t = await createTicket(context, { title: `Needs notes ${uid()}` });
	await api(context, 'post', `/api/v1/tickets/${t.id}/transitions`, { to: 'in_progress' });
	await openTicket(page, t.number);
	const resolve = page.getByRole('button', { name: 'Resolve' });
	await expect(resolve).toBeEnabled();

	const app = modifiedIncidentApp({
		'app.yaml': bumpVersion('0.5.0'),
		'default/lifecycles/incident.yaml': (text) =>
			text.replace(
				/verb: Resolve,\s+primary: true \}/,
				"verb: Resolve, primary: true, when: 'size(record.resolution) > 0', error: Add resolution notes before resolving. }"
			)
	});
	try {
		installApp(app.dir);
		await expect(resolve).toBeDisabled();
		await expect(page.getByText('Add resolution notes before resolving.')).toBeVisible();

		const notes = page.getByLabel('Resolution notes');
		await notes.fill('Rebooted the switch.');
		await notes.blur();
		await expect(resolve).toBeEnabled(); // the save's response already carries the new moves
		await expect(page.getByText('Add resolution notes before resolving.')).toHaveCount(0);

		await resolve.click();
		await expect(details(page).getByText('Resolved', { exact: true })).toBeVisible();
	} finally {
		app.cleanup();
	}
});

test('a rule that says no shows its own message, and the edit is put back', async ({ browser }) => {
	const { context, page } = await as(browser, 'agent');
	const tag = uid();
	const t = await createTicket(context, { title: `Rule ${tag}` });
	await openTicket(page, t.number);

	// The rule only matches this test's ticket: the server is shared, so it must not touch anyone else's.
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'sl-rules-'));
	fs.mkdirSync(path.join(dir, 'default/rules'), { recursive: true });
	fs.writeFileSync(
		path.join(dir, 'app.yaml'),
		'apiVersion: 1\nname: e2e-house-rules\nversion: 1.0.0\nauthor: e2e\ndescription: "Rules for a test."\n' + APP_PERMISSIONS
	);
	fs.writeFileSync(
		path.join(dir, 'default/rules/critical.yaml'),
		`kind: rule
name: critical-needs-assignee
record_type: incident
timing: before
on: [update]
when: 'record.title.contains("${tag}") && record.priority == 1 && !has(record.assignee_id)'
run:
  - action: abort
    message: Critical incidents need an assignee.
`
	);
	try {
		installApp(dir);
		const before = await details(page).getByLabel('Priority').inputValue();
		await details(page).getByLabel('Priority').selectOption('Critical');
		await expect(page.getByText('Critical incidents need an assignee.')).toBeVisible();
		await expect(details(page).getByLabel('Priority')).toHaveValue(before);

		// With an assignee the same edit is fine.
		await details(page).getByLabel('Assignee').selectOption({ label: USERS.agent.name });
		await details(page).getByLabel('Priority').selectOption('Critical');
		await page.reload();
		await expect(details(page).getByLabel('Priority')).toHaveValue('1');
	} finally {
		servicelater('uninstall', 'e2e-house-rules');
		fs.rmSync(dir, { recursive: true, force: true });
	}
});

test('an async rule adds its note after the save, and the open page shows it live', async ({ browser }) => {
	const { context, page } = await as(browser, 'agent');
	const tag = uid();
	const t = await createTicket(context, { title: `Async ${tag}` });
	await openTicket(page, t.number);

	// Scoped to this test's ticket: the server is shared.
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'sl-async-'));
	fs.mkdirSync(path.join(dir, 'default/rules'), { recursive: true });
	fs.writeFileSync(
		path.join(dir, 'app.yaml'),
		'apiVersion: 1\nname: e2e-async-rules\nversion: 1.0.0\nauthor: e2e\ndescription: "Rules for a test."\n' + APP_PERMISSIONS
	);
	fs.writeFileSync(
		path.join(dir, 'default/rules/escalated.yaml'),
		`kind: rule
name: escalation-note
record_type: incident
timing: async
on: [update]
when: 'record.title.contains("${tag}") && record.priority == 1 && old.priority != 1'
run:
  - action: comment.add
    body: '=record.reference + " was escalated to critical by " + user.name'
`
	);
	try {
		installApp(dir);
		await details(page).getByLabel('Priority').selectOption('Critical');
		// Nothing reloads the page: the worker writes the note after the save, and the live connection delivers it.
		await expect(page.locator('.activity li', { hasText: 'was escalated to critical by' })).toBeVisible();
		await page.reload();
		const note = page.locator('.activity li', { hasText: `INC${t.number} was escalated` });
		await expect(note).toHaveCount(1);
		// Written by the app (it runs as its own principal), for the person whose change set it off. Regressed once: the
		// space before "for" was trimmed, so it read "e2e-async-rulesfor Priya".
		await expect(note).toContainText(`e2e-async-rules for ${USERS.agent.name}`);
		await expect(note).toContainText(`escalated to critical by ${USERS.agent.name}`);
	} finally {
		servicelater('uninstall', 'e2e-async-rules');
		fs.rmSync(dir, { recursive: true, force: true });
	}
});

test('an async rule can change the ticket, and the open page shows the new assignee live', async ({ browser }) => {
	const { context, page } = await as(browser, 'agent');
	const tag = uid();
	const t = await createTicket(context, { title: `Auto assign ${tag}` });
	await openTicket(page, t.number);
	await expect(details(page).getByLabel('Assignee').locator('option:checked')).toHaveText('Unassigned');

	// Scoped to this test's ticket: the server is shared.
	const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'sl-update-'));
	fs.mkdirSync(path.join(dir, 'default/rules'), { recursive: true });
	fs.writeFileSync(
		path.join(dir, 'app.yaml'),
		'apiVersion: 1\nname: e2e-update-rules\nversion: 1.0.0\nauthor: e2e\ndescription: "Rules for a test."\n' + APP_PERMISSIONS
	);
	fs.writeFileSync(
		path.join(dir, 'default/rules/claim.yaml'),
		`kind: rule
name: critical-claims-the-ticket
record_type: incident
timing: async
on: [update]
when: 'record.title.contains("${tag}") && record.priority == 1 && old.priority != 1'
run:
  - action: record.update
    field: assignee_id
    value: =user.id
`
	);
	try {
		installApp(dir);
		await details(page).getByLabel('Priority').selectOption('Critical');
		// The worker saves the assignee after the priority change committed; nothing reloads the page.
		await expect(details(page).getByLabel('Assignee').locator('option:checked')).toHaveText(USERS.agent.name);
		await page.reload();
		await expect(details(page).getByLabel('Priority')).toHaveValue('1');
		await expect(details(page).getByLabel('Assignee').locator('option:checked')).toHaveText(USERS.agent.name);
	} finally {
		servicelater('uninstall', 'e2e-update-rules');
		fs.rmSync(dir, { recursive: true, force: true });
	}
});

// Regressed: a live update that arrived while the agent's own edit was still in flight was thrown away, and then the
// edit's response (which predates it) overwrote the page. Here a rule's follow-up change commits while the PATCH
// response is held back, so the race happens every time instead of one run in three.
test('a change made by a rule while your own edit is in flight is not lost', async ({ browser }) => {
	const { context, page } = await as(browser, 'agent');
	const tag = uid();
	const t = await createTicket(context, { title: `Race ${tag}` });
	const app = tempApp(`e2e-race-${tag}`, {
		'default/rules/claim.yaml': `kind: rule
name: claim-when-critical
record_type: incident
timing: async
on: [update]
when: 'record.title.contains("${tag}") && record.priority == 1 && old.priority != 1'
run:
  - action: record.update
    field: assignee_id
    value: =user.id
`
	});
	try {
		installApp(app.dir);
		await openTicket(page, t.number);
		// Hold the edit's response back long enough for the worker to finish first.
		// The server handles the edit at once, and the response then waits: the worker's change commits in the meantime.
		await page.route(/\/api\/v1\/tickets\/[^/]+$/, async (route) => {
			if (route.request().method() !== 'PATCH') return route.continue();
			const response = await route.fetch();
			await new Promise((r) => setTimeout(r, 1500));
			await route.fulfill({ response });
		});
		await details(page).getByLabel('Priority').selectOption('Critical');
		await expect(details(page).getByLabel('Assignee').locator('option:checked')).toHaveText(USERS.agent.name);
	} finally {
		servicelater('uninstall', `e2e-race-${tag}`);
		app.cleanup();
	}
});
