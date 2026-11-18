import { api, as, expect, openTicket, test, uid } from './helpers';

// Admin → Forms: Tasks have the built-in form; an admin adds fields (one shown only for hardware, built in Simple mode),
// puts them in two columns and on the New ticket dialog, and saves. The server then enforces the form: the asset tag
// shows on a hardware task's record and not on a software one's. The form is put back in finally: tests share Tasks.
test('an admin builds the Task form, and records follow it', async ({ browser }) => {
	const id = uid();
	const admin = await as(browser, 'admin');
	const { page } = admin;
	try {
		await page.goto('/admin/forms');
		await page.getByRole('link', { name: 'Task', exact: true }).click();
		await expect(page.getByRole('heading', { name: 'Task form' })).toBeVisible();
		const section = page.getByRole('region', { name: 'Section 1' });

		// A choice field.
		await section.getByLabel('Add a field to section 1').selectOption('+new');
		let settings = page.getByRole('region', { name: /^Settings for/ });
		await settings.getByLabel('Label').fill('Category');
		await settings.getByLabel('Type').selectOption('choice');
		await settings.getByLabel('Choice 1', { exact: true }).fill('Hardware');
		await settings.getByRole('button', { name: 'Add a choice' }).click();
		await settings.getByLabel('Choice 2', { exact: true }).fill('Software');

		// A text field shown only for hardware: Simple mode starts from the first field, Category is Hardware.
		await section.getByLabel('Add a field to section 1').selectOption('+new');
		settings = page.getByRole('region', { name: /^Settings for/ });
		await settings.getByLabel('Label').fill('Hardware asset tag');
		await settings.getByRole('group', { name: 'Shown: how' }).getByRole('button', { name: 'Simple' }).click();
		await expect(settings.locator('code')).toHaveText('has(record.fields.category) && record.fields.category == "Hardware"');

		// A date, then the section in two columns under a heading.
		await section.getByLabel('Add a field to section 1').selectOption('+new');
		settings = page.getByRole('region', { name: /^Settings for/ });
		await settings.getByLabel('Label').fill('Due date');
		await settings.getByLabel('Type').selectOption('date');
		await section.getByRole('group', { name: 'Columns of section 1' }).getByRole('button', { name: '2' }).click();
		await section.getByLabel('Heading of section 1').fill('Work');
		await expect(section.getByRole('list', { name: 'Column 1' })).toContainText('Category');
		await expect(section.getByRole('list', { name: 'Column 2' })).toContainText('Due date');
		// Dragging a field onto another puts it there: Due date goes above Category, in column 1.
		const rowOf = (label: string) => section.locator('li.frow').filter({ has: page.getByRole('button', { name: label, exact: true }) });
		await rowOf('Due date').dragTo(rowOf('Category'));
		await expect(section.getByRole('list', { name: 'Column 1' }).locator('li.frow').first()).toContainText('Due date');

		// The preview follows the condition as it is filled in.
		const preview = page.getByRole('complementary', { name: 'Preview' });
		await expect(preview.getByLabel('Hardware asset tag')).toBeHidden();
		await preview.getByLabel('Category').selectOption('Hardware');
		await expect(preview.getByLabel('Hardware asset tag')).toBeVisible();

		// Category is asked when filing one too.
		await page.getByRole('group', { name: 'Layout' }).getByRole('button', { name: 'New ticket dialog' }).click();
		await page.getByRole('region', { name: 'Section 1' }).getByLabel('Add a field to section 1').selectOption({ label: 'Category' });

		await page.getByRole('button', { name: 'Save' }).click();
		await expect(page.getByText('Unsaved changes')).toBeHidden();
		await expect(page.getByRole('button', { name: 'Undo the changes made here' })).toBeVisible();

		// Records follow it: the server decides the asset tag shows for hardware only.
		const agent = await as(browser, 'agent');
		const hw = await api(agent.context, 'post', '/api/v1/tickets', { type: 'work_task', title: `Swap a disk ${id}`, fields: { category: 'Hardware' } });
		const sw = await api(agent.context, 'post', '/api/v1/tickets', { type: 'work_task', title: `Install an app ${id}`, fields: { category: 'Software' } });
		const work = agent.page.getByRole('region', { name: 'Work' });
		await openTicket(agent.page, hw.reference);
		await expect(work.getByLabel('Hardware asset tag')).toBeVisible();
		await expect(work.getByLabel('Due date')).toBeVisible();
		await openTicket(agent.page, sw.reference);
		await expect(work.getByLabel('Category')).toHaveValue('Software');
		await expect(work.getByLabel('Hardware asset tag')).toBeHidden();
	} finally {
		await admin.context.request.delete('/api/v1/forms/work_task/edits');
	}
});
