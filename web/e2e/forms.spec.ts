import { expect, test } from '@playwright/test';
import { installFirstParty, writeLocalOverride } from './apps';
import { as, details, uid } from './helpers';

// incident-management's form: category and subcategory sort an incident, and impact and urgency work out its
// priority (a before rule), which people then cannot set by hand. The server decides all of it; the page only shows
// what it says.
test.describe('Forms', () => {
	test('filing an incident: the subcategory follows the category, and impact and urgency set the priority', async ({ browser }) => {
		const { page } = await as(browser, 'agent');
		const title = `Wi-Fi drops in the east wing ${uid()}`;
		await page.goto('/tickets');
		await page.getByRole('button', { name: 'New ticket' }).click();
		const dialog = page.getByRole('dialog', { name: 'New ticket' });
		await dialog.getByLabel('Summary').fill(title);

		// No subcategory until there is a category, then only that category's.
		await expect(dialog.getByLabel('Subcategory')).toHaveCount(0);
		await dialog.getByLabel('Category').selectOption('network');
		const sub = dialog.getByLabel('Subcategory');
		await expect(sub.locator('option')).toHaveText(['Not set', 'Wi-Fi', 'VPN', 'Wired network']);
		await sub.selectOption('wifi');

		// Impact and urgency lock the priority, and the form shows what it will be.
		await dialog.getByLabel('Impact').selectOption({ label: 'A team' });
		await dialog.getByLabel('Urgency').selectOption({ label: 'Work has stopped' });
		await expect(dialog.getByLabel('Priority')).toBeDisabled();
		await expect(dialog.getByLabel('Priority')).toHaveValue('2');
		await dialog.getByRole('button', { name: 'Create ticket' }).click();

		await expect(page.getByRole('heading', { level: 1 })).toHaveText(title);
		const classification = page.getByRole('region', { name: 'Classification' });
		await expect(classification.getByLabel('Subcategory')).toHaveValue('wifi');
		const priority = details(page).getByLabel('Priority');
		await expect(priority).toHaveValue('2');
		await expect(priority).toBeDisabled();
		await expect(details(page).getByText('Worked out from impact and urgency')).toBeVisible();

		// Lowering the urgency on the record works the priority out again; clearing impact gives it back to people.
		await classification.getByLabel('Urgency').selectOption({ label: 'It can wait' });
		await expect(priority).toHaveValue('4');
		await classification.getByLabel('Impact').selectOption('');
		await expect(priority).toBeEnabled();
		await priority.selectOption('Critical');
		await expect(priority).toHaveValue('1');
	});

	test('the requester reads the fields they filled in, and cannot change them', async ({ browser }) => {
		const { page } = await as(browser, 'lena');
		const title = `Laptop will not charge ${uid()}`;
		await page.goto('/tickets');
		await page.getByRole('button', { name: 'New ticket' }).click();
		const dialog = page.getByRole('dialog', { name: 'New ticket' });
		await dialog.getByLabel('Summary').fill(title);
		await dialog.getByLabel('Category').selectOption('hardware');
		await dialog.getByLabel('Subcategory').selectOption('computer');
		await dialog.getByRole('button', { name: 'Create ticket' }).click();

		await expect(page.getByRole('heading', { level: 1 })).toHaveText(title);
		const classification = page.getByRole('region', { name: 'Classification' });
		await expect(classification).toContainText('Hardware');
		await expect(classification).toContainText('Laptop or desktop');
		await expect(classification.getByRole('combobox')).toHaveCount(0);
		// Fields with nothing in them are left out of what they read.
		await expect(classification).not.toContainText('Impact');
	});

	// An operator customises the form in the local layer, without touching what the app ships: a required field of
	// their own, on the form for filing and on the record. Removing the override puts the shipped form back.
	test('a local override adds a required field, which the form and the server both enforce', async ({ browser }) => {
		const { context, page } = await as(browser, 'agent');
		const remove = writeLocalOverride(
			'incident-management',
			'forms/incident.yaml',
			`fields:
  asset_tag:
    type: text
    label: Asset tag
    required: true
    max_length: 12
layout:
  create: [title, description, asset_tag, category, subcategory, impact, urgency, priority]
  detail:
    - section: Classification
      fields: [asset_tag, category, subcategory, impact, urgency]
    - fields: [resolution]
`
		);
		try {
			installFirstParty();
			const refused = await context.request.post('/api/v1/tickets', { data: { title: `No tag ${uid()}` } });
			expect(refused.status()).toBe(422);
			expect(await refused.text()).toContain('Asset tag is required');

			const title = `Monitor flickers ${uid()}`;
			await page.goto('/tickets');
			await page.getByRole('button', { name: 'New ticket' }).click();
			const dialog = page.getByRole('dialog', { name: 'New ticket' });
			await dialog.getByLabel('Summary').fill(title);
			const tag = dialog.getByLabel('Asset tag');
			await expect(tag).toHaveAttribute('required', '');
			await tag.fill('LT-0042');
			await dialog.getByRole('button', { name: 'Create ticket' }).click();
			await expect(page.getByRole('heading', { level: 1 })).toHaveText(title);
			await expect(page.getByRole('region', { name: 'Classification' }).getByLabel('Asset tag')).toHaveValue('LT-0042');
		} finally {
			remove();
			installFirstParty();
		}
		const allowed = await context.request.post('/api/v1/tickets', { data: { title: `Untagged again ${uid()}` } });
		expect(allowed.status()).toBe(201);
	});
});
