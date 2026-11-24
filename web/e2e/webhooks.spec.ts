import { installApp, servicelater, tempApp } from './apps';
import { as, expect, test, uid } from './helpers';

// A workflow started by another system: the builder turns its webhook on, shows the address and that the secret is
// not set, calls are refused until an admin sets it, then a call with the token starts a run that files an incident
// from the body, and the builder's log shows each call. The app and its secret are removed in finally.
test('a webhook starts a workflow once its secret is set, and the builder shows its calls', async ({ browser, request }) => {
	const id = uid();
	const app = `e2e-hook-${id}`;
	const admin = await as(browser, 'admin');
	const { page } = admin;
	const files = tempApp(app, {
		'app.yaml':
			`apiVersion: 1\nname: ${app}\nversion: 1.0.0\nauthor: e2e\ndescription: "Made by an end-to-end test."\n` +
			'permissions: [records:read:incident, records:create:incident]\nconfig:\n  - { key: alert_token, label: Alert token, type: secret }\n',
		'default/workflows/alert.yaml':
			'kind: workflow\nname: alert\nrecord_type: incident\nstart: file\nsteps:\n' +
			'  file:\n    type: record.create\n    record_type: incident\n    title: =string(vars.payload.title)\n    next: done\n  done:\n    type: end\n'
	});
	const hook = `/api/v1/hooks/${app}/alert`;
	try {
		installApp(files.dir);
		await page.goto(`/workflows/${app}/alert`);
		await page.getByLabel('Also start it when another system calls it').check();
		await expect(page.getByLabel('The caller proves itself with')).toHaveValue('token');
		await expect(page.getByLabel('Address to give the other system')).toHaveValue(new RegExp(`${hook}$`));
		await page.getByRole('button', { name: 'Save', exact: true }).click();
		await expect(page.getByText('Its secret is not set yet')).toBeVisible();

		// Refused until the secret is set; the log says why.
		expect((await request.post(hook, { data: { title: 'x' }, headers: { Authorization: 'Bearer t0ken' } })).status()).toBe(401);
		const calls = page.getByRole('region', { name: 'Latest calls' });
		await calls.getByRole('button', { name: 'Refresh' }).click();
		await expect(calls.locator('li').first()).toContainText('Refused');
		await expect(calls.locator('li').first()).toContainText('secret is not set');

		expect((await admin.context.request.put(`/api/v1/connections/${app}/secrets/alert_token`, { data: { value: 't0ken' } })).status()).toBe(204);
		await page.reload();
		await expect(page.getByText('Its secret is set.')).toBeVisible();

		expect((await request.post(hook, { data: { title: 'x' }, headers: { Authorization: 'Bearer wrong' } })).status()).toBe(401);
		const title = `Disk full on db-${id}`;
		const res = await request.post(hook, { data: { title }, headers: { Authorization: 'Bearer t0ken', 'Idempotency-Key': `k-${id}` } });
		expect(res.status()).toBe(202);
		const { run_id } = (await res.json()) as { run_id: string };
		const again = await request.post(hook, { data: { title }, headers: { Authorization: 'Bearer t0ken', 'Idempotency-Key': `k-${id}` } });
		expect(again.status()).toBe(200);
		expect(((await again.json()) as { run_id: string }).run_id).toBe(run_id);

		// The started run shows in the log by itself (it tells the page), and it filed the incident.
		await expect(calls.getByRole('link', { name: 'Started a run' })).toBeVisible();
		await calls.getByRole('button', { name: 'Refresh' }).click();
		await expect(calls.locator('li').first()).toContainText('Repeat, nothing started');
		await calls.getByRole('link', { name: 'Started a run' }).click();
		await expect(page).toHaveURL(new RegExp(`/runs/${run_id}`));
		await expect(page.locator('header .meta')).toContainText('Finished');
		const found = await admin.context.request.get(`/api/v1/tickets?q=${encodeURIComponent(title)}`);
		expect(JSON.stringify(await found.json())).toContain(title);
	} finally {
		await admin.context.request.put(`/api/v1/connections/${app}/secrets/alert_token`, { data: { value: null } });
		servicelater('uninstall', app);
		files.cleanup();
	}
});
