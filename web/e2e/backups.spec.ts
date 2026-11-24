import fs from 'node:fs';
import { as, expect, test } from './helpers';

// Admin → Instance and Admin → Backups: naming the instance and marking it UAT shows the banner on every page; a
// snapshot taken by hand is taken by the worker and can be downloaded; the restore form asks for the instance's name
// before it can be sent. It never restores (the database is shared with every other test; restoring is covered by
// internal/api/backups_test.go), and it deletes its snapshot and puts the instance back in finally.
test('an admin names the instance, takes a snapshot and downloads it', async ({ browser }) => {
	const admin = await as(browser, 'admin');
	const { page } = admin;
	const before = await (await admin.context.request.get('/api/v1/settings/instance')).json();
	let snapshotID = '';
	try {
		await page.goto('/admin/instance');
		await page.getByLabel('Name').fill('Acme UAT');
		await page.getByLabel('Environment').selectOption('uat');
		await page.getByRole('button', { name: 'Save' }).click();
		const banner = page.getByRole('note').filter({ hasText: 'UAT' });
		await expect(banner).toContainText('UAT · Acme UAT');
		await page.goto('/tickets');
		await expect(banner).toBeVisible();

		await page.goto('/admin/backups');
		await expect(page.getByRole('heading', { level: 1, name: 'Backups' })).toBeVisible();
		await page.getByRole('button', { name: 'Take a snapshot now' }).click();
		const snaps = page.getByRole('region', { name: 'Snapshots' });
		const row = snaps.locator('li').first();
		await expect(row).toContainText('Taken by hand');
		// The worker takes it within a few seconds; the page looks again on its own while it waits.
		await expect(row.getByRole('button', { name: 'Download' })).toBeVisible({ timeout: 20_000 });
		await expect(row).toContainText('rows');

		const [file] = await Promise.all([page.waitForEvent('download'), row.getByRole('button', { name: 'Download' }).click()]);
		expect(file.suggestedFilename()).toMatch(/^servicelater-acme-uat-.*\.slsnap$/);
		const saved = await file.path();
		expect(fs.readFileSync(saved).subarray(0, 8).toString()).toBe('SLSNAP1\n');

		await row.getByRole('button', { name: /^Restore the snapshot/ }).click();
		const form = row.getByRole('form', { name: 'Restore this snapshot' });
		await expect(form).toContainText('Everything in Acme UAT is replaced');
		const go = form.getByRole('button', { name: 'Restore it' });
		await expect(go).toBeDisabled();
		await form.getByLabel(/to confirm/).fill('Acme UA');
		await expect(go).toBeDisabled();
		await form.getByLabel(/to confirm/).fill('Acme UAT');
		await expect(go).toBeEnabled();
		await form.getByRole('button', { name: 'Cancel' }).click();

		const list = (await (await admin.context.request.get('/api/v1/backups')).json()) as { snapshots: { id: string; kind: string }[] };
		snapshotID = list.snapshots.find((s) => s.kind === 'manual')?.id ?? '';
	} finally {
		if (snapshotID) await admin.context.request.delete(`/api/v1/backups/${snapshotID}`);
		await admin.context.request.put('/api/v1/settings/instance', { data: before });
	}
});
