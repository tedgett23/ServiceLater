import { as, expect, test, uid } from './helpers';

// Admins change the catalog in the UI: an edit to a shipped item is live at once and can be undone; a new item joins
// the catalog. Edits go into the app's studio layer, so every test undoes its own in finally: the database is shared.
test('an admin changes a shipped item, and undoes it', async ({ browser }) => {
	const { page, context } = await as(browser, 'admin');
	try {
		await page.goto('/admin/catalog');
		await page.getByRole('link', { name: 'New laptop' }).click();
		await expect(page.getByRole('heading', { name: 'New laptop', level: 1 })).toBeVisible();
		await page.getByRole('button', { name: 'Add a question' }).click();
		await page.getByLabel('Question', { exact: true }).fill('Do you need a bag?');
		await page.getByLabel('Kind of answer').selectOption('boolean');
		// The preview shows it as people will.
		await expect(page.getByRole('complementary', { name: 'What people see' }).getByLabel('Do you need a bag?')).toBeVisible();
		await page.getByRole('button', { name: 'Save' }).click();
		await expect(page.getByText(/changed here by/)).toBeVisible();
		await page.getByText('YAML').click();
		await expect(page.locator('pre').first()).toContainText('do_you_need_a_bag');

		const lena = await as(browser, 'lena');
		// Publishing commits before the reader's registry receives its NOTIFY. The order page fetches once on mount.
		const hasBagQuestion = async () => {
			const item = await lena.context.request.get('/api/v1/catalog/new-laptop');
			expect(item.ok()).toBe(true);
			return (await item.json()).form.create.includes('do_you_need_a_bag');
		};
		await expect.poll(hasBagQuestion).toBe(true);
		await lena.page.goto('/catalog/new-laptop');
		await expect(lena.page.getByLabel('Do you need a bag?')).toBeVisible();

		await page.getByRole('button', { name: 'Undo the changes made here' }).click();
		await page.getByRole('button', { name: 'Undo changes' }).click();
		await expect(page.getByText(/changed here by/)).toHaveCount(0);
		await expect.poll(hasBagQuestion).toBe(false);
		await lena.page.reload();
		await expect(lena.page.getByLabel('Model')).toBeVisible();
		await expect(lena.page.getByLabel('Do you need a bag?')).toHaveCount(0);
	} finally {
		await context.request.delete('/api/v1/catalog-items/new-laptop/edits');
	}
});

test('an admin adds an item people can order, and the server explains a mistake', async ({ browser }) => {
	const id = uid();
	const name = `desk-lamp-${id}`;
	const { page, context } = await as(browser, 'admin');
	try {
		await page.goto('/admin/catalog/new');
		// Scoped to the editor: the New ticket dialog, always in the page, has fields of the same names.
		const ed = page.getByRole('form', { name: 'New catalog item' });
		await ed.getByLabel('Label').fill(`Desk lamp ${id}`);
		await expect(ed.getByRole('textbox', { name: /^Name/ })).toHaveValue(name);
		await ed.getByRole('combobox', { name: 'Category' }).fill('Office');
		await ed.getByLabel('Summary').fill('A lamp for your desk.');
		await ed.getByRole('button', { name: 'Add a question' }).click();
		await ed.getByLabel('Question', { exact: true }).fill('Which desk?');
		await ed.getByRole('group', { name: 'Required: how' }).getByRole('button', { name: 'Always' }).click();
		// Conditions are Simple or an expression; a mistake in one is listed as an install would list it.
		await ed.getByRole('group', { name: 'Shown: how' }).getByRole('button', { name: 'Expression' }).click();
		await ed.getByLabel('Shown when').fill('record.fields.nope');
		await ed.getByRole('button', { name: 'Save' }).click();
		await expect(ed.getByRole('alert')).toContainText('nope'); // every problem, as an install would list it

		await ed.getByLabel('Shown when').fill('');
		await ed.getByRole('button', { name: 'Save' }).click();
		await expect(page).toHaveURL(new RegExp(`/admin/catalog/${name}$`));

		const lena = await as(browser, 'lena');
		await lena.page.goto('/catalog');
		await lena.page.getByRole('link', { name: new RegExp(`Desk lamp ${id}`) }).click();
		await lena.page.getByLabel('Which desk?').fill('3.14');
		await lena.page.getByRole('button', { name: 'Send request' }).click();
		await expect(lena.page.getByRole('heading', { level: 1 })).toHaveText(`Desk lamp ${id}`);
	} finally {
		await context.request.delete(`/api/v1/catalog-items/${name}/edits`);
	}
});
