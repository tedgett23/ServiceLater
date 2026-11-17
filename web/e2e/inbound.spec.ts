import { IDP_ISSUER } from '../playwright.config';
import { api, as, createTicket, expect, openTicket, test, uid } from './helpers';

// Puts an email in the fake Microsoft 365 inbox (backend/cmd/fakeidp), for the worker to read (SL_MAIL_POLL is 1s here).
async function mail(raw: string) {
	const res = await fetch(`${IDP_ISSUER}/fake/mail`, { method: 'POST', body: raw.replace(/\n/g, '\r\n') });
	expect(res.status).toBe(201);
}

// Email in, set up in Admin → Incoming email with a Microsoft 365 mailbox: a reply to a notification email becomes a
// comment by the person it went to; new mail that its domain does not vouch for (no DMARC pass) is refused, and the
// log says so. Incoming mail is switched off again afterwards.
test('a reply by email becomes a comment, and unauthenticated new mail is refused', async ({ browser }) => {
	const id = uid();
	const admin = await as(browser, 'admin');
	try {
		await admin.page.goto('/admin/incoming');
		const form = admin.page.getByRole('form', { name: 'Incoming email settings' });
		await form.getByLabel('Read this mailbox every minute').check();
		await form.getByLabel('Mailbox address').fill('servicedesk@e2e.test');
		await form.getByRole('button', { name: 'Microsoft 365' }).click();
		await form.getByLabel('Tenant ID').fill('00000000-0000-0000-0000-00000000e2e0');
		await form.getByLabel('Client ID').fill('00000000-0000-0000-0000-0000000000e2');
		await form.getByLabel('Client secret').fill('graph-secret');
		await form.getByLabel('Mailbox to read').fill('servicedesk@e2e.test');
		await form.getByText('National clouds').click();
		await form.getByLabel('Graph address').fill(IDP_ISSUER);
		await form.getByLabel('Sign-in address').fill(IDP_ISSUER);
		await form.getByLabel('File an incident from new email (not a reply)').check();
		await form.getByLabel('Allowed sender domains').fill('e2e.test');
		await form.getByRole('button', { name: 'Save' }).click();
		await expect(form.getByLabel('Client secret')).toHaveAttribute('placeholder', /Stored/);
		await form.getByRole('button', { name: 'Test the connection' }).click();
		await expect(form.getByRole('status')).toContainText('Connected.');

		// Tomás is assigned Lena's ticket, so he has a notification; his email client answers it.
		const lena = await as(browser, 'lena');
		const tomas = await as(browser, 'agent2');
		const me = await (await tomas.context.request.get('/api/v1/auth/me')).json();
		const t = await createTicket(lena.context, { title: `Scanner offline ${id}` });
		await api(admin.context, 'patch', `/api/v1/tickets/${t.id}`, { assignee_id: me.id });
		let notification = '';
		await expect(async () => {
			const list = await (await tomas.context.request.get('/api/v1/notifications')).json();
			notification = list.items.find((n: { link: string }) => n.link.endsWith(t.reference))?.id ?? '';
			expect(notification).not.toBe('');
		}).toPass();

		await mail(`From: Tomás Ibarra <tomas@e2e.test>
To: servicedesk@e2e.test
Subject: Re: ${t.reference} was assigned to you
Message-ID: <reply-${id}@mail.e2e.test>
In-Reply-To: <n.${notification}@servicelater>
Content-Type: text/plain; charset=utf-8

Rebooted it, scanning again ${id}.

On Mon, 17 Nov 2026, ServiceLater wrote:
> ${t.reference} was assigned to you
`);
		await mail(`From: Marcus Webb <marcus@e2e.test>
To: servicedesk@e2e.test
Subject: Unsigned request ${id}
Message-ID: <new-${id}@mail.e2e.test>
Content-Type: text/plain; charset=utf-8

Please reset my password.
`);

		const log = admin.page.getByRole('region', { name: 'Received' });
		await expect(async () => {
			await admin.page.reload();
			await expect(log.locator('tr', { hasText: `Re: ${t.reference}` })).toContainText('Comment', { timeout: 1000 });
		}).toPass({ timeout: 20_000 });
		await expect(log.locator('tr', { hasText: `Re: ${t.reference}` }).getByRole('link', { name: t.reference })).toBeVisible();
		await expect(log.locator('tr', { hasText: `Unsigned request ${id}` })).toContainText(/Refused.*does not vouch for it/s);

		// The quoted message is left out; the comment is Tomás's.
		await openTicket(lena.page, t.reference);
		const comment = lena.page.locator('.activity li', { hasText: `scanning again ${id}` });
		await expect(comment).toContainText('Tomás Ibarra');
		await expect(comment).not.toContainText('was assigned to you');
	} finally {
		await admin.context.request.put('/api/v1/settings/inbound', {
			data: { enabled: false, mode: 'imap', max_per_hour: 10, allowed_domains: [], graph_secret: null }
		});
	}
});
