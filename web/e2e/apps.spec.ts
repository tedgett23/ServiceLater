import { expect, test } from '@playwright/test';
import { api, as, createTicket, details, openTicket, uid } from './helpers';
import { bumpVersion, installFirstParty, modifiedIncidentApp, servicelater, writeLocalOverride } from './apps';

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
		'app.yaml': bumpVersion('0.1.1'),
		'default/lifecycles/incident.yaml': (text) =>
			text.replace('label: New,', 'label: Fresh,').replace('verb: Start work', 'verb: Pick it up')
	});
	try {
		servicelater('install', app.dir);
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
		'app.yaml': bumpVersion('0.2.0'),
		'default/lifecycles/incident.yaml': (text) =>
			text
				.replace(/^  on_hold:.*\n/m, '')
				.replace(/^.*to: on_hold.*\n/gm, '')
				.replace(/^.*from: on_hold.*\n/gm, '')
	});
	try {
		expect(() => servicelater('install', app.dir)).toThrow(/on_hold.*renamed_from|renamed_from.*on_hold/s);
	} finally {
		app.cleanup();
	}

	// The installed lifecycle is untouched.
	const res = await context.request.get('/api/v1/lifecycles/incident');
	const lifecycle = await res.json();
	expect(lifecycle.version).toBe('0.1.0');
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
		servicelater('install', app.dir);

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
		expect(() => servicelater('install', app.dir)).toThrow(/undefined field 'prioity'/);
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
		servicelater('install', app.dir);
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
