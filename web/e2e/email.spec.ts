import { type BrowserContext } from '@playwright/test';
import { as, createTicket, expect, test, uid } from './helpers';
import { mailSink } from './mailsink';
import { USERS } from './users';

async function userId(context: BrowserContext, email: string): Promise<string> {
	const res = await context.request.get(`/api/v1/users?q=${encodeURIComponent(email)}`);
	const items = (await res.json()).items as { id: string; email: string }[];
	return items.find((u) => u.email === email)!.id;
}

// Email is set up in the admin section against a mail catcher on this machine: the password is stored encrypted and
// never shown again, a test email arrives, and so does an assignment notification, unless the person turned email off.
// Email is turned off again afterwards, so no other test sends any.
test('an admin sets up email, and assignments reach people by email unless they turn it off', async ({ browser }) => {
	const sink = await mailSink();
	const admin = await as(browser, 'admin');
	const agent = await as(browser, 'agent');
	const tomas = await as(browser, 'agent2');
	const tomasId = await userId(admin.context, USERS.agent2.email);
	try {
		const { page } = admin;
		await page.goto('/admin');
		await page.getByRole('navigation', { name: 'Admin' }).getByRole('link', { name: 'Email' }).click();
		const form = page.getByRole('form', { name: 'Mail server' });
		await form.getByLabel('Send email notifications').check();
		await form.getByLabel('Host', { exact: true }).fill('127.0.0.1');
		await form.getByLabel('Port', { exact: true }).fill(String(sink.port));
		await form.getByLabel('Encryption').selectOption('none');
		await form.getByLabel('Password', { exact: true }).fill('e2e-relay-secret');
		await form.getByLabel('From address').fill('desk@e2e.test');
		await form.getByLabel('Link address').fill('https://sl.e2e.test');
		await form.getByRole('button', { name: 'Save' }).click();
		await expect(form.getByText(`Stored encrypted, set by ${USERS.admin.name}`)).toBeVisible();
		// Regressed: this checkbox once sat inside the password field and took the field's full-width input style.
		const remove = await form.getByLabel('Remove the stored password').boundingBox();
		expect(remove!.width).toBeLessThan(30);
		// The password field is empty again: it is never sent back.
		await expect(form.getByLabel('Password', { exact: true })).toHaveValue('');
		const settings = await (await admin.context.request.get('/api/v1/settings/email')).json();
		expect(JSON.stringify(settings)).not.toContain('e2e-relay-secret');

		const test = page.getByRole('form', { name: 'Send a test email' });
		await test.getByRole('button', { name: 'Send test email' }).click();
		await expect(test.getByRole('status')).toContainText('Sent. Check the inbox.');
		const sent = await sink.waitFor((m) => m.to.includes(USERS.admin.email));
		expect(sent.data).toContain('Subject: ServiceLater test email');

		// An assignment notification goes by email too, with a link back to the ticket.
		const title = `Badge printer jammed ${uid()}`;
		const t = await createTicket(agent.context, { title });
		const assigned = await agent.context.request.patch(`/api/v1/tickets/${t.id}`, { data: { assignee_id: tomasId } });
		expect(assigned.ok()).toBeTruthy();
		const mail = await sink.waitFor((m) => m.to.includes(USERS.agent2.email) && m.data.includes(title));
		expect(mail.data).toContain(`Subject: ${t.reference} was assigned to you`);
		expect(mail.data).toContain(`https://sl.e2e.test/tickets/${t.reference}`);
		// The admin's delivery log follows as it is sent.
		await expect(page.getByRole('region', { name: 'Recent emails' }).locator('li', { hasText: USERS.agent2.name }).first()).toContainText('Sent');

		// Tomás turns email off from the bell; the next assignment reaches him only in the app.
		await tomas.page.goto('/tickets');
		await tomas.page.getByLabel(/^Notifications/).click();
		await tomas.page.getByRole('link', { name: 'Notification settings' }).click();
		const toggle = tomas.page.getByLabel('Also email me my notifications');
		await expect(toggle).toBeChecked();
		await toggle.uncheck();
		await expect(toggle).not.toBeChecked();
		const quiet = `Quiet one ${uid()}`;
		const t2 = await createTicket(agent.context, { title: quiet });
		await agent.context.request.patch(`/api/v1/tickets/${t2.id}`, { data: { assignee_id: tomasId } });
		const notes = await (await tomas.context.request.get('/api/v1/notifications')).json();
		expect(JSON.stringify(notes.items)).toContain(t2.reference);
		const log = await (await admin.context.request.get('/api/v1/settings/email/deliveries')).json();
		expect(log.items.filter((d: { to_address: string }) => d.to_address === USERS.agent2.email)).toHaveLength(1);
	} finally {
		await admin.context.request.put('/api/v1/settings/email', {
			data: { enabled: false, host: '', port: 587, tls: 'starttls', username: '', from_address: '', from_name: 'ServiceLater', base_url: '', approvals: true, password: null }
		});
		await tomas.context.request.patch('/api/v1/account', { data: { email_notifications: true } });
		await sink.close();
	}
});

test('people who do not manage settings have no email page', async ({ browser }) => {
	const { context, page } = await as(browser, 'agent');
	expect((await context.request.get('/api/v1/settings/email')).status()).toBe(403);
	await page.goto('/admin/email');
	await expect(page).not.toHaveURL(/\/admin\/email/);
});
