import fs from 'node:fs';
import { as, expect, test, uid } from './helpers';

// Admin → Clones: an admin makes a profile (open records only, addresses masked), previews what it would take, saves
// it, makes a package with a passphrase, and downloads it once the worker has made it. The package's report shows
// what it holds. It deletes the package and the profile in finally.
test('an admin makes a clone profile and a package from it', async ({ browser }) => {
	const admin = await as(browser, 'admin');
	const { page } = admin;
	const name = `e2e-${uid()}`;
	try {
		await page.goto('/admin/clones');
		await expect(page.getByRole('heading', { level: 1, name: 'Clones' })).toBeVisible();
		await page.getByRole('button', { name: 'New profile' }).click();
		const editor = page.getByRole('form', { name: 'New profile' });
		await editor.getByLabel('Name', { exact: true }).fill(name);
		await editor.getByLabel('Description', { exact: true }).fill('UAT, open work only');
		await editor.getByLabel('Which').selectOption('open');
		await editor.getByLabel(/Email addresses/).check();

		// An exclusion that does not compile is refused with the reason.
		await editor.getByRole('button', { name: 'Add an exclusion' }).click();
		await editor.getByLabel('When').fill('record.no_such_field == 1');
		await editor.getByRole('button', { name: 'Preview' }).click();
		await expect(page.getByRole('alert')).toContainText('exclude 1');
		await editor.getByRole('button', { name: 'Remove exclusion 1' }).click();

		await editor.getByRole('button', { name: 'Preview' }).click();
		const preview = editor.getByRole('region', { name: 'Preview' });
		await expect(preview.getByRole('cell', { name: 'People' })).toBeVisible();
		await expect(preview).toContainText('link');
		await editor.getByRole('button', { name: 'Save' }).click();
		await expect(editor).toBeHidden();

		const profiles = page.getByRole('region', { name: 'Profiles' });
		const row = profiles.locator('li').filter({ hasText: name });
		await expect(row).toContainText('open records of every type');
		await expect(row).toContainText('masks emails');

		await row.getByRole('button', { name: 'Make a package' }).click();
		const ask = row.getByRole('form', { name: `Make a package of ${name}` });
		const make = ask.getByRole('button', { name: 'Make it' });
		await ask.getByLabel('Passphrase').fill('too short');
		await expect(make).toBeDisabled();
		await ask.getByLabel('Passphrase').fill('correct horse battery');
		await ask.getByLabel('Again').fill('correct horse battery');
		await make.click();
		await expect(ask).toBeHidden();

		const pkg = page.getByRole('region', { name: 'Packages' }).locator('li').filter({ hasText: name }).first();
		// The worker makes it within a few seconds; the page looks again on its own while it waits.
		await expect(pkg.getByRole('button', { name: 'Download' })).toBeVisible({ timeout: 20_000 });
		await expect(pkg).toContainText('rows');
		await pkg.getByText('What it holds').click();
		await expect(pkg.getByRole('cell', { name: 'People' })).toBeVisible();

		const [file] = await Promise.all([page.waitForEvent('download'), pkg.getByRole('button', { name: 'Download' }).click()]);
		expect(file.suggestedFilename()).toMatch(new RegExp(`^servicelater-.*${name}-.*\\.slclone$`));
		expect(fs.readFileSync(await file.path()).subarray(0, 9).toString()).toBe('SLCLONE1\n');
	} finally {
		const list = (await (await admin.context.request.get('/api/v1/clones')).json()) as { packages: { id: string; profile: string }[] };
		for (const p of list.packages.filter((p) => p.profile === name)) await admin.context.request.delete(`/api/v1/clones/packages/${p.id}`);
		await admin.context.request.delete(`/api/v1/clones/profiles/${name}`);
	}
});
