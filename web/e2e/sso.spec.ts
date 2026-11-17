import type { Browser } from '@playwright/test';
import { IDP_ISSUER } from '../playwright.config';
import { as, expect, test, uid } from './helpers';

// A provider the test sets up and removes again: the sign-in page is everyone's, so it must not outlive the test.
async function withProvider(browser: Browser, id: string, groupId: string, run: () => Promise<void>) {
	const { context } = await as(browser, 'admin');
	const put = await context.request.put(`/api/v1/sso-providers/${id}`, {
		data: {
			label: 'Fake IdP', issuer: IDP_ISSUER, client_id: 'servicelater-e2e', client_secret: 'e2e-secret', enabled: true,
			create_users: true, groups_claim: 'groups', group_maps: [{ claim_value: 'desk', group_id: groupId }]
		}
	});
	expect(put.status(), await put.text()).toBe(200);
	try {
		await run();
	} finally {
		await context.request.put('/api/v1/settings/sign-in', { data: { passwords: 'everyone' } });
		await context.request.delete(`/api/v1/sso-providers/${id}`);
	}
}

// Someone new signs in with the organisation's provider: they get an account and the group their provider puts them
// in. A refusal comes back to the sign-in page with the reason. With passwords for admins only, the provider is the way in.
test('signing in with single sign-on', async ({ browser }) => {
	const id = uid();
	const { context: admin } = await as(browser, 'admin');
	const group = await (await admin.request.post('/api/v1/groups', { data: { name: `Desk ${id}` } })).json();

	await withProvider(browser, `fake-${id}`.slice(0, 31).toLowerCase(), group.id, async () => {
		const visitor = await browser.newContext();
		try {
			const page = await visitor.newPage();
			await page.goto('/login');
			await page.getByRole('link', { name: 'Sign in with Fake IdP' }).click();
			await expect(page.getByRole('heading', { name: 'Fake identity provider' })).toBeVisible();
			await page.getByLabel('Email', { exact: true }).fill(`sam-${id}@e2e.test`);
			await page.getByLabel('Name', { exact: true }).fill(`Sam ${id}`);
			await page.getByLabel('Groups').fill('desk, other');
			await page.getByRole('button', { name: 'Sign in' }).click();
			// An end user lands on the catalog, signed in as the new account.
			await expect(page).toHaveURL(/\/catalog$/);
			const me = await (await visitor.request.get('/api/v1/auth/me')).json();
			expect(me.email).toBe(`sam-${id}@e2e.test`);
			const members = await (await admin.request.get(`/api/v1/groups/${group.id}`)).json();
			expect(members.members.map((m: { email: string }) => m.email)).toContain(`sam-${id}@e2e.test`);
		} finally {
			await visitor.close();
		}

		// An unverified email is refused, and the sign-in page says why.
		const stranger = await browser.newContext();
		try {
			const page = await stranger.newPage();
			await page.goto('/login');
			await page.getByRole('link', { name: 'Sign in with Fake IdP' }).click();
			await expect(page.getByRole('heading', { name: 'Fake identity provider' })).toBeVisible();
			await page.getByLabel('Email', { exact: true }).fill(`kim-${id}@e2e.test`);
			await page.getByLabel('Email not verified').check();
			await page.getByRole('button', { name: 'Sign in' }).click();
			await expect(page).toHaveURL(/\/login\?sso_error=/);
			await expect(page.getByRole('alert')).toContainText('has not verified your email address');

			// Passwords for admins only: the provider is the way in, the password form is one click away for admins.
			expect((await admin.request.put('/api/v1/settings/sign-in', { data: { passwords: 'admins' } })).status()).toBe(200);
			await page.goto('/login');
			await expect(page.getByRole('link', { name: 'Sign in with Fake IdP' })).toBeVisible();
			await expect(page.getByLabel('Password')).toHaveCount(0);
			await page.getByRole('button', { name: 'Admins: sign in with a password' }).click();
			await expect(page.getByLabel('Password')).toBeVisible();
		} finally {
			await stranger.close();
		}
	});
});

// The admin page lists providers with the redirect address to register at them.
test('an admin adds a provider on the sign-in page', async ({ browser }) => {
	const id = uid();
	const pid = `p${id}`.toLowerCase().slice(0, 20);
	const { page, context } = await as(browser, 'admin');
	try {
		await page.goto('/admin/sign-in');
		await page.getByRole('button', { name: 'Add a provider' }).click();
		const form = page.getByRole('form', { name: 'New provider' });
		await form.getByLabel('Button label').fill(`Corp ${id}`);
		await form.getByLabel('Short name (in the address)').fill(pid);
		await expect(form).toContainText(`/api/v1/auth/sso/${pid}/callback`);
		await form.getByLabel('Issuer').fill(IDP_ISSUER);
		await form.getByLabel('Client id').fill('servicelater-e2e');
		await form.getByLabel('Client secret').fill('e2e-secret');
		await form.getByLabel('Allowed email domains').fill('e2e.test');
		await form.getByLabel('Offer it on the sign-in page').uncheck();
		await form.getByRole('button', { name: 'Save' }).click();
		const row = page.getByRole('region', { name: 'Single sign-on' }).locator('li', { hasText: `Corp ${id}` });
		await expect(row).toContainText('e2e.test');
		await expect(row).toContainText('Off');
		await row.getByRole('button', { name: `Edit Corp ${id}` }).click();
		await expect(page.getByRole('form', { name: `Edit Corp ${id}` }).getByLabel('Client secret')).toHaveAttribute('placeholder', /Stored/);
	} finally {
		await context.request.delete(`/api/v1/sso-providers/${pid}`);
	}
});
