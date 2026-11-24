import path from 'node:path';
import { servicelater, tempApp } from './apps';
import { as, expect, test, uid } from './helpers';

// Admin → Apps: an admin uploads a packed app, reads what installing it does (what it adds, and the permissions it
// asks for, which must be approved), installs it, then removes it after typing its name. First-party apps cannot be
// removed here. In finally the app is removed from the command line if the test stopped half way.
test('an admin installs an app from its .swapp file and removes it again', async ({ browser }) => {
	const name = `e2e-tools-${uid()}`;
	const app = tempApp(name, {
		'default/roles/helper.yaml': 'kind: role\nname: ' + name + '-helper\nlabel: Helper\ndescription: "Helps."\npermissions: [users:read]\n'
	});
	try {
		servicelater('pack', '-o', app.dir, app.dir);
		const archive = path.join(app.dir, `${name}-1.0.0.swapp`);
		const { page } = await as(browser, 'admin');
		await page.goto('/admin');
		await page.getByRole('navigation', { name: 'Admin' }).getByRole('link', { name: 'Apps' }).click();
		await expect(page.getByRole('heading', { name: 'Apps', level: 1 })).toBeVisible();

		const installed = page.getByRole('region', { name: 'Installed' });
		const core = installed.locator('li', { hasText: 'core-itsm' }).first();
		await expect(core).toContainText('Comes with ServiceLater');
		await expect(core.getByRole('button', { name: /Remove/ })).toHaveCount(0);

		await page.getByLabel('Upload an app').setInputFiles(archive);
		const plan = page.getByRole('region', { name: 'What installing does' });
		await expect(plan.getByRole('heading')).toContainText(`${name} 1.0.0, new`);
		await expect(plan).toContainText(`role ${name}-helper`);
		await expect(plan.getByRole('note')).toContainText('records:read:*');
		const install = plan.getByRole('button', { name: 'Install' });
		await expect(install).toBeDisabled();
		await plan.getByLabel('Approve these permissions').check();
		await install.click();
		await expect(plan).toHaveCount(0);

		const row = installed.locator('li', { hasText: name });
		await expect(row).toContainText('1.0.0');
		await expect(row).toContainText('installed by ada@e2e.test');

		await row.getByRole('button', { name: `Remove ${name}` }).click();
		const confirm = row.getByRole('form', { name: `Remove ${name}` });
		const go = confirm.getByRole('button', { name: `Remove ${name}` });
		await expect(go).toBeDisabled();
		await confirm.getByLabel('to confirm').fill(name);
		await go.click();
		await expect(installed.locator('li', { hasText: name })).toHaveCount(0);
	} finally {
		try {
			servicelater('uninstall', name);
		} catch {
			/* already removed */
		}
		app.cleanup();
	}
});
