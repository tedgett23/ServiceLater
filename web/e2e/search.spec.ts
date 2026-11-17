import { api, as, createTicket, expect, test, uid } from './helpers';

// Global search: Ctrl+K or the Search button at the left. It waits for three characters, reads them as a reference
// first (INC, INC42) and otherwise searches titles, typos allowed.
test.describe('global search', () => {
	test('a reference typed after Ctrl+K jumps straight to the record', async ({ browser }) => {
		const { context, page } = await as(browser, 'agent');
		const t = await createTicket(context, { title: `Jump target ${uid()}` });
		await page.goto('/kb'); // from anywhere, not only the queue
		await expect(page.getByRole('heading', { name: 'Knowledge' })).toBeVisible(); // the app is up and listening

		await page.keyboard.press('Control+k');
		const box = page.getByRole('combobox', { name: 'Search everything' });
		await expect(box).toBeFocused();
		const results = page.getByRole('listbox', { name: 'Results' });

		// Two characters search nothing yet.
		await box.fill('IN');
		await expect(page.getByText('Type at least three characters')).toBeVisible();
		await expect(results).toHaveCount(0);

		// The prefix alone offers the latest of that type; the number narrows it to the one.
		await box.fill('inc');
		await expect(page.getByText('Latest INC records')).toBeVisible();
		await expect(results.getByRole('option').first()).toContainText('INC');
		await box.fill(t.reference.toLowerCase());
		await expect(results.getByRole('option').first()).toContainText(t.title);
		await expect(results.getByRole('option').first()).toContainText(t.reference);

		// Regressed: Enter pressed while the search for the last keystroke was still running did nothing. It now waits
		// for that search and opens its first result.
		await page.keyboard.press('Enter');
		await expect(page).toHaveURL(new RegExp(`/tickets/${t.reference}$`));
		await expect(page.getByRole('heading', { level: 1 })).toHaveText(t.title);
		await expect(page.getByRole('dialog', { name: 'Search' })).toBeHidden();
	});

	test('words find a record even with a typo, and only what the person may see', async ({ browser }) => {
		const agent = await as(browser, 'agent');
		const tag = uid();
		const t = await createTicket(agent.context, { title: `Quokka laminator ${tag} overheats` });

		await agent.page.goto('/tickets');
		await agent.page.getByRole('button', { name: 'Search everything' }).click();
		const box = agent.page.getByRole('combobox', { name: 'Search everything' });
		await box.fill(`quoka laminater ${tag}`);
		const hit = agent.page.getByRole('option', { name: new RegExp(t.title) });
		await expect(hit).toBeVisible();
		await hit.click();
		await expect(agent.page).toHaveURL(new RegExp(`/tickets/${t.reference}$`));

		// Lena did not file it, so for her it does not exist.
		const lena = await as(browser, 'lena');
		await lena.page.goto('/catalog');
		await expect(lena.page.getByRole('heading', { name: 'How can we help?' })).toBeVisible();
		await lena.page.keyboard.press('Control+k');
		await lena.page.getByRole('combobox', { name: 'Search everything' }).fill(`Quokka laminator ${tag}`);
		await expect(lena.page.getByText('Nothing matches')).toBeVisible();
		await lena.page.keyboard.press('Escape');
		await expect(lena.page.getByRole('dialog', { name: 'Search' })).toBeHidden();
	});

	test('a value inside a field finds its record, and says where it matched', async ({ browser }) => {
		const { page, context } = await as(browser, 'agent');
		const code = `BKP${uid().toUpperCase()}`;
		const prb = await createTicket(context, { type: 'problem', title: `VPN drops for remote staff ${uid()}` });
		await api(context, 'patch', `/api/v1/tickets/${prb.id}`, { fields: { workaround: `Connect with profile ${code} until the fix ships` } });

		await page.goto('/tickets');
		await page.getByRole('button', { name: 'Search everything' }).click();
		await page.getByRole('combobox', { name: 'Search everything' }).fill(code.slice(3));
		const hit = page.getByRole('option', { name: new RegExp(prb.title) });
		await expect(hit).toContainText(`Workaround: Connect with profile ${code} until the fix ships`);
		await hit.click();
		await expect(page).toHaveURL(new RegExp(`/tickets/${prb.reference}$`));
	});
});
