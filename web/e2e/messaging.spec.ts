import { IDP_ISSUER } from '../playwright.config';
import { api, as, createTicket, expect, test, uid } from './helpers';

// A made-up Twilio account id, assembled so that secret scanners do not take it for a real one.
const TEST_SID = 'A' + 'C0123456789abcdef0123456789abcdef';

type Sent = { channel: string; to: string; body: string };
// What the fake Twilio and Teams (backend/cmd/fakeidp) were sent.
async function sent(channel: string, to: string): Promise<Sent[]> {
	return (await fetch(`${IDP_ISSUER}/fake/sent?channel=${channel}&to=${encodeURIComponent(to)}`)).json();
}

// SMS through Twilio (here a fake one) and posts to a group's Teams channel, set up in Admin → Messaging; people
// confirm their own number with a texted code. Everything is switched off again afterwards: other tests file tickets.
test('text messages and Teams posts are set up, confirmed and sent', async ({ browser }) => {
	const id = uid();
	const admin = await as(browser, 'admin');
	try {
		await admin.page.goto('/admin/messaging');
		const smsForm = admin.page.getByRole('form', { name: 'Text messages (Twilio)' });
		await smsForm.getByLabel('Account SID').fill(TEST_SID);
		await smsForm.getByLabel('Auth token').fill('e2e-token');
		await smsForm.getByLabel('Send from').fill('+15550100000');
		await smsForm.getByLabel('API address').fill(IDP_ISSUER);
		await smsForm.getByLabel('Text notifications to people who turned it on').check();
		await smsForm.getByRole('button', { name: 'Save' }).click();
		await expect(smsForm.getByLabel('Auth token')).toHaveAttribute('placeholder', /Stored/);

		const testNumber = `+1555011${String(Date.now()).slice(-4)}`;
		const testForm = admin.page.getByRole('form', { name: 'Send a test text' });
		await testForm.getByLabel('To').fill(testNumber);
		await testForm.getByRole('button', { name: 'Send test' }).click();
		await expect(testForm.getByRole('status')).toHaveText('Sent.');
		expect((await sent('sms', testNumber))[0]?.body).toContain('SMS works');

		// An agent confirms their number with the code texted to it.
		const agent = await as(browser, 'agent');
		const number = `+1555012${String(Date.now()).slice(-4)}`;
		await agent.page.goto('/account');
		const section = agent.page.getByRole('region', { name: 'Text messages' });
		await section.getByLabel('Mobile number').fill(number);
		await section.getByRole('button', { name: 'Text me a code' }).click();
		await expect(section.getByLabel(/Code texted to/)).toBeVisible();
		const code = /\b(\d{6})\b/.exec((await sent('sms', number)).at(-1)?.body ?? '')?.[1] ?? '';
		expect(code).toMatch(/^\d{6}$/);
		await section.getByLabel(/Code texted to/).fill(code);
		await section.getByRole('button', { name: 'Confirm' }).click();
		await expect(section.getByLabel(`Text me urgent notifications at ${number}`)).toBeChecked();

		// A group's Teams channel: connected (a test card is posted), then told when a ticket is routed to it.
		const group = await api(admin.context, 'post', '/api/v1/groups', { name: `Desk ${id}` });
		await admin.page.reload();
		const teams = admin.page.getByRole('region', { name: 'Microsoft Teams' });
		await teams.getByLabel('Post notifications for a group in its Teams channel').check();
		const row = teams.locator('li', { hasText: `Desk ${id}` });
		await row.getByRole('button', { name: 'Connect a channel' }).click();
		await row.getByLabel(`Webhook address for Desk ${id}`).fill(`${IDP_ISSUER}/teams/${id}`);
		await row.getByRole('button', { name: 'Connect' }).click();
		await expect(row).toContainText('Connected');
		expect((await sent('teams', id))[0]?.body).toContain('ServiceLater is connected');

		const t = await createTicket(agent.context, { title: `Printer on fire ${id}` });
		await api(agent.context, 'patch', `/api/v1/tickets/${t.id}`, { assignment_group_id: group.id });
		await expect.poll(async () => (await sent('teams', id)).length, { timeout: 10_000 }).toBe(2);
		const card = (await sent('teams', id))[1].body;
		expect(card).toContain(`${t.reference} is waiting for Desk ${id}`);
		expect(card).toContain(`Printer on fire ${id}`);

		await admin.page.reload();
		await expect(admin.page.getByRole('region', { name: 'Recent messages' })).toContainText(`Desk ${id}`);
	} finally {
		await admin.context.request.put('/api/v1/settings/sms', {
			data: { enabled: false, account_sid: TEST_SID, from: '+15550100000', api_base: IDP_ISSUER }
		});
		await admin.context.request.put('/api/v1/settings/teams', { data: { enabled: false } });
	}
});
