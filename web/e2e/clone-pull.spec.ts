import { as, expect, test, uid } from './helpers';

// The scheduled pull, with this instance as its own source: Admin → Clones makes a pull key for a profile (shown
// once), Admin → Clone in → Source takes this instance's address and the key, tests them, and pulls a package, which
// the worker fetches and which previews with no passphrase to type. Admin → Clones then lists the pull. It never
// applies or turns the schedule on (the database is shared); in finally it deletes the package and the profile (its
// keys go with it) and clears the source.
test('an admin pulls a clone package from the source', async ({ browser, baseURL }) => {
	const admin = await as(browser, 'admin');
	const { page } = admin;
	const r = admin.context.request;
	const profile = `e2e-pull-${uid()}`;
	const keyName = `E2E ${uid()}`;
	try {
		expect((await r.post('/api/v1/clones/profiles', { data: { name: profile, config: { records: 'open' } } })).status()).toBe(201);

		await page.goto('/admin/clones');
		const row = page.getByRole('region', { name: 'Profiles' }).locator('li').filter({ hasText: profile }).first();
		await row.getByText('Pull keys').click();
		const keys = row.getByRole('region', { name: `Pull keys for ${profile}` });
		await keys.getByLabel('Which instance will hold it').fill(keyName);
		await keys.getByRole('button', { name: 'Make a pull key' }).click();
		const shown = keys.getByRole('status');
		await expect(shown).toContainText('not shown again');
		const key = (await shown.locator('code').innerText()).trim();
		expect(key).toMatch(/^slp_/);
		await shown.getByRole('button', { name: 'Done' }).click();
		await expect(keys.getByText(keyName)).toBeVisible();
		await expect(keys).toContainText('never used');

		await page.goto('/admin/clone-in');
		const source = page.getByRole('form', { name: 'Source' });
		await source.getByLabel('Production’s address').fill(baseURL!);
		await source.getByLabel('Pull key').fill(key);
		await source.getByRole('button', { name: 'Test the connection' }).click();
		await expect(source.getByRole('status', { name: 'The source' })).toContainText(profile);
		await source.getByRole('button', { name: 'Save' }).click();
		await expect(source.getByLabel('Pull key')).toHaveAttribute('placeholder', /Set/);

		await source.getByRole('button', { name: 'Pull now' }).click();
		const packages = page.getByRole('region', { name: 'Packages' });
		const pulled = packages.locator('li').filter({ hasText: profile }).first();
		await expect(pulled).toContainText('Not applied', { timeout: 30_000 });
		await expect(pulled).toContainText('pulled');
		await pulled.getByRole('button', { name: /^Preview and apply/ }).click();
		const form = pulled.getByRole('form', { name: 'Apply this package' });
		await expect(form.getByLabel('Passphrase')).toHaveCount(0);
		await form.getByRole('button', { name: 'Preview' }).click();
		await expect(form.getByRole('region', { name: 'Preview' })).toContainText('People');
		await form.getByRole('button', { name: 'Cancel' }).click();

		await page.goto('/admin/clones');
		const pulls = page.getByRole('region', { name: 'Pulls' });
		await expect(pulls.locator('li').filter({ hasText: profile }).first()).toContainText(`key ${keyName}`);
	} finally {
		const now = (await (await r.get('/api/v1/clone-in')).json()) as {
			loads: { id: string; source: { profile: string } }[];
			source: { cron: string; timezone: string };
		};
		for (const l of now.loads.filter((l) => l.source.profile === profile)) await r.delete(`/api/v1/clone-in/${l.id}`);
		await r.put('/api/v1/clone-in/source', {
			data: { url: '', enabled: false, cron: now.source.cron, timezone: now.source.timezone, keep_people: true, keep_studio: true, clear_key: true }
		});
		await r.delete(`/api/v1/clones/profiles/${profile}`);
	}
});
