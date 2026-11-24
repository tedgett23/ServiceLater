import { as, expect, test, uid } from './helpers';
import { USERS } from './users';

// Admin → Clone in: a package (made here through Admin → Clones, standing in for production's) is uploaded and
// previewed with its passphrase. This instance is marked production, so the preview says it refuses clones and offers
// no way to apply; allowing it brings the confirmation, which only enables the button for the instance's name. A person
// is put on the keep list and taken off. It never applies (the database is shared with every other test; loading is
// covered by internal/api/clone_in_test.go and internal/clone), and in finally it deletes what it made and puts the
// setting back.
test('an admin uploads a clone package and previews it', async ({ browser }) => {
	const admin = await as(browser, 'admin');
	const { page } = admin;
	const r = admin.context.request;
	const profile = `e2e-${uid()}`;
	let loadID = '';
	try {
		expect((await r.post('/api/v1/clones/profiles', { data: { name: profile, config: { records: 'open' } } })).status()).toBe(201);
		const made = await (await r.post('/api/v1/clones/packages', { data: { profile, passphrase: 'correct horse battery', destination: 'folder' } })).json();
		let file: Buffer | null = null;
		for (let i = 0; i < 40 && !file; i++) {
			const res = await r.get(`/api/v1/clones/packages/${made.id}/file`);
			if (res.ok()) file = await res.body();
			else await page.waitForTimeout(500);
		}
		expect(file).not.toBeNull();

		await page.goto('/admin/clone-in');
		await expect(page.getByRole('heading', { level: 1, name: 'Clone in' })).toBeVisible();
		await page.locator('input[type=file][accept=".slclone"]').setInputFiles({ name: 'prod.slclone', mimeType: 'application/octet-stream', buffer: file! });
		const packages = page.getByRole('region', { name: 'Packages' });
		const row = packages.locator('li').filter({ hasText: profile }).first();
		await expect(row).toContainText('Not applied');
		loadID = ((await (await r.get('/api/v1/clone-in')).json()) as { loads: { id: string; source: { profile: string } }[] }).loads.find(
			(l) => l.source.profile === profile
		)!.id;

		await row.getByRole('button', { name: /^Preview and apply/ }).click();
		const form = row.getByRole('form', { name: 'Apply this package' });
		await form.getByLabel('Passphrase').fill('wrong horse battery');
		await form.getByRole('button', { name: 'Preview' }).click();
		await expect(page.getByRole('alert')).toContainText('passphrase');

		await form.getByLabel('Passphrase').fill('correct horse battery');
		await form.getByRole('button', { name: 'Preview' }).click();
		const preview = form.getByRole('region', { name: 'Preview' });
		await expect(preview).toContainText('refuses clones');
		await expect(preview).toContainText(USERS.admin.name); // whoever applies it is kept
		await expect(preview.getByRole('button', { name: 'Replace this instance’s records' })).toHaveCount(0);

		await page.getByLabel('Allow clones into this production instance').check();
		await expect(page.getByLabel('Allow clones into this production instance')).toBeChecked();
		await form.getByRole('button', { name: 'Preview' }).click();
		const go = preview.getByRole('button', { name: 'Replace this instance’s records' });
		await expect(go).toBeDisabled();
		await preview.getByLabel(/to confirm/).fill('something else');
		await expect(go).toBeDisabled();
		const word = await preview.locator('label[for=ci-confirm] strong').innerText();
		await preview.getByLabel(/to confirm/).fill(word);
		await expect(go).toBeEnabled();
		await form.getByRole('button', { name: 'Cancel' }).click();

		const keep = page.getByRole('region', { name: 'People kept on a clone' });
		await keep.getByLabel('Keep someone').fill(USERS.agent2.name.split(' ')[0]);
		await keep.getByRole('button', { name: new RegExp(USERS.agent2.name) }).click();
		await expect(keep.getByText(USERS.agent2.email)).toBeVisible();
		await keep.getByRole('button', { name: `Stop keeping ${USERS.agent2.name}` }).click();
		await expect(keep.getByText(USERS.agent2.email)).toHaveCount(0);
	} finally {
		const now = (await (await r.get('/api/v1/clone-in')).json()) as { settings: object };
		await r.put('/api/v1/clone-in/settings', { data: { ...now.settings, allow_into_production: false } });
		if (loadID) await r.delete(`/api/v1/clone-in/${loadID}`);
		const list = (await (await r.get('/api/v1/clones')).json()) as { packages: { id: string; profile: string }[] };
		for (const p of list.packages.filter((p) => p.profile === profile)) await r.delete(`/api/v1/clones/packages/${p.id}`);
		await r.delete(`/api/v1/clones/profiles/${profile}`);
	}
});
