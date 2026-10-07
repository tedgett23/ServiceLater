import { expect, test } from '@playwright/test';
import { as, createTicket, uid } from './helpers';
import { USERS } from './users';

// core-itsm's ticket-viewer role, granted and revoked from the admin section. Marcus is an end user; the grant must
// be revoked whatever happens, because later tests rely on him seeing only his own tickets.
test('an admin grants a role from the admin section, and it changes what that person can see', async ({ browser }) => {
	const tag = uid();
	const admin = await as(browser, 'admin');
	const lena = await as(browser, 'lena');
	const marcus = await as(browser, 'marcus');
	const t = await createTicket(lena.context, { title: `Lena's monitor ${tag}` });

	expect((await marcus.context.request.get(`/api/v1/tickets/${t.id}`)).status()).toBe(404);

	await admin.page.goto('/admin/roles');
	// Regressed once: the space before "· from core-itsm" was trimmed away.
	await expect(admin.page.getByRole('link', { name: /Ticket viewer/ })).toContainText('auditors. · from core-itsm');
	await admin.page.getByRole('link', { name: /Ticket viewer/ }).click();
	await expect(admin.page.getByRole('heading', { name: 'Ticket viewer' })).toBeVisible();
	await expect(admin.page.locator('.perms')).toContainText('records:read:*');
	try {
		await admin.page.getByLabel('Grant to someone').fill('Marcus');
		await admin.page.getByRole('button', { name: `Grant to ${USERS.marcus.name}` }).click();
		await expect(admin.page.locator('.grants li', { hasText: USERS.marcus.name })).toBeVisible();

		// Marcus now sees everyone's tickets, and the app shows him the queue like staff, but he cannot edit.
		expect((await marcus.context.request.get(`/api/v1/tickets/${t.id}`)).status()).toBe(200);
		await marcus.page.goto(`/tickets/${t.number}`);
		await expect(marcus.page).toHaveTitle(new RegExp(`Lena's monitor ${tag}`));
		await expect(marcus.page.getByRole('link', { name: 'Tickets', exact: true }).first()).toBeVisible();
		expect((await marcus.context.request.patch(`/api/v1/tickets/${t.id}`, { data: { priority: 1 } })).status()).toBe(403);

		// His own page lists it; revoking it there takes it away.
		const me = (await (await marcus.context.request.get('/api/v1/auth/me')).json()) as { id: string; permissions: string[] };
		expect(me.permissions).toContain('records:read:*');
		await admin.page.goto(`/admin/users/${me.id}`);
		await admin.page.getByRole('button', { name: 'Revoke Ticket viewer' }).click();
		await expect(admin.page.getByText('No other roles.')).toBeVisible();
		expect((await marcus.context.request.get(`/api/v1/tickets/${t.id}`)).status()).toBe(404);
	} finally {
		const role = await (await admin.context.request.get('/api/v1/roles/ticket-viewer')).json();
		for (const g of role.granted_to as { id: string }[]) {
			await admin.context.request.delete(`/api/v1/roles/ticket-viewer/grants/${g.id}`);
		}
	}
});

test('on a phone the roles pages fit', async ({ browser }) => {
	const admin = await as(browser, 'admin', { viewport: { width: 390, height: 844 } });
	for (const path of ['/admin/roles', '/admin/roles/ticket-viewer', '/admin/roles/agent']) {
		await admin.page.goto(path);
		await admin.page.waitForLoadState('networkidle');
		const overflow = await admin.page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
		expect(overflow, path).toBeLessThanOrEqual(0);
	}
});
